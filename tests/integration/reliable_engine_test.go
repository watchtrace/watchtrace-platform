package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/checkengine"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/modworker"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
	"github.com/watchtrace/watchtrace-platform/internal/workerjournal"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type capturedSender struct{ input fifo.SendInput }

func (s *capturedSender) Send(_ context.Context, input fifo.SendInput) (string, error) {
	s.input = input
	return "sqs-message", nil
}

type ambiguousSender struct {
	inputs    []fifo.SendInput
	failFirst bool
}

func (s *ambiguousSender) Send(_ context.Context, input fifo.SendInput) (string, error) {
	input.Body = append([]byte(nil), input.Body...)
	s.inputs = append(s.inputs, input)
	if s.failFirst && len(s.inputs) == 1 {
		return "", errors.New("accepted response lost")
	}
	return "confirmed-message", nil
}

func TestPublisherAcceptedSendRecoveryUsesExactImmutableMessage(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"publisher-accepted-crash"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, slugs) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Publisher recovery", 60, time.Now().UTC().Add(-time.Second))
	_, platformPrivate, _ := ed25519.GenerateKey(rand.Reader)
	workerPrivate, _ := ecdh.X25519().GenerateKey(rand.Reader)
	resultPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	keys, _ := secureheaders.New(1, map[int32][]byte{1: make([]byte, 32)})
	if _, err := pool.Exec(ctx, `UPDATE worker_pools SET encryption_key_id='enc-v1',encryption_public_key=$1,result_key_id='result-v1',result_public_key=$2,job_queue_url='https://sqs.local/jobs.fifo' WHERE id='hosted'`, workerPrivate.PublicKey().Bytes(), resultPublic); err != nil {
		t.Fatal(err)
	}
	scheduler, err := fifo.NewScheduler(pool, platformPrivate, "platform-v1", keys)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := scheduler.ScheduleDue(ctx, 1); err != nil || created != 1 {
		t.Fatalf("created=%d err=%v", created, err)
	}
	sender := &ambiguousSender{failFirst: true}
	publisher := fifo.NewPublisher(pool, sender)
	if worked, err := publisher.PublishNext(ctx); !worked || err == nil {
		t.Fatalf("worked=%t err=%v", worked, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE check_dispatch_outbox SET next_attempt_at=CURRENT_TIMESTAMP WHERE job_id IN(SELECT id FROM check_jobs WHERE monitor_id=$1::uuid)`, monitorID); err != nil {
		t.Fatal(err)
	}
	if worked, err := publisher.PublishNext(ctx); !worked || err != nil {
		t.Fatalf("retry worked=%t err=%v", worked, err)
	}
	if len(sender.inputs) != 2 || !bytes.Equal(sender.inputs[0].Body, sender.inputs[1].Body) || sender.inputs[0].DeduplicationID != sender.inputs[1].DeduplicationID || sender.inputs[0].GroupID != sender.inputs[1].GroupID || sender.inputs[0].Attributes != sender.inputs[1].Attributes {
		t.Fatal("publisher recovery changed the immutable FIFO message")
	}
}

func TestReliableEngineSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT") == "" {
		t.Skip("WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT is not set")
	}
	ctx, pool := openSchedulerTestPool(t)
	for _, table := range []string{"worker_pools", "check_dispatch_outbox", "monitor_schedule_periods", "monitor_rollups_hourly", "monitor_rollups_daily", "monitoring_coverage_gaps"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("Phase 1.2 table %s survived rollback", table)
		}
	}
	var versionColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='monitors' AND column_name='version')`).Scan(&versionColumn); err != nil {
		t.Fatal(err)
	}
	if versionColumn {
		t.Fatal("Phase 1.2 monitor columns survived rollback")
	}
}

type workerLoopback struct {
	delivery workqueue.Delivery
	result   []byte
	acks     int
}

func (w *workerLoopback) Pull(context.Context, time.Duration) (workqueue.Delivery, error) {
	return w.delivery, nil
}
func (w *workerLoopback) Extend(context.Context, workqueue.Delivery, time.Duration) error { return nil }
func (w *workerLoopback) PublishResultAndAcknowledge(_ context.Context, _ workqueue.Delivery, result []byte) error {
	w.result = append([]byte(nil), result...)
	w.acks++
	return nil
}
func (w *workerLoopback) AcknowledgeExpired(context.Context, workqueue.Delivery, []byte) error {
	w.acks++
	return nil
}

type capturedResultSource struct {
	body  []byte
	acked int
}

func (s *capturedResultSource) PullResult(context.Context, time.Duration) (fifo.ResultDelivery, error) {
	result, err := envelope.PeekResult(s.body)
	if err != nil {
		return fifo.ResultDelivery{}, err
	}
	return fifo.ResultDelivery{Body: s.body, Attributes: envelope.ResultAttributes(result), Receipt: "result-receipt"}, nil
}
func (s *capturedResultSource) AcknowledgeResult(context.Context, fifo.ResultDelivery) error {
	s.acked++
	return nil
}

type engineDoer func(*http.Request) (*http.Response, error)

func (d engineDoer) Do(request *http.Request) (*http.Response, error) { return d(request) }

func TestReliableFIFOEngineWithPostgreSQL(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"reliable-fifo-engine"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, slugs) })

	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Encrypted FIFO monitor", 60, time.Now().UTC().Add(-time.Second))
	if _, err := pool.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at) SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,next_check_at,next_check_at FROM monitors WHERE id=$1::uuid`, monitorID); err != nil {
		t.Fatal(err)
	}
	platformPublic, platformPrivate, _ := ed25519.GenerateKey(rand.Reader)
	resultPublic, resultPrivate, _ := ed25519.GenerateKey(rand.Reader)
	workerPrivate, _ := ecdh.X25519().GenerateKey(rand.Reader)
	headerKeys, _ := secureheaders.New(1, map[int32][]byte{1: make([]byte, 32)})
	ciphertext, keyVersion, err := headerKeys.Encrypt(map[string]string{"Authorization": "Bearer monitor-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE worker_pools SET encryption_key_id='enc-v1',encryption_public_key=$1,result_key_id='result-v1',result_public_key=$2,job_queue_url='https://sqs.local/jobs.fifo' WHERE id='hosted'`, workerPrivate.PublicKey().Bytes(), resultPublic); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE monitors SET headers_ciphertext=$1,header_key_version=$2 WHERE id=$3::uuid`, ciphertext, keyVersion, monitorID); err != nil {
		t.Fatal(err)
	}

	scheduler, err := fifo.NewScheduler(pool, platformPrivate, "platform-v1", headerKeys)
	if err != nil {
		t.Fatal(err)
	}
	created, err := scheduler.ScheduleDue(ctx, 20)
	if err != nil || created != 1 {
		t.Fatalf("schedule created=%d error=%v", created, err)
	}
	var jobID string
	var storedBody []byte
	if err = pool.QueryRow(ctx, `SELECT j.id::text,o.message_body FROM check_jobs j JOIN check_dispatch_outbox o ON o.job_id=j.id WHERE j.monitor_id=$1::uuid`, monitorID).Scan(&jobID, &storedBody); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedBody), "monitor-secret") {
		t.Fatal("immutable outbox contains plaintext monitor header")
	}

	sender := &capturedSender{}
	publisher := fifo.NewPublisher(pool, sender)
	worked, err := publisher.PublishNext(ctx)
	if err != nil || !worked || string(sender.input.Body) != string(storedBody) || sender.input.DeduplicationID != jobID || sender.input.GroupID != jobID {
		t.Fatalf("publish worked=%t error=%v", worked, err)
	}
	loopback := &workerLoopback{delivery: workqueue.Delivery{Body: sender.input.Body, Attributes: sender.input.Attributes, LeaseToken: "job-receipt"}}
	journal, err := workerjournal.Open(filepath.Join(t.TempDir(), "worker.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	targetCalls := 0
	engine := checkengine.NewWithClient(engineDoer(func(request *http.Request) (*http.Response, error) {
		targetCalls++
		if request.Header.Get("Authorization") != "Bearer monitor-secret" || request.Header.Get("X-WatchTrace-Job-ID") != jobID {
			t.Fatal("worker did not receive the signed immutable request snapshot")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("response-secret-must-not-be-stored"))}, nil
	}))
	worker, err := modworker.New(loopback, journal, engine, modworker.Config{WorkerID: "worker-a", WorkerPoolID: "hosted", PlatformKeyID: "platform-v1", WorkerEncryptionKeyID: "enc-v1", ResultKeyID: "result-v1", ClockTolerance: 5 * time.Second, WorkerPrivate: workerPrivate, PlatformPublic: platformPublic, ResultPrivate: resultPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if targetCalls != 1 || loopback.acks != 2 {
		t.Fatalf("target calls=%d acknowledgements=%d", targetCalls, loopback.acks)
	}

	source := &capturedResultSource{body: loopback.result}
	consumer := fifo.NewResultConsumer(pool, source)
	if _, err = consumer.ConsumeNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = consumer.ConsumeNext(ctx); err != nil {
		t.Fatal(err)
	}
	var resultCount int
	var storedText string
	if err = pool.QueryRow(ctx, `SELECT count(*),COALESCE(string_agg(COALESCE(error_category,''),'') ,'') FROM health_checks WHERE job_id=$1::uuid`, jobID).Scan(&resultCount, &storedText); err != nil {
		t.Fatal(err)
	}
	if resultCount != 1 || strings.Contains(storedText, "response-secret") || source.acked != 2 {
		t.Fatalf("result count=%d acknowledgements=%d", resultCount, source.acked)
	}

	reports := reliability.New(pool)
	bucket := time.Now().UTC().Truncate(time.Hour)
	if _, err = reports.RollupHour(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	report, err := reports.Report(ctx, monitorID, bucket, bucket.Add(time.Hour))
	if err != nil || report.Expected != 1 || report.Observed != 1 || report.Successful != 1 || report.Unknown != 0 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}

func TestReliabilityRetentionPreservesRequiredSummaries(t *testing.T) {
	ctx, pool := openSchedulerTestPool(t)
	slugs := []string{"reliability-retention"}
	deleteSchedulerTestData(t, ctx, pool, slugs)
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), pool, slugs) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, pool, slugs[0])
	now := time.Now().UTC().Truncate(time.Hour)
	monitorID := insertSchedulerMonitor(t, ctx, pool, organizationID, environmentID, "Retention", 300, now.Add(time.Hour))
	old := now.Add(-8 * 24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO monitor_schedule_periods(organization_id,environment_id,monitor_id,monitor_version,interval_seconds,worker_pool_id,starts_at,first_slot_at,ends_at) SELECT organization_id,environment_id,id,version,interval_seconds,worker_pool_id,$2::timestamptz,$2::timestamptz,$2::timestamptz+INTERVAL '5 minutes' FROM monitors WHERE id=$1::uuid`, monitorID, old); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := pool.QueryRow(ctx, `INSERT INTO check_jobs(organization_id,environment_id,monitor_id,state,scheduled_at,started_at,completed_at,expires_at) VALUES($1::uuid,$2::uuid,$3::uuid,'completed',$4::timestamptz,$4::timestamptz,$4::timestamptz,$4::timestamptz+INTERVAL '2 minutes') RETURNING id::text`, organizationID, environmentID, monitorID, old).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO health_checks(job_id,organization_id,environment_id,monitor_id,job_type,scheduled_at,started_at,completed_at,succeeded,status_code,total_duration_microseconds) VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,'scheduled',$5,$5,$5,true,204,10)`, jobID, organizationID, environmentID, monitorID, old); err != nil {
		t.Fatal(err)
	}
	service := reliability.New(pool)
	if _, err := service.ApplyRetention(ctx, now); err != nil {
		t.Fatal(err)
	}
	var rawBeforeRollup int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM health_checks WHERE job_id=$1::uuid`, jobID).Scan(&rawBeforeRollup); err != nil || rawBeforeRollup != 1 {
		t.Fatalf("raw result removed before summary: count=%d error=%v", rawBeforeRollup, err)
	}
	if _, err := service.RollupHour(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyRetention(ctx, now); err != nil {
		t.Fatal(err)
	}
	var rawAfterRollup, hourly int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM health_checks WHERE job_id=$1::uuid),(SELECT count(*) FROM monitor_rollups_hourly WHERE monitor_id=$2::uuid AND bucket_start=$3)`, jobID, monitorID, old).Scan(&rawAfterRollup, &hourly); err != nil {
		t.Fatal(err)
	}
	if rawAfterRollup != 0 || hourly != 1 {
		t.Fatalf("retention raw=%d hourly=%d", rawAfterRollup, hourly)
	}
}
