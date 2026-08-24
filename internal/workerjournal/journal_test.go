package workerjournal

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalDurablyReplaysResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = j.Accept(ctx, "job", "hash"); err != nil {
		t.Fatal(err)
	}
	if err = j.StoreResult(ctx, "job", "hash", []byte("signed")); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	got, ok, err := j.Result(ctx, "job", "hash")
	if err != nil || !ok || string(got) != "signed" {
		t.Fatalf("got=%q ok=%v err=%v", got, ok, err)
	}
	if _, err = j.Cleanup(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestJournalRejectsJobIDSnapshotConflictBeforeExecution(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	if err = j.Accept(ctx, "job", "first"); err != nil {
		t.Fatal(err)
	}
	if err = j.Accept(ctx, "job", "different"); err == nil {
		t.Fatal("conflicting snapshot was accepted")
	}
}

func TestJournalMetricsExposePendingReplayHealth(t *testing.T) {
	j, err := Open(filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err = j.Accept(context.Background(), "pending-job", "hash"); err != nil {
		t.Fatal(err)
	}
	metrics, err := j.Metrics(context.Background())
	if err != nil || metrics.Accepted != 1 || metrics.Completed != 0 || metrics.OldestAcceptedAgeSeconds < 0 {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
}
