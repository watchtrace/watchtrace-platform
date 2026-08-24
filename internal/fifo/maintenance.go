package fifo

import (
	"context"
	"time"
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
	tag, err := tx.Exec(ctx, `UPDATE check_dispatch_outbox SET publish_state=CASE WHEN publish_attempts>=3 THEN 'ambiguous' ELSE 'pending' END,publish_lease_token=NULL,publish_lease_expires_at=NULL,last_safe_error='publisher_interrupted',updated_at=CURRENT_TIMESTAMP WHERE publish_state='publishing' AND publish_lease_expires_at<CURRENT_TIMESTAMP`)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
func CleanupLedger(ctx context.Context, db DB, now time.Time) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `DELETE FROM check_jobs WHERE (state='completed' AND completed_at<$1) OR (state IN('dead','expired','cancelled','quarantined') AND completed_at<$2)`, now.UTC().Add(-48*time.Hour), now.UTC().Add(-7*24*time.Hour))
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
func ReadMetrics(ctx context.Context, db DB, now time.Time) (Metrics, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Metrics{}, err
	}
	defer tx.Rollback(context.Background())
	var m Metrics
	var oldest *time.Time
	var oldestJob *time.Time
	err = tx.QueryRow(ctx, `SELECT count(*)FILTER(WHERE state IN('pending','pending_publish')),count(*)FILTER(WHERE state='published'),count(*)FILTER(WHERE state='running'),count(*)FILTER(WHERE state='dead'),count(*)FILTER(WHERE state='expired'),min(created_at)FILTER(WHERE state IN('pending','pending_publish','published','running')) FROM check_jobs`).Scan(&m.PendingJobs, &m.PublishedJobs, &m.RunningJobs, &m.DeadJobs, &m.ExpiredJobs, &oldestJob)
	if err != nil {
		return m, err
	}
	err = tx.QueryRow(ctx, `SELECT count(*)FILTER(WHERE publish_state IN('pending','publishing')),count(*)FILTER(WHERE publish_state='ambiguous'),min(created_at)FILTER(WHERE publish_state IN('pending','publishing','ambiguous')) FROM check_dispatch_outbox`).Scan(&m.OutboxPending, &m.OutboxAmbiguous, &oldest)
	if err != nil {
		return m, err
	}
	if oldest != nil {
		m.OldestOutboxAge = now.UTC().Sub(*oldest)
		if m.OldestOutboxAge < 0 {
			m.OldestOutboxAge = 0
		}
		m.OldestOutboxAgeSeconds = int64(m.OldestOutboxAge.Seconds())
	}
	if oldestJob != nil {
		m.OldestNonterminalJobAgeSeconds = int64(now.UTC().Sub(*oldestJob).Seconds())
		if m.OldestNonterminalJobAgeSeconds < 0 {
			m.OldestNonterminalJobAgeSeconds = 0
		}
	}
	return m, nil
}
