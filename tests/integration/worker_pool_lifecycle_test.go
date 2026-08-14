package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/workerpool"
)

func TestWorkerPoolPartialProvisioningLifecycleAndAudit(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	poolID := "lifecycle-pool"
	_, _ = db.Exec(ctx, `DELETE FROM worker_pool_audit_events WHERE worker_pool_id=$1; DELETE FROM worker_pools WHERE id=$1`, poolID)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM worker_pool_audit_events WHERE worker_pool_id=$1; DELETE FROM worker_pools WHERE id=$1`, poolID)
	})
	encryption, _ := ecdh.X25519().GenerateKey(rand.Reader)
	resultPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	service := workerpool.New(db)
	registration := workerpool.Registration{ID: poolID, Mode: "hosted", JobQueueURL: "https://sqs.local/jobs.fifo", JobQueueARN: "arn:aws:sqs:ap-south-1:000000000000:jobs.fifo", JobDLQURL: "https://sqs.local/jobs-dlq.fifo", JobDLQARN: "arn:aws:sqs:ap-south-1:000000000000:jobs-dlq.fifo", EncryptionKeyID: "enc-v1", ResultKeyID: "result-v1", EncryptionPublic: encryption.PublicKey().Bytes(), ResultPublic: resultPublic, SchemaMin: 1, SchemaMax: 2}
	if err = service.Register(ctx, registration, "operator", "test provisioning"); err != nil {
		t.Fatal(err)
	}
	digest := bytes.Repeat([]byte{4}, 32)
	if err = service.Activate(ctx, poolID, digest, false, "operator", "missing gateway mapping"); err == nil {
		t.Fatal("partial provisioning activated")
	}
	if err = service.Activate(ctx, poolID, digest, true, "operator", "dependencies verified"); err != nil {
		t.Fatal(err)
	}
	if err = service.Reconcile(ctx, poolID, bytes.Repeat([]byte{5}, 32), true, "operator", "detect drift"); err == nil {
		t.Fatal("manifest drift was not detected")
	}
	var state string
	var enabled bool
	if err = db.QueryRow(ctx, `SELECT lifecycle_state,enabled FROM worker_pools WHERE id=$1`, poolID).Scan(&state, &enabled); err != nil || state != "failed" || enabled {
		t.Fatalf("state=%s enabled=%t err=%v", state, enabled, err)
	}
	if err = service.Transition(ctx, poolID, "deleting", "operator", "remove failed pool"); err != nil {
		t.Fatal(err)
	}
	ready := workerpool.DeletionReadiness{SourceQueueEmpty: true, DeadLetterQueueEmpty: true, GatewayMappingRemoved: true}
	if err = service.Delete(ctx, poolID, "wrong", "operator", "remove failed pool", ready); err == nil {
		t.Fatal("inexact delete confirmation accepted")
	}
	if err = service.Delete(ctx, poolID, "delete:"+poolID, "operator", "remove failed pool", workerpool.DeletionReadiness{}); err == nil {
		t.Fatal("deletion without drained queues and gateway removal was accepted")
	}
	if err = service.Delete(ctx, poolID, "delete:"+poolID, "operator", "remove failed pool", ready); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM worker_pool_audit_events WHERE worker_pool_id=$1`, poolID).Scan(&audits); err != nil || audits < 5 {
		t.Fatalf("audits=%d err=%v", audits, err)
	}
}
