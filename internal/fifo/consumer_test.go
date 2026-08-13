package fifo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	consumer := NewResultConsumer(unavailableDB{}, source)
	if worked, err := consumer.ConsumeNext(context.Background()); err == nil || worked {
		t.Fatalf("worked=%t err=%v", worked, err)
	}
	if source.pulls != 0 {
		t.Fatalf("received %d messages while database was unavailable", source.pulls)
	}
}
