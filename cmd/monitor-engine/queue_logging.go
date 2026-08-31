package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/fifo"
)

// loggingSender records the successful control-plane handoff to the job
// queue. Payloads, queue URLs, monitor targets, headers, and credentials are
// deliberately excluded from the log record.
type loggingSender struct {
	next   fifo.Sender
	logger *slog.Logger
}

func (s loggingSender) Send(ctx context.Context, input fifo.SendInput) (string, error) {
	messageID, err := s.next.Send(ctx, input)
	if err == nil {
		s.logger.InfoContext(ctx, "queue message pushed",
			"component", "queue",
			"queue_kind", "job",
			"job_id", input.Attributes.JobID,
			"worker_pool_id", input.Attributes.WorkerPoolID,
			"message_id", messageID,
		)
	}
	return messageID, err
}

// loggingResultSource records the point where the control plane retrieves a
// worker result from the result queue. Acceptance and persistence happen later
// in the result consumer and are intentionally separate concepts.
type loggingResultSource struct {
	next   fifo.ResultSource
	logger *slog.Logger
}

func (s loggingResultSource) PullResult(ctx context.Context, wait time.Duration) (fifo.ResultDelivery, error) {
	delivery, err := s.next.PullResult(ctx, wait)
	if err == nil {
		s.logger.InfoContext(ctx, "queue message retrieved",
			"component", "queue",
			"queue_kind", "result",
			"job_id", delivery.Attributes.JobID,
			"result_id", delivery.Attributes.ResultID,
			"worker_pool_id", delivery.Attributes.WorkerPoolID,
			"receive_count", delivery.ReceiveCount,
		)
	}
	return delivery, err
}

func (s loggingResultSource) AcknowledgeResult(ctx context.Context, delivery fifo.ResultDelivery) error {
	return s.next.AcknowledgeResult(ctx, delivery)
}
