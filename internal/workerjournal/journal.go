// Package workerjournal keeps database-free worker replay state in SQLite.
package workerjournal

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

type Journal struct{ db *sql.DB }
type Metrics struct {
	Accepted  int64 `json:"accepted"`
	Completed int64 `json:"completed"`
}

func Open(path string) (*Journal, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS worker_jobs(job_id TEXT PRIMARY KEY,snapshot_hash TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN('accepted','completed')),result BLOB,updated_at INTEGER NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	return &Journal{db}, nil
}
func (j *Journal) Close() error { return j.db.Close() }
func (j *Journal) Accept(ctx context.Context, jobID, hash string) error {
	tag, err := j.db.ExecContext(ctx, `INSERT INTO worker_jobs(job_id,snapshot_hash,state,updated_at) VALUES(?,?,'accepted',?) ON CONFLICT(job_id) DO UPDATE SET updated_at=excluded.updated_at WHERE worker_jobs.snapshot_hash=excluded.snapshot_hash`, jobID, hash, time.Now().UTC().Unix())
	if err != nil {
		return err
	}
	count, err := tag.RowsAffected()
	if err != nil || count != 1 {
		return errors.New("journal snapshot conflict")
	}
	return nil
}
func (j *Journal) StoreResult(ctx context.Context, jobID, hash string, result []byte) error {
	tag, err := j.db.ExecContext(ctx, `UPDATE worker_jobs SET state='completed',result=?,updated_at=? WHERE job_id=? AND snapshot_hash=?`, result, time.Now().UTC().Unix(), jobID, hash)
	if err != nil {
		return err
	}
	count, _ := tag.RowsAffected()
	if count != 1 {
		return errors.New("journal snapshot conflict")
	}
	return nil
}
func (j *Journal) Result(ctx context.Context, jobID, hash string) ([]byte, bool, error) {
	var result []byte
	err := j.db.QueryRowContext(ctx, `SELECT result FROM worker_jobs WHERE job_id=? AND snapshot_hash=? AND state='completed'`, jobID, hash).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return result, err == nil, err
}
func (j *Journal) Cleanup(ctx context.Context, before time.Time) (int64, error) {
	tag, err := j.db.ExecContext(ctx, `DELETE FROM worker_jobs WHERE state='completed' AND updated_at<?`, before.UTC().Unix())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected()
}
func (j *Journal) Metrics(ctx context.Context) (Metrics, error) {
	var metrics Metrics
	err := j.db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state='accepted'),count(*) FILTER(WHERE state='completed') FROM worker_jobs`).Scan(&metrics.Accepted, &metrics.Completed)
	return metrics, err
}
