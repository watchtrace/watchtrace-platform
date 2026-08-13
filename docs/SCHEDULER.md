# FIFO Durable Scheduler and Publisher

The original PostgreSQL scheduler remains a compatibility path for the first
backend slice. Phase 1.2 production scheduling uses `internal/fifo`: it commits
a stable job ledger row, exact encrypted dispatch body, FIFO identifiers, and
the next stable schedule in one transaction. Amazon SQS is the durable delivery
boundary.

Every monitor has a stably spread `next_check_at` timestamp. The scheduler:

1. Selects a bounded due batch with `FOR UPDATE SKIP LOCKED`.
2. Enforces one outstanding scheduled job per monitor and a 1,000-job global
   admission limit.
3. Freezes the monitor version, request, limits, worker-pool audience,
   network-policy version, key IDs, and two-minute start expiry.
4. Signs the snapshot, encrypts it for the worker pool, and inserts an immutable
   `check_dispatch_outbox` row with both FIFO IDs equal to `job_id`.
5. Advances directly to the first future schedule and commits all changes
   together. Work older than the start window becomes an unknown coverage gap
   instead of an unlimited replay storm.

The publisher sends the exact stored bytes up to three times on its fast retry
schedule and refuses publication after expiry. Ambiguous rows remain available
for operator reconciliation; a valid result repairs missing publish
confirmation. PostgreSQL remains the source of truth for configuration, job
identity, and publication intent, while SQS FIFO owns accepted pending delivery.
