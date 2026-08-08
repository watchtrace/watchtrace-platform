package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNextScheduleAfterUsesPlannedTime(t *testing.T) {
	scheduledAt := time.Date(2026, 8, 8, 12, 0, 30, 125000000, time.UTC)
	tests := []struct {
		name            string
		schedulerTime   time.Time
		intervalSeconds int32
		want            time.Time
	}{
		{
			name:            "ordinary interval",
			schedulerTime:   scheduledAt.Add(2 * time.Second),
			intervalSeconds: 60,
			want:            scheduledAt.Add(60 * time.Second),
		},
		{
			name:            "exact next boundary",
			schedulerTime:   scheduledAt.Add(60 * time.Second),
			intervalSeconds: 60,
			want:            scheduledAt.Add(120 * time.Second),
		},
		{
			name:            "overdue intervals skip replay backlog",
			schedulerTime:   scheduledAt.Add(12*time.Minute + 3*time.Second),
			intervalSeconds: 300,
			want:            scheduledAt.Add(15 * time.Minute),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := nextScheduleAfter(scheduledAt, test.schedulerTime, test.intervalSeconds)
			if err != nil {
				t.Fatalf("next schedule: %v", err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("next schedule = %s, want %s", got, test.want)
			}
		})
	}
}

func TestNextScheduleAfterRejectsInvalidInterval(t *testing.T) {
	if _, err := nextScheduleAfter(time.Now(), time.Now(), 0); err == nil {
		t.Fatal("zero interval unexpectedly succeeded")
	}
}

func TestScheduleDueRejectsUnboundedBatchBeforeDatabaseAccess(t *testing.T) {
	service := NewService(nil)
	for _, batchSize := range []int{0, -1, MaximumBatchSize + 1} {
		if _, err := service.ScheduleDue(context.Background(), batchSize); !errors.Is(err, ErrInvalidBatchSize) {
			t.Fatalf("batch size %d error = %v, want ErrInvalidBatchSize", batchSize, err)
		}
	}
}
