package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

// loggingTransport makes the database-free worker's queue handoffs visible
// without exposing encrypted payloads, receipt handles, target URLs, headers,
// tokens, or queue URLs.
type loggingTransport struct {
	next          workqueue.Transport
	logger        *slog.Logger
	transportName string
}

func (t loggingTransport) Pull(ctx context.Context, wait time.Duration) (workqueue.Delivery, error) {
	delivery, err := t.next.Pull(ctx, wait)
	if err == nil {
		t.logger.InfoContext(ctx, "queue message retrieved",
			"component", "queue",
			"queue_kind", "job",
			"transport", t.transportName,
			"job_id", delivery.Attributes.JobID,
			"worker_pool_id", delivery.Attributes.WorkerPoolID,
			"receive_count", delivery.ReceiveCount,
		)
	}
	return delivery, err
}

func (t loggingTransport) Extend(ctx context.Context, delivery workqueue.Delivery, duration time.Duration) error {
	return t.next.Extend(ctx, delivery, duration)
}

func (t loggingTransport) PublishResultAndAcknowledge(ctx context.Context, delivery workqueue.Delivery, body []byte) error {
	err := t.next.PublishResultAndAcknowledge(ctx, delivery, body)
	if err == nil {
		resultID := ""
		if result, parseErr := envelope.PeekResult(body); parseErr == nil {
			resultID = result.ResultID
		}
		t.logger.InfoContext(ctx, "queue message pushed",
			"component", "queue",
			"queue_kind", "result",
			"transport", t.transportName,
			"job_id", delivery.Attributes.JobID,
			"result_id", resultID,
			"worker_pool_id", delivery.Attributes.WorkerPoolID,
		)
	}
	return err
}

func (t loggingTransport) AcknowledgeExpired(ctx context.Context, delivery workqueue.Delivery, body []byte) error {
	return t.next.AcknowledgeExpired(ctx, delivery, body)
}
