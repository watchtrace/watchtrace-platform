package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type queueLogTransport struct{}

func (queueLogTransport) Pull(context.Context, time.Duration) (workqueue.Delivery, error) {
	return workqueue.Delivery{
		Body:         []byte("secret-job-payload"),
		LeaseToken:   "secret-receipt-handle",
		ReceiveCount: 3,
		Attributes: envelope.Attributes{
			JobID:        "job-456",
			WorkerPoolID: "hosted",
		},
	}, nil
}

func (queueLogTransport) Extend(context.Context, workqueue.Delivery, time.Duration) error {
	return nil
}

func (queueLogTransport) PublishResultAndAcknowledge(context.Context, workqueue.Delivery, []byte) error {
	return nil
}

func (queueLogTransport) AcknowledgeExpired(context.Context, workqueue.Delivery, []byte) error {
	return nil
}

func TestWorkerQueueLoggingUsesSafeIdentifiersOnly(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	transport := loggingTransport{next: queueLogTransport{}, logger: logger, transportName: "direct_sqs"}
	delivery, err := transport.Pull(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = transport.PublishResultAndAcknowledge(context.Background(), delivery, []byte("secret-result-payload")); err != nil {
		t.Fatal(err)
	}

	logs := output.String()
	for _, expected := range []string{"queue message retrieved", "queue message pushed", "job-456", "hosted", "direct_sqs"} {
		if !strings.Contains(logs, expected) {
			t.Fatalf("log does not contain %q: %s", expected, logs)
		}
	}
	for _, secret := range []string{"secret-job-payload", "secret-result-payload", "secret-receipt-handle"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("queue log contains secret %q: %s", secret, logs)
		}
	}
}
