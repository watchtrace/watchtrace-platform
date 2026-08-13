# Bounded Modular HTTP Check Worker

Phase 1.2 production execution uses the database-free `cmd/worker` binary. It
accepts the same signed-and-encrypted job through direct Amazon SQS or the
stateless HTTPS gateway, and keeps completed signed results in a durable SQLite
journal. The original PostgreSQL worker remains only as the verified initial
compatibility slice.

The shared `internal/checkengine` supports GET and HEAD, a one-to-ten-second
total timeout, bounded response headers and body reads, at most three redirects,
and no response-body persistence. It sends `WatchTrace-Phase1/1.0` and
`X-WatchTrace-Job-ID`, records available DNS/connect/TLS/first-byte/total
timings, and emits fixed safe categories for target failures.

Every resolved IPv4/IPv6 address is checked immediately before dialing. Hosted
workers allow public destinations only. Customer-VPC workers can additionally
reach private addresses solely inside their local protected CIDR allowlist;
jobs cannot widen it. Metadata, loopback, link-local, multicast, alternate
address encodings, unsafe redirects, and DNS rebinding escapes remain blocked.
All custom headers are stripped when a redirect changes host.

Workers verify the exact outer attributes, decrypt the pool envelope, verify
the platform signature, and reject expired or mismatched jobs. Results are
signed and durably published before the job is acknowledged. Same-worker
redelivery republishes a journaled result without repeating the target request.
The central consumer validates identity, pool, snapshot, signature, and time
bounds before storing at most one result per job; conflicting valid results are
quarantined. Stored data never includes target response bodies or plaintext
monitor-header values.
