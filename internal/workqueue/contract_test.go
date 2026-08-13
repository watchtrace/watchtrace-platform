package workqueue

import (
	"context"
	"errors"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"testing"
	"time"
)

type recordingTransport struct {
	published, acked bool
	err              error
}

func (r *recordingTransport) Pull(context.Context, time.Duration) (Delivery, error) {
	return Delivery{}, ErrNoMessage
}
func (r *recordingTransport) Extend(context.Context, Delivery, time.Duration) error { return nil }
func (r *recordingTransport) PublishResultAndAcknowledge(_ context.Context, _ Delivery, _ []byte) error {
	r.published = true
	if r.err != nil {
		return r.err
	}
	r.acked = true
	return nil
}
func (r *recordingTransport) AcknowledgeExpired(context.Context, Delivery, []byte) error {
	r.acked = true
	return nil
}
func TestTransportCannotAcknowledgeWhenPublicationFails(t *testing.T) {
	r := &recordingTransport{err: errors.New("publish")}
	_ = r.PublishResultAndAcknowledge(context.Background(), Delivery{Attributes: envelope.Attributes{JobID: "x"}}, []byte("result"))
	if !r.published || r.acked {
		t.Fatal("acknowledged before durable result publication")
	}
}
