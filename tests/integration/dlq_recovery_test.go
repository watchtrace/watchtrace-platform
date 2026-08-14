package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type scriptedDLQ struct {
	job, result         fifo.DLQDelivery
	jobSent, resultSent bool
	acknowledged        []string
}

func (s *scriptedDLQ) PullDLQ(_ context.Context, kind string, _ time.Duration) (fifo.DLQDelivery, error) {
	if kind == "job" && !s.jobSent {
		s.jobSent = true
		return s.job, nil
	}
	if kind == "result" && !s.resultSent {
		s.resultSent = true
		return s.result, nil
	}
	return fifo.DLQDelivery{}, workqueue.ErrNoMessage
}
func (s *scriptedDLQ) AcknowledgeDLQ(_ context.Context, delivery fifo.DLQDelivery) error {
	s.acknowledged = append(s.acknowledged, delivery.Kind)
	return nil
}

func TestDLQReconciliationMarksUnknownAndEncryptsRecoverableResult(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slug := "dlq-recovery"
	deleteSchedulerTestData(t, ctx, pool, []string{slug})
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, []string{slug}) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slug)
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "DLQ recovery", 60, time.Now().UTC().Add(-time.Second))
	_, platformPrivate, _ := ed25519.GenerateKey(rand.Reader)
	resultPublic, resultPrivate, _ := ed25519.GenerateKey(rand.Reader)
	workerPrivate, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err := pool.Exec(ctx, `UPDATE worker_pools SET encryption_key_id='dlq-enc',encryption_public_key=$1,result_key_id='dlq-result',result_public_key=$2,job_queue_url='https://sqs.test/jobs.fifo' WHERE id='hosted'`, workerPrivate.PublicKey().Bytes(), resultPublic); err != nil {
		t.Fatal(err)
	}
	headers, _ := secureheaders.New(1, map[int32][]byte{1: make([]byte, 32)})
	scheduler, err := fifo.NewScheduler(pool, platformPrivate, "dlq-platform", headers)
	if err != nil {
		t.Fatal(err)
	}
	if created, scheduleErr := scheduler.ScheduleDue(ctx, 1); scheduleErr != nil || created != 1 {
		t.Fatalf("created=%d error=%v", created, scheduleErr)
	}
	var jobID, snapshotHash string
	var scheduledAt time.Time
	if err = pool.QueryRow(ctx, `SELECT id::text,encode(snapshot_hash,'hex'),scheduled_at FROM check_jobs WHERE monitor_id=$1::uuid`, monitorID).Scan(&jobID, &snapshotHash, &scheduledAt); err != nil {
		t.Fatal(err)
	}
	resultID, attemptID := randomDatabaseUUID(t, ctx, pool), randomDatabaseUUID(t, ctx, pool)
	statusCode := int16(204)
	signed, err := envelope.SignResult(envelope.Result{SchemaVersion: 2, ResultID: resultID, JobID: jobID, SnapshotHash: snapshotHash, WorkerPoolID: "hosted", WorkerID: "dlq-worker", AttemptID: attemptID, ScheduledAt: scheduledAt, StartedAt: scheduledAt, CompletedAt: scheduledAt.Add(time.Second), Succeeded: true, StatusCode: &statusCode, TotalMicros: 1_000_000, ResultKeyID: "dlq-result"}, resultPrivate)
	if err != nil {
		t.Fatal(err)
	}
	sealer, _ := quarantine.New(bytes.Repeat([]byte{9}, 32))
	source := &scriptedDLQ{
		job:    fifo.DLQDelivery{Kind: "job", Attributes: envelope.Attributes{JobID: jobID, WorkerPoolID: "hosted"}, Receipt: "job-receipt"},
		result: fifo.DLQDelivery{Kind: "result", Body: signed, Receipt: "result-receipt"},
	}
	reconciler := fifo.NewDLQReconciler(pool, source, sealer)
	if worked, reconcileErr := reconciler.ReconcileNext(ctx); reconcileErr != nil || !worked {
		t.Fatalf("job reconcile worked=%t error=%v", worked, reconcileErr)
	}
	if worked, reconcileErr := reconciler.ReconcileNext(ctx); reconcileErr != nil || !worked {
		t.Fatalf("result reconcile worked=%t error=%v", worked, reconcileErr)
	}
	var state string
	var gaps int
	if err = pool.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM monitoring_coverage_gaps WHERE monitor_id=$2::uuid AND reason='dead') FROM check_jobs WHERE id=$1::uuid`, jobID, monitorID).Scan(&state, &gaps); err != nil {
		t.Fatal(err)
	}
	if state != "dead" || gaps != 1 {
		t.Fatalf("state=%s gaps=%d", state, gaps)
	}
	var encrypted []byte
	if err = pool.QueryRow(ctx, `SELECT encrypted_payload FROM monitoring_quarantine WHERE result_id=$1::uuid`, resultID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := sealer.Open(encrypted, []byte("result:"+resultID))
	if err != nil || !bytes.Equal(plain, signed) {
		t.Fatalf("quarantine open error=%v", err)
	}
	if len(source.acknowledged) != 2 || source.acknowledged[0] != "job" || source.acknowledged[1] != "result" {
		t.Fatalf("acknowledged=%v", source.acknowledged)
	}
}

func randomDatabaseUUID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
