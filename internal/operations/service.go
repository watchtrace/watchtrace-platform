// Package operations exposes bounded, non-sensitive platform health and maintenance state.
package operations

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}
type Service struct {
	db        DB
	now       func() time.Time
	sqs       SQSClient
	queueURLs QueueURLs
}

func New(db DB) *Service { return &Service{db: db, now: time.Now} }

func NewWithSQS(db DB, client SQSClient, urls QueueURLs) *Service {
	return &Service{db: db, now: time.Now, sqs: client, queueURLs: urls}
}

type Maintenance struct {
	Task          string     `json:"task"`
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	LastSafeError *string    `json:"last_safe_error,omitempty"`
	RowsAffected  int64      `json:"rows_affected"`
}
type Health struct {
	GeneratedAt                  time.Time       `json:"generated_at"`
	DatabaseDelayMilliseconds    int64           `json:"database_delay_ms"`
	SchedulerDelaySeconds        int64           `json:"scheduler_delay_seconds"`
	ResultConsumerDelaySeconds   int64           `json:"result_consumer_delay_seconds"`
	MissedChecks                 int64           `json:"missed_checks"`
	CompletedChecks              int64           `json:"completed_checks_24h"`
	FailedChecks                 int64           `json:"failed_checks_24h"`
	NotificationPending          int64           `json:"notification_pending"`
	NotificationOldestAgeSeconds int64           `json:"notification_oldest_age_seconds"`
	Queue                        fifo.Metrics    `json:"job_ledger"`
	Transport                    TransportHealth `json:"sqs"`
	Maintenance                  []Maintenance   `json:"maintenance"`
	Disk                         DiskHealth      `json:"disk"`
}

func (s *Service) Read(ctx context.Context) (Health, error) {
	started := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Health{}, err
	}
	defer tx.Rollback(context.Background())
	h := Health{GeneratedAt: started.UTC(), Maintenance: []Maintenance{}, Disk: readDiskHealth()}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP-min(next_check_at)))::bigint,0) FROM monitors WHERE deleted_at IS NULL AND paused_at IS NULL AND next_check_at<CURRENT_TIMESTAMP`).Scan(&h.SchedulerDelaySeconds); err != nil {
		return h, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP-max(created_at)))::bigint,0) FROM health_checks`).Scan(&h.ResultConsumerDelaySeconds); err != nil {
		return h, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM monitoring_coverage_gaps WHERE scheduled_at>CURRENT_TIMESTAMP-INTERVAL '24 hours'`).Scan(&h.MissedChecks); err != nil {
		return h, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*),count(*)FILTER(WHERE NOT succeeded) FROM health_checks WHERE completed_at>CURRENT_TIMESTAMP-INTERVAL '24 hours'`).Scan(&h.CompletedChecks, &h.FailedChecks); err != nil {
		return h, err
	}
	var oldest *time.Time
	if err = tx.QueryRow(ctx, `SELECT count(*),min(created_at) FROM notification_outbox WHERE state IN('pending','leased')`).Scan(&h.NotificationPending, &oldest); err != nil {
		return h, err
	}
	if oldest != nil {
		h.NotificationOldestAgeSeconds = int64(started.Sub(*oldest).Seconds())
		if h.NotificationOldestAgeSeconds < 0 {
			h.NotificationOldestAgeSeconds = 0
		}
	}
	rows, err := tx.Query(ctx, `SELECT task_name,last_started_at,last_success_at,last_failure_at,last_safe_error,rows_affected FROM maintenance_status ORDER BY task_name`)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var m Maintenance
		if err = rows.Scan(&m.Task, &m.LastStartedAt, &m.LastSuccessAt, &m.LastFailureAt, &m.LastSafeError, &m.RowsAffected); err != nil {
			rows.Close()
			return h, err
		}
		h.Maintenance = append(h.Maintenance, m)
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return h, err
	}
	h.DatabaseDelayMilliseconds = time.Since(started).Milliseconds()
	h.Queue, err = fifo.ReadMetrics(ctx, s.db, started)
	if err != nil {
		return h, err
	}
	h.Transport, err = ReadTransportHealth(ctx, s.sqs, s.queueURLs)
	return h, err
}
func (s *Service) Record(ctx context.Context, task string, started time.Time, count int64, runErr error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	safe := any(nil)
	success, failure := any(nil), any(nil)
	if runErr == nil {
		success = s.now().UTC()
	} else {
		failure = s.now().UTC()
		safe = "maintenance_failed"
	}
	if count < 0 {
		return errors.New("invalid maintenance count")
	}
	tag, err := tx.Exec(ctx, `UPDATE maintenance_status SET last_started_at=$2,last_success_at=COALESCE($3,last_success_at),last_failure_at=COALESCE($4,last_failure_at),last_safe_error=$5,rows_affected=$6,updated_at=CURRENT_TIMESTAMP WHERE task_name=$1`, task, started.UTC(), success, failure, safe, count)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("unknown maintenance task")
	}
	return tx.Commit(ctx)
}
func (s *Service) CleanupExpired(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	var count int64
	queries := []string{
		`DELETE FROM user_action_tokens WHERE expires_at<$1::timestamptz OR used_at<($1::timestamptz-INTERVAL '7 days')`,
		`DELETE FROM org_invitations WHERE expires_at<($1::timestamptz-INTERVAL '7 days') OR accepted_at<($1::timestamptz-INTERVAL '7 days')`,
		`DELETE FROM notification_outbox WHERE state IN('accepted','failed') AND updated_at<($1::timestamptz-INTERVAL '30 days')`,
		`DELETE FROM api_refresh_events WHERE occurred_at<($1::timestamptz-INTERVAL '1 day')`,
	}
	for _, query := range queries {
		tag, e := tx.Exec(ctx, query, now.UTC())
		if e != nil {
			return 0, e
		}
		count += tag.RowsAffected()
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
