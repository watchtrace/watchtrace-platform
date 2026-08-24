# Reliability reporting and retention

Phase 1.2 reports observed uptime and monitoring coverage separately:

```text
observed uptime = successful scheduled results / observed scheduled results
coverage        = observed scheduled results / expected UTC slots
unknown         = expected UTC slots - observed scheduled results
```

Expected slots come from immutable `monitor_schedule_periods` history. Time
before creation, paused time, and time after effective deletion are excluded.
Interval and worker-pool changes close the previous period and start a new one;
they never rewrite earlier expectations. Manual checks, duplicate deliveries,
conflicts, dead jobs, and expired jobs are not observations.

If a window has no expected checks, both ratios are `no data`. If it has
expected checks but no observations, coverage is zero and observed uptime is
`no data`. Partial buckets use only slots inside the requested UTC interval.

## Ordered state and late results

Scheduled results are evaluated by `(scheduled_at, job_id)`, never arrival
order. Three failed observations produce `down`; two successful observations
recover a down monitor. Unknown slots pause these counters and make the current
display state `unknown` while retaining the last observed state as context.

A result accepted before its job deadline plus ten minutes can recompute the
bounded later sequence. The correction is audited once. Older valid results
repair raw reporting only and cannot rewrite current state. Phase 1.3 uses the
same ordered correction record when it adds incidents and notifications.

## Rollups and cleanup

Accepted results invalidate exactly their UTC hour and day. The maintenance
worker repairs hourly rollups before daily rollups, uses upserts, and persists
a checkpoint so restart catch-up is bounded and repeatable. Retention never
deletes raw data or an hourly rollup while its replacement summary is missing
or invalidated.

- Raw scheduled results and coverage gaps: 7 days.
- Hourly summaries: 90 days.
- Daily summaries: 1 year.

The monitor engine refreshes current buckets, repairs invalidations, advances
closed-bucket checkpoints, refreshes unknown states, and applies retention on
its maintenance cycle.

## Controlled timeout-load baseline

The deterministic local test runs 100 one-second timeouts through the
scheduler, immutable publisher outbox, FIFO job queue, 20 independent worker
journals, FIFO result queue, result consumer, and PostgreSQL ledger. It expects
the execution window to remain below ten seconds (the nominal worker-only
window is about five seconds).

Repeated controlled August 2026 Amazon SQS runs with the approved single
non-production identity completed the same timeout execution window in
10.98–11.33 seconds, or 8.83–9.11 checks/second. Remote SQS
receive/publication latency is part of this result. Until a deployment-specific
ARM64 capacity exercise replaces it, the regression floor for the controlled
remote path is 7.5 checks/second; this is a validation baseline, not a
production capacity commitment.
