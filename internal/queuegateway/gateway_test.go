package queuegateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type fakeTransport struct {
	delivery  workqueue.Delivery
	published []byte
	acked     bool
}

func (f *fakeTransport) Pull(context.Context, time.Duration) (workqueue.Delivery, error) {
	return f.delivery, nil
}
func (f *fakeTransport) Extend(context.Context, workqueue.Delivery, time.Duration) error { return nil }
func (f *fakeTransport) PublishResultAndAcknowledge(_ context.Context, _ workqueue.Delivery, result []byte) error {
	f.published = append([]byte(nil), result...)
	f.acked = true
	return nil
}
func (f *fakeTransport) AcknowledgeExpired(context.Context, workqueue.Delivery, []byte) error {
	f.acked = true
	return nil
}

func TestGatewayAuthenticatesLeaseAndPublishesBeforeAcknowledging(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC().Truncate(time.Microsecond)
	transport := &fakeTransport{delivery: workqueue.Delivery{
		Body:       []byte("encrypted"),
		Attributes: envelope.Attributes{SchemaVersion: 1, JobID: "job", WorkerPoolID: "pool-a", SnapshotHash: string(bytes.Repeat([]byte{'a'}, 64)), ExpiresAt: now.Add(time.Minute)},
		LeaseToken: "sqs-receipt",
	}}
	gateway, err := New([]Pool{{ID: "pool-a", ResultKeyID: "result-v1", Transport: transport, ResultPublic: public}}, bytes.Repeat([]byte{1}, 32), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gateway.now = func() time.Time { return now }
	handler := gateway.Handler()

	pull := authenticatedRequest(http.MethodPost, "/v1/jobs/pull", nil, "pool-a")
	pullRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pullRecorder, pull)
	if pullRecorder.Code != http.StatusOK {
		t.Fatalf("pull status = %d", pullRecorder.Code)
	}
	var pulled pullResponse
	if err = json.Unmarshal(pullRecorder.Body.Bytes(), &pulled); err != nil {
		t.Fatal(err)
	}
	resultBody, err := envelope.SignResult(envelope.Result{SchemaVersion: 1, ResultID: "result", JobID: "job", SnapshotHash: transport.delivery.Attributes.SnapshotHash, WorkerPoolID: "pool-a", WorkerID: "worker", AttemptID: "attempt", ScheduledAt: now, StartedAt: now, CompletedAt: now.Add(time.Second), Succeeded: true, StatusCode: int16Pointer(204), TotalMicros: 1_000_000, ResultKeyID: "result-v1"}, private)
	if err != nil {
		t.Fatal(err)
	}
	action, _ := json.Marshal(actionRequest{LeaseToken: pulled.LeaseToken, Body: base64.StdEncoding.EncodeToString(resultBody)})
	resultRequest := authenticatedRequest(http.MethodPost, "/v1/jobs/result", bytes.NewReader(action), "pool-a")
	resultResponse := httptest.NewRecorder()
	handler.ServeHTTP(resultResponse, resultRequest)
	if resultResponse.Code != http.StatusNoContent || !transport.acked || !bytes.Equal(transport.published, resultBody) {
		t.Fatalf("result status=%d acked=%t", resultResponse.Code, transport.acked)
	}

	action[10] ^= 1
	tampered := authenticatedRequest(http.MethodPost, "/v1/jobs/result", bytes.NewReader(action), "pool-a")
	tamperedResponse := httptest.NewRecorder()
	handler.ServeHTTP(tamperedResponse, tampered)
	if tamperedResponse.Code < 400 {
		t.Fatal("tampered lease request was accepted")
	}
}

func authenticatedRequest(method, path string, body *bytes.Reader, pool string) *http.Request {
	if body == nil {
		body = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, body)
	now := time.Now().UTC()
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: pool}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour)}}}
	return request
}

func int16Pointer(value int16) *int16 { return &value }

func TestGatewayMalformedResultNeverAcknowledgesJob(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	transport := &fakeTransport{delivery: workqueue.Delivery{Body: []byte("encrypted"), Attributes: envelope.Attributes{SchemaVersion: 2, JobID: "job", WorkerPoolID: "pool-a", SnapshotHash: string(bytes.Repeat([]byte{'a'}, 64)), ExpiresAt: now.Add(time.Minute)}, LeaseToken: "receipt"}}
	gateway, err := New([]Pool{{ID: "pool-a", ResultKeyID: "result-v1", Transport: transport, ResultPublic: public}}, bytes.Repeat([]byte{2}, 32), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gateway.now = func() time.Time { return now }
	handler := gateway.Handler()
	pull := authenticatedRequest(http.MethodPost, "/v1/jobs/pull", nil, "pool-a")
	pullRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pullRecorder, pull)
	var response pullResponse
	if json.Unmarshal(pullRecorder.Body.Bytes(), &response) != nil {
		t.Fatal("invalid pull response")
	}
	action, _ := json.Marshal(actionRequest{LeaseToken: response.LeaseToken, Body: base64.StdEncoding.EncodeToString([]byte("malformed"))})
	request := authenticatedRequest(http.MethodPost, "/v1/jobs/result", bytes.NewReader(action), "pool-a")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity || transport.acked || len(transport.published) != 0 {
		t.Fatalf("status=%d acked=%t", recorder.Code, transport.acked)
	}
}

func TestGatewayRejectsRevokedCertificateAndLimitsOnePoolOnly(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	delivery := workqueue.Delivery{Body: []byte("encrypted"), Attributes: envelope.Attributes{SchemaVersion: 2, JobID: "job", WorkerPoolID: "pool-a", SnapshotHash: string(bytes.Repeat([]byte{'b'}, 64)), ExpiresAt: now.Add(time.Minute)}, LeaseToken: "receipt"}
	transportA := &fakeTransport{delivery: delivery}
	delivery.Attributes.WorkerPoolID = "pool-b"
	transportB := &fakeTransport{delivery: delivery}
	gateway, err := New([]Pool{{ID: "pool-a", ResultKeyID: "key", Transport: transportA, ResultPublic: public, MaxRequestsPerMinute: 1, RevokedCertificateSerials: map[string]struct{}{"1": {}}}, {ID: "pool-b", ResultKeyID: "key", Transport: transportB, ResultPublic: public, MaxRequestsPerMinute: 2}}, bytes.Repeat([]byte{3}, 32), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gateway.now = func() time.Time { return now }
	handler := gateway.Handler()
	revoked := authenticatedRequest(http.MethodPost, "/v1/jobs/pull", nil, "pool-a")
	revokedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(revokedRecorder, revoked)
	if revokedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status=%d", revokedRecorder.Code)
	}
	first := authenticatedRequest(http.MethodPost, "/v1/jobs/pull", nil, "pool-b")
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("pool B status=%d", firstRecorder.Code)
	}
}
