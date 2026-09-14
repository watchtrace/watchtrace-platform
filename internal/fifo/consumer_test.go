package fifo

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
)

type unavailableDB struct{}

func (unavailableDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("postgres unavailable")
}

type countingResultSource struct{ pulls int }

func (s *countingResultSource) PullResult(context.Context, time.Duration) (ResultDelivery, error) {
	s.pulls++
	return ResultDelivery{}, nil
}
func (*countingResultSource) AcknowledgeResult(context.Context, ResultDelivery) error { return nil }

func TestResultConsumerDoesNotReceiveWhilePostgreSQLIsUnavailable(t *testing.T) {
	source := &countingResultSource{}
	sealer, err := quarantine.New(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := NewResultConsumer(unavailableDB{}, source, sealer)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := consumer.ConsumeNext(context.Background()); err == nil || worked {
		t.Fatalf("worked=%t err=%v", worked, err)
	}
	if source.pulls != 0 {
		t.Fatalf("received %d messages while database was unavailable", source.pulls)
	}
}

func TestNewResultConsumerRequiresCompleteConfiguration(t *testing.T) {
	source := &countingResultSource{}
	sealer, err := quarantine.New(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewResultConsumer(nil, source, sealer); err == nil {
		t.Fatal("NewResultConsumer accepted a missing database")
	}
	if _, err = NewResultConsumer(unavailableDB{}, nil, sealer); err == nil {
		t.Fatal("NewResultConsumer accepted a missing result source")
	}
	if _, err = NewResultConsumer(unavailableDB{}, source, nil); err == nil {
		t.Fatal("NewResultConsumer accepted a missing quarantine sealer")
	}
}
