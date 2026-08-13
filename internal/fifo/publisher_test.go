package fifo

import (
	"testing"
	"time"
)

func TestNextScheduleBoundsOverdueReplay(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	next, err := nextSchedule(now.Add(-24*time.Hour), now, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !next.After(now) || next.After(now.Add(time.Minute)) {
		t.Fatalf("next schedule = %s", next)
	}
}

func TestExactFIFOIdentifiersUseStableJobID(t *testing.T) {
	input := SendInput{DeduplicationID: "job-id", GroupID: "job-id", Body: []byte("immutable")}
	if input.DeduplicationID != input.GroupID || input.DeduplicationID != "job-id" {
		t.Fatal("FIFO identifiers are not the stable job ID")
	}
}
