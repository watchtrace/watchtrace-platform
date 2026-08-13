package workqueue_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/queuegateway"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type parityTransport struct {
	delivery  workqueue.Delivery
	published []byte
	acked     bool
}

func (p *parityTransport) Pull(context.Context, time.Duration) (workqueue.Delivery, error) {
	return p.delivery, nil
}
func (p *parityTransport) Extend(context.Context, workqueue.Delivery, time.Duration) error {
	return nil
}
func (p *parityTransport) PublishResultAndAcknowledge(_ context.Context, _ workqueue.Delivery, body []byte) error {
	p.published = append([]byte(nil), body...)
	p.acked = true
	return nil
}
func (p *parityTransport) AcknowledgeExpired(context.Context, workqueue.Delivery, []byte) error {
	p.acked = true
	return nil
}

func TestHTTPSTransportUsesSameImmutableContract(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	attrs := envelope.Attributes{SchemaVersion: 2, JobID: "job", WorkerPoolID: "pool-a", SnapshotHash: string(bytes.Repeat([]byte{'a'}, 64)), ExpiresAt: now.Add(time.Minute), PlatformKeyID: "platform", WorkerEncryptionKeyID: "worker"}
	inner := &parityTransport{delivery: workqueue.Delivery{Body: []byte("encrypted"), Attributes: attrs, LeaseToken: "receipt", ReceiveCount: 2}}
	gateway, err := queuegateway.New([]queuegateway.Pool{{ID: "pool-a", ResultKeyID: "result", ResultPublic: public, Transport: inner}}, bytes.Repeat([]byte{4}, 32), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gateway.WithTokenValidator(func(context.Context, string) (string, bool) { return "pool-a", true })
	server := httptest.NewUnstartedServer(gateway.Handler())
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	transport := &workqueue.HTTPS{BaseURL: server.URL, Client: client, PoolToken: "test"}
	delivery, err := transport.Pull(context.Background(), 20*time.Second)
	if err != nil || !bytes.Equal(delivery.Body, inner.delivery.Body) || delivery.Attributes != attrs || delivery.ReceiveCount != 2 {
		t.Fatalf("pull=%+v err=%v", delivery, err)
	}
	result, err := envelope.SignResult(envelope.Result{SchemaVersion: 2, ResultID: "result-id", JobID: "job", SnapshotHash: attrs.SnapshotHash, WorkerPoolID: "pool-a", WorkerID: "worker", AttemptID: "attempt", ScheduledAt: now, StartedAt: now, CompletedAt: now.Add(time.Second), Succeeded: true, StatusCode: int16Ptr(204), TotalMicros: 1, ResultKeyID: "result"}, private)
	if err != nil {
		t.Fatal(err)
	}
	if err = transport.PublishResultAndAcknowledge(context.Background(), delivery, result); err != nil || !inner.acked || !bytes.Equal(inner.published, result) {
		t.Fatalf("publish err=%v acked=%t", err, inner.acked)
	}
}

func int16Ptr(v int16) *int16 { return &v }
