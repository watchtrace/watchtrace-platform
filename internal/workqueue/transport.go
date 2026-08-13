// Package workqueue defines the database-free worker transport contract.
package workqueue

import (
	"context"
	"errors"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
)

var ErrNoMessage = errors.New("no queue message available")

type Delivery struct {
	Body         []byte
	Attributes   envelope.Attributes
	LeaseToken   string
	ReceiveCount int
}

// Transport has identical ordering in direct-SQS and HTTPS modes: executed
// results are published durably before the job acknowledgement succeeds.
type Transport interface {
	Pull(context.Context, time.Duration) (Delivery, error)
	Extend(context.Context, Delivery, time.Duration) error
	PublishResultAndAcknowledge(context.Context, Delivery, []byte) error
	AcknowledgeExpired(context.Context, Delivery, []byte) error
}
