package workqueue_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/queuegateway"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type parityTransport struct {
	delivery  workqueue.Delivery
	published []byte
	acked     bool
}

type directSQSBackend struct {
	delivery workqueue.Delivery
	events   []string
	result   []byte
}

func (d *directSQSBackend) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	a := d.delivery.Attributes
	attributes := map[string]types.MessageAttributeValue{
		"schema_version":           {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(a.SchemaVersion))},
		"job_id":                   {DataType: aws.String("String"), StringValue: aws.String(a.JobID)},
		"worker_pool_id":           {DataType: aws.String("String"), StringValue: aws.String(a.WorkerPoolID)},
		"snapshot_hash":            {DataType: aws.String("String"), StringValue: aws.String(a.SnapshotHash)},
		"expires_at":               {DataType: aws.String("String"), StringValue: aws.String(a.ExpiresAt.Format(time.RFC3339Nano))},
		"platform_key_id":          {DataType: aws.String("String"), StringValue: aws.String(a.PlatformKeyID)},
		"worker_encryption_key_id": {DataType: aws.String("String"), StringValue: aws.String(a.WorkerEncryptionKeyID)},
	}
	wireBody := base64.StdEncoding.EncodeToString(d.delivery.Body)
	return &sqs.ReceiveMessageOutput{Messages: []types.Message{{Body: aws.String(wireBody), ReceiptHandle: aws.String("receipt"), MessageAttributes: attributes, Attributes: map[string]string{"ApproximateReceiveCount": "2"}}}}, nil
}
func (d *directSQSBackend) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	d.events = append(d.events, "extend")
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}
func (d *directSQSBackend) SendMessage(_ context.Context, input *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	d.events = append(d.events, "publish")
	d.result, _ = base64.StdEncoding.DecodeString(aws.ToString(input.MessageBody))
	return &sqs.SendMessageOutput{MessageId: aws.String("result-message")}, nil
}
func (d *directSQSBackend) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	d.events = append(d.events, "ack")
	return &sqs.DeleteMessageOutput{}, nil
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
	server, client := startMTLSServer(t, gateway.Handler(), "pool-a")
	defer server.Close()
	transport := &workqueue.HTTPS{BaseURL: server.URL, Client: client}
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

func TestDirectSQSAndHTTPSTransportsPassTheSameExecutedJobContract(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	attrs := envelope.Attributes{SchemaVersion: 2, JobID: "parity-job", WorkerPoolID: "pool-a", SnapshotHash: string(bytes.Repeat([]byte{'b'}, 64)), ExpiresAt: now.Add(time.Minute), PlatformKeyID: "platform", WorkerEncryptionKeyID: "worker"}
	delivery := workqueue.Delivery{Body: []byte("encrypted"), Attributes: attrs, LeaseToken: "receipt", ReceiveCount: 2}
	result, err := envelope.SignResult(envelope.Result{SchemaVersion: 2, ResultID: "parity-result", JobID: attrs.JobID, SnapshotHash: attrs.SnapshotHash, WorkerPoolID: attrs.WorkerPoolID, WorkerID: "worker", AttemptID: "attempt", ScheduledAt: now, StartedAt: now, CompletedAt: now.Add(time.Second), Succeeded: true, StatusCode: int16Ptr(204), TotalMicros: 1, ResultKeyID: "result"}, private)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("direct_sqs", func(t *testing.T) {
		backend := &directSQSBackend{delivery: delivery}
		transport := &workqueue.DirectSQS{Client: backend, JobQueueURL: "https://sqs.test/jobs.fifo", ResultQueueURL: "https://sqs.test/results.fifo", WorkerPoolID: attrs.WorkerPoolID}
		assertExecutedContract(t, transport, delivery, result)
		if !bytes.Equal(backend.result, result) || len(backend.events) != 3 || backend.events[0] != "extend" || backend.events[1] != "publish" || backend.events[2] != "ack" {
			t.Fatalf("direct events=%v", backend.events)
		}
	})

	t.Run("https_mtls_gateway_contract", func(t *testing.T) {
		inner := &parityTransport{delivery: delivery}
		gateway, err := queuegateway.New([]queuegateway.Pool{{ID: attrs.WorkerPoolID, ResultKeyID: "result", ResultPublic: public, Transport: inner}}, bytes.Repeat([]byte{5}, 32), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		server, client := startMTLSServer(t, gateway.Handler(), attrs.WorkerPoolID)
		defer server.Close()
		transport := &workqueue.HTTPS{BaseURL: server.URL, Client: client}
		assertExecutedContract(t, transport, delivery, result)
		if !inner.acked || !bytes.Equal(inner.published, result) {
			t.Fatal("HTTPS gateway did not publish before acknowledging")
		}
	})
}

func assertExecutedContract(t *testing.T, transport workqueue.Transport, want workqueue.Delivery, result []byte) {
	t.Helper()
	delivery, err := transport.Pull(context.Background(), 20*time.Second)
	if err != nil || !bytes.Equal(delivery.Body, want.Body) || delivery.Attributes != want.Attributes || delivery.ReceiveCount != want.ReceiveCount {
		t.Fatalf("pull=%+v err=%v", delivery, err)
	}
	if err = transport.Extend(context.Background(), delivery, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = transport.PublishResultAndAcknowledge(context.Background(), delivery, result); err != nil {
		t.Fatal(err)
	}
}

func int16Ptr(v int16) *int16 { return &v }

func startMTLSServer(t *testing.T, handler http.Handler, poolID string) (*httptest.Server, *http.Client) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate mTLS key: %v", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: poolID},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("create mTLS certificate: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal mTLS key: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	certificate, err := tls.X509KeyPair(certificatePEM, privatePEM)
	if err != nil {
		t.Fatalf("load mTLS certificate: %v", err)
	}
	parsedCertificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse mTLS certificate: %v", err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(parsedCertificate)

	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
		MinVersion: tls.VersionTLS12,
	}
	server.StartTLS()

	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
	client.Transport = transport
	return server, client
}
