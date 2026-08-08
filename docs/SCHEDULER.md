# Initial Durable Scheduler

P1-106 adds the first PostgreSQL-backed scheduler path. It does not claim or
execute jobs; worker leasing and HTTP result storage begin in P1-107.

## Database source of truth

Every monitor has a `next_check_at` timestamp. New monitors are immediately
due by default. The scheduler runs a bounded transaction that:

1. Selects at most the requested number of due monitors in planned-time order.
2. Locks those monitor rows with `FOR UPDATE SKIP LOCKED`.
3. Inserts one `scheduled`/`pending` `check_jobs` row for each planned time.
4. Advances each monitor from its planned time to its first future interval.
5. Commits the job insertions and schedule advances together.

The default batch size is 20 and the hard per-call maximum is 100. PostgreSQL
remains the only queue source of truth; no process-local channel owns work.

## Initial queue guarantees

- Job IDs are database-generated UUIDs.
- `scheduled_at`, `started_at`, and `completed_at` are separate timezone-aware
  columns. New jobs leave the worker lifecycle timestamps null.
- `(monitor_id, scheduled_at)` is unique, making scheduling for one planned
  time idempotent.
- A partial unique index permits at most one pending or running scheduled job
  per monitor.
- Every job carries organization, environment, and monitor IDs connected by a
  composite foreign key, preventing cross-tenant queue rows.
- An overdue monitor creates one job for its recorded planned time and advances
  directly to the first future interval instead of replaying every missed slot.
- A failure while advancing a schedule rolls back the job insertion as part of
  the same transaction.

The initial table recognizes the Phase 1 job types and states, but P1-106 only
creates scheduled pending jobs. Attempts, priorities, leases, stale-monitor
versions, hard global queue limits, cleanup, and operational metrics are added
by their later durable-engine tasks.
