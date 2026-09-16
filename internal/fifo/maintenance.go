package fifo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

type Metrics struct {
	PendingJobs                    int64         `json:"pending_jobs"`
	PublishedJobs                  int64         `json:"published_jobs"`
	RunningJobs                    int64         `json:"running_jobs"`
	DeadJobs                       int64         `json:"dead_jobs"`
	ExpiredJobs                    int64         `json:"expired_jobs"`
	OutboxPending                  int64         `json:"outbox_pending"`
	OutboxAmbiguous                int64         `json:"outbox_ambiguous"`
	OldestOutboxAge                time.Duration `json:"-"`
	OldestOutboxAgeSeconds         int64         `json:"oldest_outbox_age_seconds"`
	OldestNonterminalJobAgeSeconds int64         `json:"oldest_nonterminal_job_age_seconds"`
}

func ReclaimPublisherLeases(ctx context.Context, db DB) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	count, err := database.New(tx).ReclaimDispatchPublisherLeases(ctx)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
func CleanupLedger(ctx context.Context, db DB, now time.Time) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	count, err := database.New(tx).DeleteOldLedgerJobs(ctx, database.DeleteOldLedgerJobsParams{CompletedBefore: databaseTimestamp(now.UTC().Add(-48 * time.Hour)), NonterminalBefore: databaseTimestamp(now.UTC().Add(-7 * 24 * time.Hour))})
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
func ReadMetrics(ctx context.Context, db DB, now time.Time) (Metrics, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Metrics{}, err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	jobCounts, err := queries.GetCheckJobStateCounts(ctx)
	if err != nil {
		return Metrics{}, err
	}
	dispatchCounts, err := queries.GetDispatchStateCounts(ctx)
	if err != nil {
		return Metrics{}, err
	}
	m := Metrics{PendingJobs: jobCounts.Pending, PublishedJobs: jobCounts.Published, RunningJobs: jobCounts.Running, DeadJobs: jobCounts.Dead, ExpiredJobs: jobCounts.Expired, OutboxPending: dispatchCounts.Pending, OutboxAmbiguous: dispatchCounts.Ambiguous}
	oldest, err := queries.GetOldestNonterminalDispatch(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return m, err
	}
	if err == nil && oldest.Valid {
		m.OldestOutboxAge = now.UTC().Sub(oldest.Time)
		if m.OldestOutboxAge < 0 {
			m.OldestOutboxAge = 0
		}
		m.OldestOutboxAgeSeconds = int64(m.OldestOutboxAge.Seconds())
	}
	oldestJob, err := queries.GetOldestActiveCheckJob(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return m, err
	}
	if err == nil && oldestJob.Valid {
		m.OldestNonterminalJobAgeSeconds = int64(now.UTC().Sub(oldestJob.Time).Seconds())
		if m.OldestNonterminalJobAgeSeconds < 0 {
			m.OldestNonterminalJobAgeSeconds = 0
		}
	}
	return m, nil
}

func (c *ResultConsumer) SweepDeadlines(ctx context.Context) (int64, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	count, err := database.New(tx).SweepExpiredCheckJobs(ctx)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
