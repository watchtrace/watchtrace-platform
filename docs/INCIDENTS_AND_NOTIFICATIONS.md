# Incidents and durable notifications

Phase 1.3 evaluates scheduled observations in their durable
`(scheduled_at, job_id)` order. The default alert rule opens one incident after
three consecutive failed observations and resolves it after two consecutive
successful observations. Alert-rule thresholds are stored per monitor.
Unknown expected slots pause the counters, and manual checks never participate.

The accepted-result transaction locks monitor state and then:

1. updates ordered monitor evaluation state;
2. opens or resolves at most one incident for the monitor and rule;
3. appends one stable incident timeline event; and
4. inserts one notification-outbox row per eligible recipient.

The partial `incidents_one_open_rule_idx` index is the final concurrency guard.
Event keys and the outbox uniqueness constraint make result replay and bounded
late correction idempotent. A late correction inside the job deadline plus ten
minutes may open a newly proven incident or resolve an invalidated open incident
with a `late_result_correction` event. Existing notification attempts are never
deleted or silently resent.

Acknowledgement records the member and time but leaves the incident open.
Manual resolution records its member and optional bounded reason without
pausing or changing monitor evaluation. Owners, admins, and members may perform
these actions; viewers may read incidents but cannot change them. Phase 1.4
exposes these service actions through the public API.

## Recipient selection

Each transition snapshots only users who are current members of the incident's
organization, have a verified email, and have
`incident_notifications_enabled=true`. Selection happens inside the incident
transaction. Cross-organization members, unverified users, and opted-out users
receive no work.

## Delivery states and retries

`notification-worker` claims one due row using `FOR UPDATE SKIP LOCKED`, commits
a short lease, and calls the provider outside the database transaction. An
expired lease returns to `pending`, so an application restart cannot lose it.
Every provider attempt is recorded with a bounded safe status.

```text
attempt 1: immediately
attempt 2: 1 minute after failure
attempt 3: 5 minutes after failure
attempt 4: 25 minutes after failure, then failed if rejected
```

`accepted` means accepted by the configured provider, not delivered to the
recipient. A crash after provider acceptance but before the database commit can
produce a rare duplicate. Every message includes stable incident and delivery
IDs so the duplicate is recognizable.

## Provider configuration

Local development uses the loopback-only plaintext SMTP adapter and Mailpit:

```text
WATCHTRACE_NOTIFICATION_PROVIDER=local
WATCHTRACE_NOTIFICATION_SMTP_ADDRESS=127.0.0.1:1025
WATCHTRACE_NOTIFICATION_FROM=watchtrace@localhost
```

OCI Email Delivery uses authenticated STARTTLS SMTP:

```text
WATCHTRACE_NOTIFICATION_PROVIDER=oci
WATCHTRACE_NOTIFICATION_SMTP_ADDRESS=<regional OCI SMTP endpoint>:587
WATCHTRACE_NOTIFICATION_FROM=<approved sender>
WATCHTRACE_NOTIFICATION_SMTP_USERNAME=<injected secret>
WATCHTRACE_NOTIFICATION_SMTP_PASSWORD=<injected secret>
```

The username and password are read only from process configuration. They are
never stored in PostgreSQL, messages, logs, documentation values, or source.
Run the worker with `go run ./cmd/notification-worker` after migration 13 is
applied.
