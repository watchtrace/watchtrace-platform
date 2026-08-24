package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/incident"
	"github.com/watchtrace/watchtrace-platform/internal/notification"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
)

type incidentFixture struct {
	organizationID string
	environmentID  string
	monitorID      string
	ownerID        string
	memberID       string
	viewerID       string
	crossUserID    string
	base           time.Time
}

func TestIncidentLifecycleRecipientsAndAuthorizedActions(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	fixture := setupIncidentFixture(t, ctx, pool, "incident-lifecycle")

	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base, false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(time.Minute), false, true)
	if countRows(t, ctx, pool, `SELECT count(*) FROM incidents WHERE monitor_id=$1::uuid`, fixture.monitorID) != 0 {
		t.Fatal("an incident opened before the third failure")
	}
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(2*time.Minute), false, true)

	incidentID := oneIncidentID(t, ctx, pool, fixture.monitorID, "open")
	var startedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT started_at FROM incidents WHERE id=$1::uuid`, incidentID).Scan(&startedAt); err != nil {
		t.Fatal(err)
	}
	if !startedAt.Equal(fixture.base) {
		t.Fatalf("incident started_at=%s want=%s", startedAt, fixture.base)
	}
	assertOutboxRecipients(t, ctx, pool, incidentID, []string{"owner-incident-lifecycle@example.test"})
	var eventID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM incident_events WHERE incident_id=$1::uuid AND event_type='opened'`, incidentID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	var crossOrganizationID string
	if err := pool.QueryRow(ctx, `SELECT organization_id::text FROM org_members WHERE user_id=$1::uuid`, fixture.crossUserID).Scan(&crossOrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notification_outbox(
 organization_id,incident_id,incident_event_id,recipient_user_id,recipient_email,transition)
VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'cross@example.test','opened')`,
		crossOrganizationID, incidentID, eventID, fixture.crossUserID); err == nil {
		t.Fatal("notification outbox accepted a cross-organization incident event")
	}

	service := incident.NewService(pool)
	if _, err := service.Acknowledge(ctx, fixture.viewerID, fixture.environmentID, incidentID, "viewer"); !errors.Is(err, incident.ErrForbidden) {
		t.Fatalf("viewer acknowledge error=%v", err)
	}
	if _, err := service.Acknowledge(ctx, fixture.crossUserID, fixture.environmentID, incidentID, "cross tenant"); !errors.Is(err, incident.ErrIncidentNotFound) {
		t.Fatalf("cross-tenant acknowledge error=%v", err)
	}
	acknowledged, err := service.Acknowledge(ctx, fixture.memberID, fixture.environmentID, incidentID, "investigating")
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Status != "open" || acknowledged.AcknowledgedAt == nil || acknowledged.AcknowledgedByUserID == nil || *acknowledged.AcknowledgedByUserID != fixture.memberID {
		t.Fatalf("acknowledged incident=%+v", acknowledged)
	}
	if _, err = service.Acknowledge(ctx, fixture.memberID, fixture.environmentID, incidentID, "duplicate"); err != nil {
		t.Fatalf("idempotent acknowledge: %v", err)
	}
	if countRows(t, ctx, pool, `SELECT count(*) FROM incident_events WHERE incident_id=$1::uuid AND event_type='acknowledged'`, incidentID) != 1 {
		t.Fatal("acknowledge created duplicate timeline events")
	}

	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(3*time.Minute), true, true)
	if oneIncidentID(t, ctx, pool, fixture.monitorID, "open") != incidentID {
		t.Fatal("one recovery success resolved the incident")
	}
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(4*time.Minute), true, true)
	resolved := oneIncidentID(t, ctx, pool, fixture.monitorID, "resolved")
	if resolved != incidentID {
		t.Fatalf("resolved incident=%s want=%s", resolved, incidentID)
	}
	assertIncidentResolution(t, ctx, pool, incidentID, "automatic_recovery")
	if countRows(t, ctx, pool, `SELECT count(*) FROM notification_outbox WHERE incident_id=$1::uuid`, incidentID) != 2 {
		t.Fatal("open and recovery did not create exactly two notification transitions")
	}

	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(5*time.Minute), false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(6*time.Minute), false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(7*time.Minute), false, true)
	manualID := oneIncidentID(t, ctx, pool, fixture.monitorID, "open")
	if manualID == incidentID {
		t.Fatal("new failure sequence reused a resolved incident")
	}
	manuallyResolved, err := service.Resolve(ctx, fixture.memberID, fixture.environmentID, manualID, "maintenance confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if manuallyResolved.Status != "resolved" || manuallyResolved.ResolutionKind == nil || *manuallyResolved.ResolutionKind != "manual_resolution" {
		t.Fatalf("manual resolution=%+v", manuallyResolved)
	}
	var monitorObserved string
	if err = pool.QueryRow(ctx, `SELECT observed_state FROM monitor_reliability_states WHERE monitor_id=$1::uuid`, fixture.monitorID).Scan(&monitorObserved); err != nil {
		t.Fatal(err)
	}
	if monitorObserved != "down" {
		t.Fatalf("manual resolution changed monitoring state to %s", monitorObserved)
	}
}

func TestConfiguredIncidentThresholds(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	fixture := setupIncidentFixture(t, ctx, pool, "incident-thresholds")
	if _, err := pool.Exec(ctx, `INSERT INTO alert_rules(
 organization_id,environment_id,monitor_id,failure_threshold,recovery_threshold)
SELECT organization_id,environment_id,id,2,1 FROM monitors WHERE id=$1::uuid`, fixture.monitorID); err != nil {
		t.Fatal(err)
	}
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base, false, true)
	if countRows(t, ctx, pool, `SELECT count(*) FROM incidents WHERE monitor_id=$1::uuid`, fixture.monitorID) != 0 {
		t.Fatal("configured incident opened before two failures")
	}
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(time.Minute), false, true)
	incidentID := oneIncidentID(t, ctx, pool, fixture.monitorID, "open")
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(2*time.Minute), true, true)
	assertIncidentResolution(t, ctx, pool, incidentID, "automatic_recovery")
}

func TestResultConsumerAtomicallyOpensIncidentAndNotification(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	fixture := setupIncidentFixture(t, ctx, pool, "incident-result-consumer")
	resultPublic, resultPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE worker_pools SET result_key_id='incident-result',result_public_key=$1 WHERE id='hosted'`, resultPublic); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		scheduled := fixture.base.Add(time.Duration(index) * time.Minute)
		var jobID string
		hash := bytes.Repeat([]byte{byte(index + 1)}, 32)
		if err = pool.QueryRow(ctx, `INSERT INTO check_jobs(
 organization_id,environment_id,monitor_id,job_type,state,scheduled_at,expires_at,worker_pool_id,snapshot_hash)
VALUES($1::uuid,$2::uuid,$3::uuid,'scheduled','published',$4::timestamptz,$4::timestamptz+INTERVAL '2 minutes','hosted',$5)
RETURNING id::text`, fixture.organizationID, fixture.environmentID, fixture.monitorID, scheduled, hash).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		resultID, attemptID := randomDatabaseUUID(t, ctx, pool), randomDatabaseUUID(t, ctx, pool)
		errorCategory := "timeout"
		body, signErr := envelope.SignResult(envelope.Result{SchemaVersion: 2, ResultID: resultID,
			JobID: jobID, SnapshotHash: fmt.Sprintf("%x", hash), WorkerPoolID: "hosted",
			WorkerID: "incident-worker", AttemptID: attemptID, ScheduledAt: scheduled,
			StartedAt: scheduled, CompletedAt: scheduled.Add(time.Second), Succeeded: false,
			ErrorCategory: &errorCategory, TotalMicros: 1_000_000, ResultKeyID: "incident-result"}, resultPrivate)
		if signErr != nil {
			t.Fatal(signErr)
		}
		source := &capturedResultSource{body: body}
		consumer := fifo.NewResultConsumer(pool, source)
		if worked, consumeErr := consumer.ConsumeNext(ctx); consumeErr != nil || !worked || source.acked != 1 {
			t.Fatalf("result %d worked=%t acknowledged=%d error=%v", index+1, worked, source.acked, consumeErr)
		}
	}
	incidentID := oneIncidentID(t, ctx, pool, fixture.monitorID, "open")
	if countRows(t, ctx, pool, `SELECT count(*) FROM health_checks WHERE monitor_id=$1::uuid`, fixture.monitorID) != 3 ||
		countRows(t, ctx, pool, `SELECT count(*) FROM notification_outbox WHERE incident_id=$1::uuid`, incidentID) != 1 {
		t.Fatal("accepted results, incident, and notification were not committed exactly once")
	}
}

func TestLateIncidentCorrectionAndConcurrentOpenAreIdempotent(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	fixture := setupIncidentFixture(t, ctx, pool, "incident-correction")

	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base, false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(time.Minute), false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(3*time.Minute), false, true)
	openID := oneIncidentID(t, ctx, pool, fixture.monitorID, "open")
	lateJobID := applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(2*time.Minute), true, true)
	assertIncidentResolution(t, ctx, pool, openID, "late_result_correction")

	// Re-evaluating the accepted correction cannot recreate incident events or
	// notification work.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := reliability.EvaluateAcceptedTx(ctx, tx, fixture.monitorID, lateJobID,
		fixture.base.Add(2*time.Minute), fixture.base.Add(4*time.Minute), fixture.base.Add(4*time.Minute+time.Second))
	if err == nil {
		err = incident.ApplyEvaluationTx(ctx, tx, fixture.monitorID, lateJobID, corrected, fixture.base.Add(4*time.Minute+time.Second))
	}
	if err == nil {
		err = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool, `SELECT count(*) FROM incident_events WHERE incident_id=$1::uuid`, openID) != 2 ||
		countRows(t, ctx, pool, `SELECT count(*) FROM notification_outbox WHERE incident_id=$1::uuid`, openID) != 2 {
		t.Fatal("replayed late correction duplicated durable side effects")
	}

	concurrent := setupIncidentFixture(t, ctx, pool, "incident-concurrency")
	lastJobID := applyIncidentObservation(t, ctx, pool, concurrent.monitorID, concurrent.base, false, false)
	lastJobID = applyIncidentObservation(t, ctx, pool, concurrent.monitorID, concurrent.base.Add(time.Minute), false, false)
	lastJobID = applyIncidentObservation(t, ctx, pool, concurrent.monitorID, concurrent.base.Add(2*time.Minute), false, false)

	// Incident and outbox writes disappear together on rollback.
	rollbackTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = incident.ApplyEvaluationTx(ctx, rollbackTx, concurrent.monitorID, lastJobID, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if countRowsTx(t, ctx, rollbackTx, `SELECT count(*) FROM notification_outbox WHERE organization_id=$1::uuid`, concurrent.organizationID) != 1 {
		t.Fatal("incident transition did not enqueue notification in its transaction")
	}
	if err = rollbackTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool, `SELECT count(*) FROM incidents WHERE monitor_id=$1::uuid`, concurrent.monitorID) != 0 {
		t.Fatal("rolled-back incident remained durable")
	}

	const contenders = 12
	errorsByWorker := make(chan error, contenders)
	var wait sync.WaitGroup
	for index := 0; index < contenders; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			workerTx, beginErr := pool.Begin(ctx)
			if beginErr != nil {
				errorsByWorker <- beginErr
				return
			}
			defer workerTx.Rollback(context.Background())
			applyErr := incident.ApplyEvaluationTx(ctx, workerTx, concurrent.monitorID, lastJobID, false, time.Now().UTC())
			if applyErr == nil {
				applyErr = workerTx.Commit(ctx)
			}
			errorsByWorker <- applyErr
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for workerErr := range errorsByWorker {
		if workerErr != nil {
			t.Fatal(workerErr)
		}
	}
	if countRows(t, ctx, pool, `SELECT count(*) FROM incidents WHERE monitor_id=$1::uuid AND status='open'`, concurrent.monitorID) != 1 ||
		countRows(t, ctx, pool, `SELECT count(*) FROM notification_outbox WHERE organization_id=$1::uuid`, concurrent.organizationID) != 1 {
		t.Fatal("concurrent evaluation violated one incident/notification transition")
	}
}

func TestNotificationRetryRestartExhaustionAndConcurrentClaim(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	fixture := setupIncidentFixture(t, ctx, pool, "notification-delivery")
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base, false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(time.Minute), false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(2*time.Minute), false, true)
	incidentID := oneIncidentID(t, ctx, pool, fixture.monitorID, "open")
	deliveryID := oneDeliveryID(t, ctx, pool, incidentID, "opened")

	clock := time.Now().UTC().Add(2 * time.Second).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `UPDATE notification_outbox SET state='leased',lease_owner='crashed-worker',
 lease_token=gen_random_uuid(),lease_expires_at=$2,next_attempt_at=$2
WHERE delivery_id=$1::uuid`, deliveryID, clock.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedNotificationProvider{failures: 3}
	newWorker := func(id string) *notification.Worker {
		worker, err := notification.NewWorker(pool, provider,
			notification.Config{WorkerID: id, LeaseDuration: 30 * time.Second},
			notification.WithClock(func() time.Time { return clock }))
		if err != nil {
			t.Fatal(err)
		}
		return worker
	}
	worker := newWorker("retry-worker-1")
	if worked, err := worker.DeliverNext(ctx); err != nil || !worked {
		t.Fatalf("expired-lease recovery worked=%t error=%v", worked, err)
	}
	assertDeliveryState(t, ctx, pool, deliveryID, "pending", 1, clock.Add(time.Minute))
	if worked, err := newWorker("restarted-too-soon").DeliverNext(ctx); err != nil || worked {
		t.Fatalf("early retry worked=%t error=%v", worked, err)
	}
	for attempt, advance := range []time.Duration{time.Minute, 5 * time.Minute, 25 * time.Minute} {
		clock = clock.Add(advance)
		if worked, err := newWorker(fmt.Sprintf("restarted-%d", attempt+2)).DeliverNext(ctx); err != nil || !worked {
			t.Fatalf("retry %d worked=%t error=%v", attempt+2, worked, err)
		}
	}
	assertDeliveryState(t, ctx, pool, deliveryID, "accepted", 4, time.Time{})
	if countRows(t, ctx, pool, `SELECT count(*) FROM notification_attempts WHERE delivery_id=$1::uuid`, deliveryID) != 4 {
		t.Fatal("retry history does not contain four attempts")
	}
	provider.mu.Lock()
	if len(provider.messages) != 4 || !strings.Contains(provider.messages[0].PlainTextBody, incidentID) || !strings.Contains(provider.messages[0].PlainTextBody, deliveryID) {
		provider.mu.Unlock()
		t.Fatal("notification identity was not stable and visible")
	}
	provider.mu.Unlock()

	// Recovery transition provides a second delivery that exhausts all four
	// attempts and remains operator-visible as failed.
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(3*time.Minute), true, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(4*time.Minute), true, true)
	resolutionDelivery := oneDeliveryID(t, ctx, pool, incidentID, "resolved")
	alwaysFail := &scriptedNotificationProvider{failures: 10}
	failedWorker, err := notification.NewWorker(pool, alwaysFail,
		notification.Config{WorkerID: "failure-worker", LeaseDuration: 30 * time.Second},
		notification.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	for attempt, advance := range []time.Duration{0, time.Minute, 5 * time.Minute, 25 * time.Minute} {
		clock = clock.Add(advance)
		if worked, deliveryErr := failedWorker.DeliverNext(ctx); deliveryErr != nil || !worked {
			t.Fatalf("final-failure attempt %d worked=%t error=%v", attempt+1, worked, deliveryErr)
		}
	}
	assertDeliveryState(t, ctx, pool, resolutionDelivery, "failed", 4, time.Time{})

	// A third incident supplies one fresh outbox row. Concurrent workers lease
	// it with SKIP LOCKED, so only one provider call occurs.
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(5*time.Minute), false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(6*time.Minute), false, true)
	applyIncidentObservation(t, ctx, pool, fixture.monitorID, fixture.base.Add(7*time.Minute), false, true)
	concurrentProvider := &scriptedNotificationProvider{}
	const workers = 10
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			candidate, createErr := notification.NewWorker(pool, concurrentProvider,
				notification.Config{WorkerID: fmt.Sprintf("claim-%d", index), LeaseDuration: 30 * time.Second},
				notification.WithClock(func() time.Time { return clock }))
			if createErr != nil {
				results <- createErr
				return
			}
			_, createErr = candidate.DeliverNext(ctx)
			results <- createErr
		}(index)
	}
	wait.Wait()
	close(results)
	for resultErr := range results {
		if resultErr != nil {
			t.Fatal(resultErr)
		}
	}
	concurrentProvider.mu.Lock()
	messageCount := len(concurrentProvider.messages)
	concurrentProvider.mu.Unlock()
	if messageCount != 1 {
		t.Fatalf("concurrent workers sent %d messages", messageCount)
	}
}

func TestIncidentNotificationSchemaRollback(t *testing.T) {
	if testing.Short() || strings.TrimSpace(os.Getenv("WATCHTRACE_EXPECT_INCIDENT_NOTIFICATION_SCHEMA_ABSENT")) == "" {
		t.Skip("incident/notification rollback check is not enabled")
	}
	ctx, pool := openSchedulerTestPool(t)
	for _, table := range []string{"alert_rules", "incidents", "incident_events", "notification_outbox", "notification_attempts"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("Phase 1.3 table %s survived rollback", table)
		}
	}
	var reliabilityExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.monitor_reliability_states') IS NOT NULL`).Scan(&reliabilityExists); err != nil || !reliabilityExists {
		t.Fatalf("Phase 1.2 schema was not preserved: %v", err)
	}
}

type scriptedNotificationProvider struct {
	mu       sync.Mutex
	failures int
	messages []notification.Message
}

func (provider *scriptedNotificationProvider) Send(_ context.Context, message notification.Message) (notification.ProviderResponse, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.messages = append(provider.messages, message)
	if len(provider.messages) <= provider.failures {
		return notification.ProviderResponse{}, notification.ProviderFailure{Status: "temporary_unavailable"}
	}
	return notification.ProviderResponse{MessageID: "provider-" + message.DeliveryID, Status: "accepted"}, nil
}

func setupIncidentFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) incidentFixture {
	t.Helper()
	deleteSchedulerTestData(t, ctx, pool, []string{slug, slug + "-cross"})
	emails := []string{
		"owner-" + slug + "@example.test",
		"member-" + slug + "@example.test",
		"viewer-" + slug + "@example.test",
		"cross-" + slug + "@example.test",
	}
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email=ANY($1::text[])`, emails)
	t.Cleanup(func() {
		deleteSchedulerTestData(t, context.Background(), pool, []string{slug, slug + "-cross"})
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE email=ANY($1::text[])`, emails)
	})
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slug)
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Incident monitor", 60, time.Now().UTC().Add(time.Hour))
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	insertSchedulePeriod(t, ctx, pool, organizationID, environmentID, monitorID, 1, 60, base, base, base.Add(20*time.Minute))
	ids := make([]string, 4)
	for index, email := range emails {
		verified := index != 2
		var verifiedAt any
		if verified {
			verifiedAt = time.Now().UTC()
		}
		if err := pool.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified_at)
VALUES($1,'test-password-hash',$2) RETURNING id::text`, email, verifiedAt).Scan(&ids[index]); err != nil {
			t.Fatal(err)
		}
	}
	for index, role := range []string{"owner", "member", "viewer"} {
		enabled := index != 1
		if _, err := pool.Exec(ctx, `INSERT INTO org_members(organization_id,user_id,role,incident_notifications_enabled)
VALUES($1::uuid,$2::uuid,$3,$4)`, organizationID, ids[index], role, enabled); err != nil {
			t.Fatal(err)
		}
	}
	crossOrganizationID, _ := insertSchedulerTenant(t, ctx, pool, slug+"-cross")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members(organization_id,user_id,role,incident_notifications_enabled)
VALUES($1::uuid,$2::uuid,'owner',true)`, crossOrganizationID, ids[3]); err != nil {
		t.Fatal(err)
	}
	return incidentFixture{organizationID: organizationID, environmentID: environmentID, monitorID: monitorID,
		ownerID: ids[0], memberID: ids[1], viewerID: ids[2], crossUserID: ids[3], base: base}
}

func applyIncidentObservation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, monitorID string, scheduled time.Time, succeeded, applyIncidents bool) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var organizationID, environmentID, jobID string
	if err = tx.QueryRow(ctx, `SELECT organization_id::text,environment_id::text FROM monitors WHERE id=$1::uuid`, monitorID).Scan(&organizationID, &environmentID); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `INSERT INTO check_jobs(
 organization_id,environment_id,monitor_id,job_type,state,scheduled_at,started_at,completed_at,expires_at)
VALUES($1::uuid,$2::uuid,$3::uuid,'scheduled','completed',$4::timestamptz,$4::timestamptz,$4::timestamptz,$4::timestamptz+INTERVAL '2 minutes') RETURNING id::text`,
		organizationID, environmentID, monitorID, scheduled).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	var status any
	var category any
	if succeeded {
		status = int16(204)
	} else {
		category = "timeout"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO health_checks(
 job_id,organization_id,environment_id,monitor_id,job_type,scheduled_at,started_at,completed_at,
 succeeded,status_code,error_category,total_duration_microseconds)
VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'scheduled',$5,$5,$5,$6,$7,$8,1000)`,
		jobID, organizationID, environmentID, monitorID, scheduled, succeeded, status, category); err != nil {
		t.Fatal(err)
	}
	now := scheduled.Add(30 * time.Second)
	corrected, err := reliability.EvaluateAcceptedTx(ctx, tx, monitorID, jobID, scheduled, scheduled.Add(2*time.Minute), now)
	if err == nil && applyIncidents {
		err = incident.ApplyEvaluationTx(ctx, tx, monitorID, jobID, corrected, now)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func oneIncidentID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, monitorID, status string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM incidents WHERE monitor_id=$1::uuid AND status=$2 ORDER BY opened_at DESC,id DESC LIMIT 1`, monitorID, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func oneDeliveryID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, incidentID, transition string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT delivery_id::text FROM notification_outbox
WHERE incident_id=$1::uuid AND transition=$2 ORDER BY created_at DESC LIMIT 1`, incidentID, transition).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertOutboxRecipients(t *testing.T, ctx context.Context, pool *pgxpool.Pool, incidentID string, want []string) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT recipient_email FROM notification_outbox WHERE incident_id=$1::uuid ORDER BY recipient_email`, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var email string
		if err = rows.Scan(&email); err != nil {
			t.Fatal(err)
		}
		got = append(got, email)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recipients=%v want=%v", got, want)
	}
}

func assertIncidentResolution(t *testing.T, ctx context.Context, pool *pgxpool.Pool, incidentID, kind string) {
	t.Helper()
	var status, gotKind string
	var resolvedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT status,resolution_kind,resolved_at FROM incidents WHERE id=$1::uuid`, incidentID).Scan(&status, &gotKind, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" || gotKind != kind || resolvedAt.IsZero() {
		t.Fatalf("incident status=%s kind=%s resolved=%s", status, gotKind, resolvedAt)
	}
}

func assertDeliveryState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deliveryID, state string, attempts int, next time.Time) {
	t.Helper()
	var gotState string
	var gotAttempts int
	var nextAttempt time.Time
	if err := pool.QueryRow(ctx, `SELECT state,attempt_count,next_attempt_at FROM notification_outbox WHERE delivery_id=$1::uuid`, deliveryID).Scan(&gotState, &gotAttempts, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if gotState != state || gotAttempts != attempts || (!next.IsZero() && !nextAttempt.Equal(next)) {
		t.Fatalf("delivery state=%s attempts=%d next=%s", gotState, gotAttempts, nextAttempt)
	}
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, query, arguments...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countRowsTx(t *testing.T, ctx context.Context, tx pgx.Tx, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := tx.QueryRow(ctx, query, arguments...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
