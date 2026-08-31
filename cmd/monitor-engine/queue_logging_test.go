package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
)

type queueLogSender struct{}

func (queueLogSender) Send(context.Context, fifo.SendInput) (string, error) {
	return "message-123", nil
}

type queueLogResultSource struct{}

func (queueLogResultSource) PullResult(context.Context, time.Duration) (fifo.ResultDelivery, error) {
	return fifo.ResultDelivery{
		Body:         []byte("secret-result-payload"),
		Receipt:      "secret-receipt-handle",
		ReceiveCount: 2,
		Attributes: envelope.Attributes{
			JobID:        "job-123",
			ResultID:     "result-123",
			WorkerPoolID: "hosted",
		},
	}, nil
}

func (queueLogResultSource) AcknowledgeResult(context.Context, fifo.ResultDelivery) error {
	return nil
}

func TestControlPlaneQueueLoggingUsesSafeIdentifiersOnly(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	sender := loggingSender{next: queueLogSender{}, logger: logger}
	_, err := sender.Send(context.Background(), fifo.SendInput{
		QueueURL: "https://queue.example/secret-account/jobs.fifo",
		Body:     []byte("secret-job-payload"),
		Attributes: envelope.Attributes{
			JobID:        "job-123",
			WorkerPoolID: "hosted",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := loggingResultSource{next: queueLogResultSource{}, logger: logger}
	if _, err = source.PullResult(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}

	logs := output.String()
	for _, expected := range []string{"queue message pushed", "queue message retrieved", "job-123", "result-123", "hosted"} {
		if !strings.Contains(logs, expected) {
			t.Fatalf("log does not contain %q: %s", expected, logs)
		}
	}
	for _, secret := range []string{"secret-job-payload", "secret-result-payload", "secret-receipt-handle", "secret-account"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("queue log contains secret %q: %s", secret, logs)
		}
	}
}
