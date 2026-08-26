# Risks, Caveats, and Constraints

- **Companion to:** `DESIGN_SPECIFICATION.md`
- **Purpose:** Record what can go wrong, what the product cannot promise yet, and what must be tested before each release
- **Last updated:** 2026-08-27

---

## 1. How to Use This Document

This project combines uptime monitoring, distributed tracing, metrics, logs, incident management, and eventually billing. That is a large product, even though each part may look small by itself.

This document is not a reason to stop building. It helps us build in a safe order and prevents us from making promises that the system cannot yet keep.

Risk levels:

| Level | Meaning |
|---|---|
| Critical | Must be addressed before public use |
| High | Must have a tested control and visible monitoring |
| Medium | Can be accepted temporarily if documented |
| Low | Track and improve when usage justifies it |

Every major risk should have:

- an owner;
- a way to detect it;
- a prevention or recovery plan; and
- a release phase in which it must be solved.

---

## 2. Most Important Caveats

These are the issues most likely to affect the project.

1. Oracle Free Tier is suitable for private personal development, not a public beta or startup-scale production.
2. A single VM and database are a single point of failure.
3. Monitoring results can be wrong or incomplete if our own scheduler is unhealthy.
4. User-provided URLs create serious server-side request forgery and abuse risks.
5. Trace and log data can accidentally contain passwords, tokens, personal data, and database statements.
6. Distributed traces are often incomplete because of sampling, missing instrumentation, or broken trace context.
7. Telemetry data grows much faster than ordinary business data.
8. Email acceptance does not guarantee delivery to a mailbox.
9. Amazon SQS FIFO reduces duplicate publication only within a five-minute deduplication window; it does not make an external HTTP check globally exactly once.
10. A database-free customer worker requires self-contained encrypted jobs, signed results, isolated routing, careful key rotation, and a local journal.
11. Splitting into microservices adds network failures and data-consistency problems.
12. A technically successful product may still have poor business economics because telemetry storage and support are expensive.
13. Result publication must deduplicate by result attempt, not only by job, or one bad attempt can hide a later valid result.
14. Late results, database outages, and partial worker-pool provisioning require explicit reconciliation; stale-backup reconciliation becomes mandatory with P4-701 in Phase 4.

---

## 3. Product Scope and Delivery Risks

### R-001: The project is several products combined

- **Level:** High
- **Affects:** All phases

Uptime monitoring, distributed tracing, metrics, logs, status pages, incident response, and billing are each substantial features. Trying to build all of them together will delay the first usable release.

**Control:** Follow the phase order and completion rules. Do not start the next phase because the current phase feels less exciting. Start Phase 2 only after Phase 1 works end to end.

### R-002: Phase estimates will be uncertain

- **Level:** Medium
- **Affects:** All phases

Security, UI work, deployment, testing, and operational tools often take longer than the main feature code.

**Control:** Plan work in small vertical slices. Review scope every two weeks. Cut optional features before reducing security or any work required by the current phase gate. Do not admit external clients before Phase 4 security and recovery work passes.

### R-003: Premature infrastructure can replace product progress

- **Level:** High
- **Affects:** Phases 1–3

Kafka, Kubernetes, ClickHouse clusters, service meshes, and many microservices can consume months without improving the customer experience.

**Control:** Introduce an infrastructure component only when a recorded capacity, reliability, or team-ownership problem requires it.

### R-004: Architecture documents can become outdated

- **Level:** Medium
- **Affects:** All phases

Implementation decisions will change as tests reveal new information.

**Control:** Keep this document and the main specification version-controlled. Record important changes as short architecture decision records. Code, migrations, API contracts, and operating instructions become the final authority for implemented behavior.

---

## 4. Oracle Free Tier Caveats

### R-005: Free resources are small and can change

- **Level:** Critical for capacity promises
- **Affects:** Phases 1–2

Oracle currently documents a total Always Free Ampere allowance of 2 OCPUs and 12 GB memory, with limited block and object storage. Availability and limits must be verified in the actual tenancy before deployment. [OCI Free Tier documentation](https://docs.oracle.com/iaas/Content/FreeTier/freetier.htm)

**Control:** Put every limit in configuration. Run ARM64 load tests. Do not advertise the design target until the real deployment passes it.

### R-006: A free compute instance may be unavailable or reclaimed

- **Level:** High
- **Affects:** Phases 1–2

Always Free capacity may be temporarily unavailable during provisioning. Oracle also documents conditions under which idle free compute may be reclaimed. [OCI Always Free resources](https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm)

**Control:** Keep reproducible infrastructure and deployment instructions so the disposable environment can be recreated. Accept total test-data loss, store no external-client or irreplaceable data, and do not offer an availability agreement from this deployment. P4-701 backups are required before customer use.

### R-007: One VM is one failure point

- **Level:** High
- **Affects:** Phases 1–2

VM failure, operating-system failure, full disk, Docker failure, PostgreSQL failure, or maintenance can stop the API, scheduling, hosted workers, traces, and notifications together. SQS retains published jobs and results independently. A database-free worker may continue executing queued jobs during a database outage, while the result FIFO safely buffers its results until the central consumer recovers.

**Control:** Use readiness monitoring, external health checks, and automatic container restart. During Phases 1–3, accept total data loss and a full rebuild/reset because the environment is private and disposable. Phase 4 must add verified backups, restore procedures, and appropriate high availability before external clients are admitted.

### R-008: ARM64 compatibility can cause deployment problems

- **Level:** Medium
- **Affects:** Phases 1–2

Some container images, native libraries, browser-test tools, or monitoring agents may not provide ARM64 builds.

**Control:** Build and test `linux/arm64` images in CI from the beginning. Do not add a dependency until its ARM64 path has been verified.

### R-009: Disk growth can stop the entire platform

- **Level:** Critical
- **Affects:** Phases 1–2

PostgreSQL raw checks, job-ledger and immutable outbox rows, indexes, write-ahead logs, traces, worker SQLite journals, temporary files, and Docker logs share limited storage. A full disk can stop dispatch recording, result commits, and new writes even while SQS still holds published messages.

**Control:** Set hard ingestion limits, short retention, disk alerts at 60%, 75%, and 85%, partition deletion, bounded Docker logs, and an emergency switch that rejects telemetry before control data is threatened.

### R-010: A domain name may still cost money

- **Level:** Low
- **Affects:** Phase 1

The cloud resources may be free, but a stable public domain usually requires registration. HTTPS and email sending are easier with a domain the project controls.

**Control:** Treat domain registration as a small business/project cost or use an existing owned domain. Do not weaken HTTPS to avoid this cost.

---

## 5. Uptime Monitoring Caveats

### R-011: Polling is not truly continuous

- **Level:** Medium
- **Affects:** All phases

A check every 60 seconds can miss an outage that begins and ends between two checks. Three required failures can take approximately two to three minutes to confirm.

**Control:** Show the expected detection time in the UI. Do not describe one-minute polling as instant detection.

### R-012: One region gives only one network view

- **Level:** High
- **Affects:** Phases 1–2

An endpoint may work from Oracle’s region while failing for users elsewhere, or it may fail only from Oracle’s network.

**Control:** Label the probe location clearly. Add multi-region workers in Phase 3. Do not claim global availability based on one probe region.

### R-013: The monitored server may block our probe IP

- **Level:** Medium
- **Affects:** All phases

Rate limiting, bot protection, firewalls, or an IP blocklist can reject monitoring traffic while normal users still succeed.

**Control:** Use a clear User-Agent, publish probe IPs when stable, allow customer headers, rate-limit checks, and show the resolved IP and error category.

### R-014: GET requests can still have side effects

- **Level:** Medium
- **Affects:** Phase 1 onward

Although GET should be safe, poorly designed APIs may change data or trigger expensive work when called.

**Control:** Require customers to confirm that they are authorized to monitor the endpoint. Keep check methods to GET and HEAD initially. Add acceptable-use terms before public signup.

### R-015: Network timing is not perfectly comparable

- **Level:** Medium
- **Affects:** All phases

DNS caching, connection reuse, TLS reuse, network congestion, and probe location affect timing. DNS or TLS timing may be absent when a connection is reused.

**Control:** Store phase timings as optional, record total time consistently, display probe location, and compare trends from the same location rather than treating one measurement as absolute truth.

### R-016: False alerts and missed alerts are unavoidable

- **Level:** High
- **Affects:** All phases

Transient network errors may create false alerts. Large failure thresholds may delay real alerts. A broken scheduler may produce no result at all.

**Control:** Use failure and recovery thresholds, multi-region confirmation later, coverage reporting, scheduler heartbeat alerts, and clear `unknown` status.

### R-017: Uptime without coverage is misleading

- **Level:** Critical for reporting
- **Affects:** All phases

If 60 checks were expected but only 10 were recorded, nine successes do not prove the service had 90% availability for the full period.

**Control:** Always show observed uptime together with coverage and unknown checks. Never convert missing checks into successes.

### R-018: Slow targets can exhaust workers

- **Level:** High
- **Affects:** All phases

A broad outage may cause every request to use its full timeout. One hundred workers with ten-second timeouts can finish only about ten checks per second during that event.

**Control:** Bound worker count and durable queue depth, allow only one outstanding scheduled job per monitor, cap timeouts, measure oldest-job age, skip unlimited backlog replay, and test a full timeout storm.

---

## 6. URL Security and Abuse Caveats

### R-019: User URLs can target internal services

- **Level:** Critical
- **Affects:** Phase 1 onward

An attacker may try to use the checker to call localhost, private networks, OCI or AWS instance metadata, or another protected system. Simple URL validation is not sufficient.

**Control:** Hosted workers validate scheme, port, every IPv4/IPv6 result, actual dialed address, and every redirect, and block private, loopback, link-local, multicast, metadata, and special-use destinations. A customer-VPC worker may allow only explicit CIDRs from its protected local configuration; a queue message can never widen that allowlist. It still blocks metadata, loopback, link-local, multicast, and unexpected redirects. Test DNS rebinding and allowlist escape in both modes.

### R-020: The platform can be used to attack third parties

- **Level:** Critical
- **Affects:** Phase 4 external-client use onward

An attacker could create many monitors targeting a victim, turning the platform into a traffic source.

**Control:** Require verified accounts, enforce organization and destination limits, keep a clear User-Agent, log destination changes, suspend abuse, and consider endpoint ownership verification for higher rates.

### R-021: Redirect rules can leak secrets

- **Level:** Critical
- **Affects:** Phase 1 onward

An endpoint could redirect a check to another host and receive the original Authorization header.

**Control:** Strip sensitive headers when the host changes. Revalidate every redirect. Limit the number of redirects.

### R-022: Response bodies can consume memory or expose data

- **Level:** High
- **Affects:** Phase 1 onward

A server can return an unlimited or sensitive response.

**Control:** Stream and discard the body after a small byte limit. Do not store response bodies in Phase 1. Never log them by default.

---

## 7. Distributed Tracing Caveats

### R-023: Tracing requires correct customer instrumentation

- **Level:** High
- **Affects:** Phase 2 onward

The platform cannot create a complete distributed trace if applications do not create spans or pass trace context to the next service.

**Control:** Provide tested examples, an instrumentation diagnostics page, and warnings for missing parent spans or missing `service.name`.

### R-024: Sampling hides some requests

- **Level:** High
- **Affects:** Phase 2 onward

Keeping 10% of requests means most individual requests are intentionally absent. Client-side sampling may discard an error before the platform knows it occurred.

**Control:** Show the sampling configuration and estimated sampled rate. Recommend customer-side Collector policies that retain more slow and failed traces. Never claim that sampled trace counts equal total traffic.

### R-025: Traces arrive late, duplicated, incomplete, or out of order

- **Level:** High
- **Affects:** Phase 2 onward

Exporters retry batches, services finish at different times, and one service may be unavailable. A trace may not arrive in a single request.

**Control:** Make spans idempotent by environment, trace ID, and span ID. Accept out-of-order spans. Finalize summaries after a quiet period and mark incomplete traces clearly.

### R-026: Service clocks may disagree

- **Level:** Medium
- **Affects:** Phase 2 onward

Clock differences can make a child span appear to start before its parent or distort the waterfall.

**Control:** Encourage time synchronization, preserve original timestamps, avoid silently rewriting data, and show a clock-skew warning when relationships are impossible.

### R-027: Trace data may contain secrets or personal data

- **Level:** Critical
- **Affects:** Phase 2 onward

URLs, database statements, HTTP headers, errors, events, and custom attributes may contain credentials or personal information.

**Control:** Apply a blocked-key list, size limits, customer filters, retention, deletion, and encryption. Encourage filtering before export. Never store raw Authorization, Cookie, or database parameter values by default.

### R-028: Attribute variety can create uncontrolled cost

- **Level:** Critical for scale
- **Affects:** Phase 2 onward

Attributes such as user ID, request ID, or full URL can create millions of different values. Indexing all of them can exhaust storage and memory.

**Control:** Limit keys and values, index only an allowlist, store selected extra attributes without indexing, measure per-project usage, and reject oversized payloads.

### R-029: PostgreSQL is temporary trace storage

- **Level:** High
- **Affects:** Phase 2

PostgreSQL can support the small trace pilot but will become expensive and slow for large analytical searches.

**Control:** Keep Phase 2 limits small, partition by time, batch inserts, retain only three days, and design a tested ClickHouse migration before accepting startup-scale traffic.

### R-030: The OpenTelemetry Collector also needs operations

- **Level:** Medium
- **Affects:** Phase 2 onward

Collector components do not all have the same maturity. Configuration errors can drop, duplicate, or expose telemetry. OpenTelemetry documents mixed component stability. [OpenTelemetry Collector documentation](https://opentelemetry.io/docs/collector/)

**Control:** Use stable components, pin versions, test upgrades, expose Collector health metrics, and keep configurations version-controlled.

---

## 8. Alert and Notification Caveats

### R-031: Email sent does not mean email delivered

- **Level:** High
- **Affects:** All phases

Our SMTP provider may accept a message that is later filtered, bounced, delayed, or placed in spam.

**Control:** Describe the state as “accepted by provider,” configure SPF and DKIM, process suppressions where available, monitor bounces, and provide additional channels in Phase 3.

### R-032: Free email limits can be exhausted

- **Level:** High
- **Affects:** Phases 1–2

Incident storms, test emails, and many recipients can consume the monthly allowance.

**Control:** Deduplicate, rate-limit test mail, limit recipients, notify only on trigger and recovery initially, track monthly use, and stop nonessential email before the hard limit.

### R-033: Exactly-once email is not possible with ordinary SMTP

- **Level:** Medium
- **Affects:** All phases

The process may crash after SMTP accepts an email but before PostgreSQL records success, causing a retry and duplicate.

**Control:** Create a stable notification-outbox row in the incident transaction, lease and retry it idempotently, include a stable incident and delivery ID, and document rare duplicate delivery.

### R-034: Alert fatigue makes the product less useful

- **Level:** High
- **Affects:** All phases

Too many alerts cause users to ignore important ones.

**Control:** Use minimum sample counts, failure/recovery thresholds, grouping, cooldowns, maintenance windows, quiet periods, and per-rule history showing why an alert fired.

---

## 9. Database and Data Lifecycle Caveats

### R-035: Partitions require active maintenance

- **Level:** High
- **Affects:** Phases 1–2

If the next time partition does not exist, inserts can fail or accumulate in a default partition. Creating a new partition may later fail if overlapping rows remain there.

**Control:** Create partitions ahead of time, alert on default-partition rows, test month/week boundaries, and move unexpected rows before attaching a replacement partition.

### R-036: Deleting raw data can break summaries

- **Level:** High
- **Affects:** All phases

If retention removes raw rows before rollups complete, long-term uptime and latency data will be incomplete.

**Control:** Make rollups repeatable, record completion checkpoints, verify summaries before dropping partitions, and alert on rollup delay.

### R-037: Backups do not provide high availability

- **Level:** High
- **Affects:** Phase 4 customer production

Phases 1 through 3 deliberately provide no backup or recovery promise: their data is disposable and no external clients may use them. In Phase 4, a backup helps recover data, but restoration still takes time and recent data after the recovery point may be lost.

**Control:** Implement P4-701 before external-client use. Publish recovery-point and recovery-time targets separately from availability, automate and alert on backups, run clean-environment and older-database/newer-queue restore tests, and add replication or continuous backup where the measured target requires it. Optional earlier snapshots do not satisfy this control.

### R-038: Schema changes can interrupt ingestion

- **Level:** High
- **Affects:** Phase 2 onward

Large table changes or indexes can lock writes for a long time.

**Control:** Use backward-compatible expand-and-contract migrations, build large indexes without blocking writes where supported, test migrations on production-sized copies, and separate schema deployment from application rollout.

### R-039: Moving to ClickHouse is not a simple switch

- **Level:** High
- **Affects:** Phase 3

PostgreSQL and ClickHouse have different query, update, uniqueness, and retention behavior. Dual writing can create mismatched results.

**Control:** Define one owner for every dataset, backfill historical data, compare counts and sampled queries, run shadow reads, keep rollback, and switch one query group at a time.

## 10. Microservice and Scaling Caveats

### R-040: More services create more failure modes

- **Level:** High
- **Affects:** Phase 3 onward

Network timeouts, retries, version differences, partial deployment, and unavailable dependencies replace simple in-process calls.

**Control:** Begin Phase 3 with four logical service areas and a small number of deployable applications. Split only for independent scale, reliability, data ownership, or team ownership.

### R-041: Shared tables create hidden service coupling

- **Level:** High
- **Affects:** Phase 3 onward

If one service reads another service’s tables, a schema change can break it without an API change.

**Control:** Assign every table to one logical service. Use separate schemas/users initially and APIs or versioned events between services. Move to separate database instances only when justified.

### R-042: Events introduce delayed consistency

- **Level:** High
- **Affects:** Phase 3 onward

A check may be stored before an incident appears, or an incident may be visible before its notification status updates.

**Control:** Use durable outboxes, idempotent consumers, event IDs, retries, dead-letter handling, and UI states such as “processing.” Do not promise immediate cross-service transactions.

### R-043: A broker can move rather than remove the bottleneck

- **Level:** High
- **Affects:** Phase 3 onward

Kafka or another broker can accumulate more data than processors or databases can consume.

**Control:** Bound retention and queue size, monitor oldest-message age, apply customer backpressure, prioritize control/incident events, and test recovery from a full backlog.

### R-044: Multi-region checks increase cost and complexity

- **Level:** Medium
- **Affects:** Phase 3 onward

Regional failures, different DNS answers, network routes, and duplicate results complicate incident rules.

**Control:** Give every result a region, define quorum rules clearly, show regional disagreement, and test the loss of one region.

### R-045: Kubernetes may be unnecessary

- **Level:** Medium
- **Affects:** Phases 3–4

A cluster adds networking, upgrades, access control, and operational work.

**Control:** Use managed containers or simple VMs until automatic scheduling and scaling of many instances clearly justify Kubernetes.

---

## 11. API and User Experience Caveats

### R-046: Live events can be lost

- **Level:** Medium
- **Affects:** Phase 1 onward

Server-Sent Events can disconnect, and in-memory events can disappear during restart.

**Control:** Treat live events as a prompt to reload data, not the source of truth. Use reconnect behavior and polling fallback.

### R-047: Large time-range queries can overload storage

- **Level:** High
- **Affects:** All phases

An unbounded dashboard query can scan millions or billions of records.

**Control:** Require time ranges, choose raw/hourly/daily data by range, enforce maximum result size, use cursors, cancel timed-out queries, and cache public status data.

### R-048: API changes can break customers and Collectors

- **Level:** High
- **Affects:** Phase 2 onward

Customer automation and telemetry exporters may depend on previous behavior.

**Control:** Version management APIs, follow OTLP compatibility rules, publish change notices, keep a deprecation window, and test old clients against new deployments.

### R-049: Complex status labels can confuse users

- **Level:** Medium
- **Affects:** All phases

Healthy, degraded, down, unknown, and paused must have consistent meanings.

**Control:** Define status in one shared domain package and explain unknown coverage in the UI. Avoid calculating state separately in several frontend components.

---

## 12. Privacy, Legal, and Abuse Caveats

### R-050: The platform will store customer operational data

- **Level:** Critical before public use
- **Affects:** All phases

Monitor URLs, headers, traces, logs, user emails, IP addresses, and incident notes may be confidential or personal data.

**Control:** Publish a privacy policy and retention rules, minimize collection, encrypt secrets, support deletion/export, restrict staff access, and keep an access audit. Obtain professional legal advice before commercial launch.

### R-051: Customers must be authorized to monitor targets

- **Level:** Critical before public use
- **Affects:** All phases

Monitoring infrastructure that a user does not own or operate may violate agreements or be treated as abusive traffic.

**Control:** Add acceptable-use terms, require authorization confirmation, respond to abuse reports, keep destination audit data, and suspend repeated abuse.

### R-052: Data location promises may be impossible initially

- **Level:** Medium
- **Affects:** Phase 4 customer production

The private Phases 1–3 deployment uses one OCI home region and admits no external clients. Phase 4 customers cannot be given a data-location promise until the production storage, deletion, and backup locations are known and tested.

**Control:** State the storage region clearly. Offer data-region selection only after regional infrastructure and deletion/backup behavior are proven.

### R-053: Open-source licenses require tracking

- **Level:** Medium
- **Affects:** All phases

Dependencies, container images, UI packages, and databases have licenses and notices. Some licenses may not suit a hosted commercial product or distributed agent.

**Control:** Generate a dependency inventory, scan licenses in CI, keep required notices, and review agent/client distribution licenses before release.

### R-054: Security assurance takes ongoing work

- **Level:** High
- **Affects:** Startup launch onward

Passing one security review does not make the product permanently secure.

**Control:** Patch dependencies and operating systems, rotate secrets, scan images, review access, handle vulnerability reports, and maintain an incident-response process. Begin mandatory backup and restore testing with P4-701 in Phase 4 before external-client use.

---

## 13. Commercial and Startup Caveats

### R-055: The market is highly competitive

- **Level:** High business risk
- **Affects:** Phases 2–4

Many mature monitoring products already provide uptime, tracing, logs, and incidents.

**Control:** Validate a focused customer problem, such as simple combined uptime and trace investigation for small teams. Measure activation and retention before expanding the feature list.

### R-056: Telemetry can cost more than customers pay

- **Level:** Critical for the business
- **Affects:** Phases 3–4

Storage, network transfer, indexing, replicas, and queries can create high costs. A customer can produce large data volume without many users.

**Control:** Measure cost per million checks/spans/log bytes, enforce plan limits, use sampling and retention, meter rejected data correctly, and design pricing from measured cost.

### R-057: Monitoring customers creates on-call expectations for us

- **Level:** High
- **Affects:** Commercial launch onward

Customers depend on the monitoring platform during their own outages. Our outage may occur at the worst possible time.

**Control:** Establish support hours first, create our own on-call process as revenue grows, publish platform status, and avoid a strong SLA before the team can support it.

### R-058: Billing mistakes damage trust

- **Level:** Critical in Phase 4
- **Affects:** Phase 4

Retries, duplicates, dropped data, and late events can produce incorrect usage totals.

**Control:** Use stable event IDs, idempotent usage records, daily reconciliation, customer-visible usage, billing tests, and a correction process before charging automatically.

### R-059: An SLA creates financial and legal responsibility

- **Level:** Critical in Phase 4
- **Affects:** Phase 4

An availability promise needs precise measurement, exclusions, support commitments, and service credits.

**Control:** Offer an SLA only after months of measured reliability, failure exercises, legal review, and a clear way to calculate credits.

## 14. Durable Queue Caveats

### R-060: FIFO deduplication lasts only five minutes

- **Level:** High
- **Affects:** Phase 1 onward

`MessageDeduplicationId = job_id` prevents a second enqueued copy only when the retry reaches the same FIFO queue within SQS's five-minute deduplication window. SQS accepts same-ID retries during that window without delivering them again, even if the original was already received or deleted. A retry after the window may create another delivery.

**Control:** Store the exact immutable body, deduplication ID, group ID, and destination in the outbox; make the three fast job-publication retries within 30 seconds; never automatically publish a job after its two-minute start expiry; retain worker-journal and result-consumer idempotency for visibility redelivery, result retry, and controlled redrive; and test both in-window suppression and forced late duplicates. Do not describe this as global exactly-once execution.

### R-061: PostgreSQL and SQS cannot commit atomically

- **Level:** High
- **Affects:** Phases 1–2

The scheduler cannot atomically commit PostgreSQL and `SendMessage`. If SQS accepts a job but the publisher crashes before updating PostgreSQL, the outbox still appears unpublished. A lost response has the same ambiguity.

**Control:** Commit the stable job, immutable encrypted message, publication identifiers, and schedule advance in one PostgreSQL transaction. Publish asynchronously and retry the identical FIFO message three times within 30 seconds. Let a valid result repair missing dispatch confirmation. Retain uncertain rows until the job-start expiry, then mark the slot unknown instead of publishing stale work. Test rollback, accepted-send crash, lost response, SQS outage, result-based repair, and post-expiry refusal.

### R-062: A target can still receive a duplicate check

- **Level:** High
- **Affects:** Phase 1 onward

A worker can crash after the target responds but before the local journal stores the result, or a different worker can receive a later duplicate. No SQS setting can atomically combine a third-party HTTP request with the worker journal or result queue.

**Control:** Permit only GET and HEAD, include `X-WatchTrace-Job-ID`, use a durable local journal, publish the result before deleting the job, accept one valid database result per job ID, document the remaining duplicate window, and test crashes at every execution boundary. Side-effecting synthetic methods require a later protocol.

### R-063: Visibility expiry and poison message groups

- **Level:** High
- **Affects:** Phase 1 onward

A pause, overloaded worker, network interruption, corrupt envelope, or engine bug can leave a job unacknowledged. Visibility expiry causes redelivery. FIFO also blocks later messages in the same group while one message is in flight.

**Control:** Keep the ten-second request timeout safely below the 90-second job visibility timeout, receive only available capacity, use `MessageGroupId = job_id` so one poison job does not block later monitor jobs, use a five-receive job DLQ rule, distinguish target outcomes from internal failures, expose DLQ movement in operator health views, and provide controlled redrive after repair. Automated DLQ alarms begin in Phase 4.

### R-064: A durable backlog can grow invisibly

- **Level:** Critical for availability
- **Affects:** Phase 1 onward

FIFO queues prevent in-memory loss but can hide overload while job messages, result messages, in-flight work, immutable outbox rows, and PostgreSQL ledger rows grow. Retention can expire unprocessed data.

**Control:** Enforce one outstanding scheduled job per monitor and global admission limits; monitor both FIFO queues, both DLQs, oldest-message age, in-flight count, outbox age, and result-consumer delay; skip unlimited overdue replay; configure retention; and count unrecoverable scheduled work as unknown coverage.

### R-065: The local worker journal is not a shared lock

- **Level:** Medium
- **Affects:** Phase 1 onward

SQLite prevents repeated execution only when the same worker instance retains the same disk. Replacing the container without its volume, moving the job to another worker, or corrupting the journal removes that protection. A journal can also grow or expose target metadata.

**Control:** Mount a durable restricted volume, store only bounded job IDs, hashes, states, and signed results, encrypt the volume where practical, apply retention, expose journal/disk health for manual review, treat journal loss as degraded protection rather than data loss, and keep database uniqueness as the final storage guard. Automated disk alarms begin in Phase 4.

### R-066: Queue credentials, lease tokens, or cryptographic keys can leak

- **Level:** Critical
- **Affects:** Phase 1 onward

AWS credentials, SQS receipt handles, HTTPS lease tokens, worker credentials, worker private keys, or platform signing keys can allow message theft, forged results, or decryption of targets and headers.

**Control:** Phase 1.2 local and controlled AWS validation may use one explicitly non-production IAM identity, but it must not carry production data or be presented as production isolation. Customer-VPC workers still use the HTTPS gateway with mTLS and receive no AWS credentials. Short-lived authenticated-encrypted lease tokens, separate keys per worker pool and purpose, protected key files, rotation and revocation, secret scanning, and strict redaction remain Phase 1 controls. Phase 4 creates separate environment-scoped publisher, hosted-worker, queue-gateway, result-consumer, DLQ-reconciler, and infrastructure-operator roles; restricts temporary workload trust to the WatchTrace account, environment, trust anchor/profile, exact queue ARNs, and required actions; verifies role/trust/policy fingerprints; and runs production security exercises. Never expose raw receipt handles or place private keys in logs, PostgreSQL plaintext, images, issues, or chat.

### R-067: Cross-cloud SQS dependency can stop dispatch or increase cost

- **Level:** High
- **Affects:** Phases 1–2

The initial control plane runs on OCI while SQS runs in AWS. Internet routing, DNS, TLS, either provider, AWS quotas, or billing controls can interrupt job publication, worker pulls, result publication, or result consumption. SQS requests and cross-cloud transfer create Phase 1 costs; Phase 4 CloudWatch alarms, SNS delivery, and optional customer-managed KMS add further costs.

**Control:** Use one explicit AWS Region per environment and its regional HTTPS endpoint; the reference Phase 1 Region is `ap-south-1`, with any override recorded before provisioning. Use bounded SDK retries, visible outbox and both-queue age metrics, manual budget review, tags, explicit quotas, minimal bounded payloads, and a cross-cloud outage runbook. Verify that job intent remains in PostgreSQL and accepted results remain in SQS during either-side outages. Phase 4 adds numeric CloudWatch thresholds, AWS cost alarms, and SNS notification delivery.

### R-068: Retention or DLQ configuration can lose visible work

- **Level:** Critical for coverage
- **Affects:** Phase 1 onward

Jobs or results can expire, move to the wrong DLQ, or become inaccessible through a bad queue policy. Treating a result DLQ entry as a target failure would also be incorrect because the result may be valid but not yet stored.

**Control:** Follow the `watchtrace-{environment}-...fifo` naming contract, record queue attributes and policies in the versioned deployment manifest, require FIFO DLQs for FIFO sources, keep DLQ retention longer than source retention, restrict redrive sources, reconcile ledger state with job/result DLQs, expose retention age for manual review, and count only unrecoverable job work as unknown. Quarantine and redrive recoverable results; automated retention alarms begin in Phase 4.

### R-069: Self-contained job messages carry sensitive data

- **Level:** Critical
- **Affects:** Phase 1 onward

A database-free worker needs the target URL and optional request headers in its job. SQS server-side encryption protects storage but does not by itself prevent a worker, gateway, or leaked queue credential from reading a message.

**Control:** Sign then encrypt each immutable job for the assigned worker-pool public key and enable SSE-SQS on every Phase 1 source queue and DLQ as a second layer. Keep application messages below 64 KiB; bind schema, audience, job ID, expiry, and snapshot hash; never log decrypted envelopes; and test wrong-pool, tampered, expired, and rotated-key cases. Customer-managed KMS is deferred to Phase 4.

### R-070: A forged or conflicting result could corrupt monitoring state

- **Level:** Critical
- **Affects:** Phase 1 onward

A compromised worker could report invented status, another pool could submit a result, or two attempts could return different outcomes. Blindly accepting the first message by job ID could open or close an incident incorrectly.

**Control:** Sign results with a registered worker-pool key; authenticate gateway submissions; verify job ID, snapshot hash, assigned pool, time bounds, schema, and size; accept the first valid result transactionally; quarantine conflicting later results; audit conflicts; and support worker-pool revocation. Worker trust is explicit and visible to the customer.

### R-071: Direct SQS and HTTPS modes can behave differently

- **Level:** High
- **Affects:** Phase 1 onward

Two transport paths can drift in acknowledgement order, visibility handling, retry timing, error mapping, or authentication. A bug in the stateless gateway can expose receipt handles or delete a job before its result is durable.

**Control:** Put execution, envelope, journal, and state-machine logic in shared packages; make adapters pass the same contract suite; in both modes publish the result before deleting an executed job and allow only a signed expired-job acknowledgement as the exception; keep the gateway free of product rules and PostgreSQL access; and run failure tests against both transports.

### R-072: Dedicated worker-pool queues create operational growth

- **Level:** Medium in Phase 1; High at startup scale
- **Affects:** Phase 1 onward

One dedicated FIFO job queue per customer worker pool gives strong routing isolation but increases queue, alarm, IAM-policy, key, and lifecycle-management counts. A shared queue is unsafe for unrelated customer workers because a consumer cannot filter before receiving a message.

**Control:** Start with one hosted pool and a small number of approved customer pools; use the reviewed manual provisioning/deletion runbook and versioned deployment manifest; expose queue health for operator review; enforce pool quotas; review AWS quotas and cost; and redesign routing in Phase 3 when measured pool count requires it. Terraform provisioning and automatic alarms begin in Phase 4.

### R-073: Published self-contained jobs cannot be recalled instantly

- **Level:** Medium
- **Affects:** Phase 1 onward

Because a worker does not query PostgreSQL, pausing, deleting, or editing a monitor cannot synchronously cancel an encrypted job already accepted by SQS. A job may run with the configuration version valid when it was scheduled.

**Control:** Keep only one outstanding scheduled job per monitor, use a two-minute job-start expiry, stop future publication immediately, and record the monitor version and snapshot hash. A valid unstarted expired job is acknowledged without calling the target; the control plane marks a result-less job unknown by deadline. A valid late executed result may correct provisional expiry but must not overwrite newer monitor state. Provide queue purge/revocation only for exceptional security events, and explain this bounded behavior in the UI and Phase 4 customer terms.

### R-074: Customer worker clocks can be wrong

- **Level:** Medium
- **Affects:** Phase 1 onward

A worker with a fast clock may discard a valid job as expired; a slow clock may run stale work. Worker timestamps can also make latency or ordering appear impossible.

**Control:** Require UTC time synchronization, expose clock-offset health, allow a small bounded expiry tolerance, use the control plane's scheduled and receive times as authoritative, validate worker timestamps within limits, and never let worker time alone overwrite newer monitor state. Test fast and slow clocks in both transport modes.

### R-075: Job-level result deduplication can hide a valid attempt

- **Level:** Critical
- **Affects:** Phase 1 onward

If every result uses `MessageDeduplicationId = job_id`, SQS may suppress a valid second execution result for five minutes after accepting a malformed or invalid first attempt. The consumer cannot store a message SQS never delivers.

**Control:** Give every durable result a stable `result_id`; reuse it only when retrying the same journaled result. Use `MessageDeduplicationId = result_id` and `MessageGroupId = job_id`. Different execution attempts use different result IDs, while PostgreSQL still accepts only one valid result per job. Test an invalid first attempt followed by a valid attempt inside the FIFO deduplication window.

### R-076: Late results can corrupt counters, incidents, and rollups

- **Level:** High
- **Affects:** Phase 1 onward

Results may arrive out of scheduled order or after a job was provisionally marked expired or dead. Applying only arrival order can create the wrong consecutive-failure count, open or resolve the wrong incident, and leave hourly summaries inconsistent with raw data.

**Control:** Serialize evaluation per monitor, order slots by scheduled time and job ID, persist the last evaluated slot, recompute the bounded threshold window, and invalidate affected rollups. Phase 1.2 records durable idempotent state-correction events only inside the documented ten-minute window. P1-401 applies those ordered corrections to incidents without deleting history or silently repeating notifications.

### R-077: A gateway can lose a job by accepting a malformed result

- **Level:** Critical
- **Affects:** Phase 1 onward

If the stateless gateway publishes any worker-supplied bytes and deletes the job as soon as SQS accepts them, the central consumer may later reject the result after the only executable job message has gone.

**Control:** Before publishing or deleting, verify mTLS identity, authenticated lease, schema and size, pool/job/snapshot/result identifiers, timestamps, and worker signature against the signed gateway configuration. A malformed result never deletes the job. The central consumer repeats authoritative checks against PostgreSQL.

### R-078: Dependency outages can consume retry counts instead of buffering

- **Level:** High
- **Affects:** Phase 1 onward

A result consumer that keeps polling while PostgreSQL is down can move valid results to the DLQ after ten receives. A worker that keeps receiving while it cannot publish results can similarly exhaust job attempts and grow its journal.

**Control:** Use readiness-gated polling and circuit breakers. Stop result receives while PostgreSQL is unhealthy. Stop new job receives while result publication is unhealthy and publish journaled results first. Use bounded backoff, visible queue-age health, and recovery tests proving receive counts do not burn during an extended dependency outage. Phase 4 adds automated queue-age alarms.

### R-079: Manual checks can delay scheduled coverage

- **Level:** High
- **Affects:** Phase 1 onward

FIFO does not provide priority between job groups. A burst of test-now work in the same pool can consume workers needed for scheduled checks even though manual results do not count toward uptime.

**Control:** Limit Phase 1 to ten waiting manual jobs, admit them only while scheduled age and capacity are healthy, reserve at least 90% of execution slots for scheduled work, defer manual messages when their semaphore is full, and reject manual work before scheduled coverage is threatened. Measure the policy under a manual burst and timeout storm.

### R-080: An underspecified cryptographic protocol is unsafe to implement

- **Level:** Critical
- **Affects:** Phase 1 onward

Saying “sign and encrypt” without a canonical encoding, algorithms, nonce rules, associated data, rotation overlap, and emergency revocation behavior can produce unverifiable messages, nonce reuse, downgrade bugs, or permanent data loss.

**Control:** Use the versioned deterministic-CBOR, Ed25519, X25519, HKDF-SHA-256, and AES-256-GCM contract in the design; bind routing fields as associated data; use reviewed libraries; keep retired normal-rotation keys for at least 21 days; and give compromise revocation no automatic grace. Test tampering, mismatch, rotation, rollback, and key-loss recovery.

### R-081: Worker credentials and mTLS certificates can expire or remain trusted too long

- **Level:** Critical
- **Affects:** Phase 1 onward

An expired certificate can stop private monitoring, while a stolen certificate or direct-SQS credential can retain queue access if revocation and renewal are not operationally defined.

**Control:** Customer Phase 1 workers use HTTPS/mTLS only; direct SQS is limited to hosted workers with temporary workload roles. Use an offline root, restricted issuance, 30-day certificates, renewal before ten days remain, and signed revocation/configuration snapshots loaded by gateways within five minutes. Alert before expiry and test loss, renewal, revocation, and gateway reload.

### R-082: A shared result FIFO can become a cross-tenant denial-of-service path

- **Level:** Critical
- **Affects:** Phase 1 onward

Cryptographic signatures prevent forged data from being accepted but do not stop a customer-controlled sender from flooding a shared queue with unique invalid messages and consuming cost, DLQ space, and consumer capacity.

**Control:** Do not give customer-VPC workers direct result-queue credentials in Phase 1. Route them through the gateway with per-pool request, byte, concurrent-pull, and publication quotas. Keep direct SQS for hosted workers, expose rejected/flood traffic in metrics and operator health views, and consider per-pool result queues if measured scale or trust requires stronger isolation. Automated flood alarms begin in Phase 4.

### R-083: Old customer workers can become protocol poison messages

- **Level:** High
- **Affects:** Phase 1 onward

Customer workers may remain offline during upgrades. Publishing a schema they cannot decode can send every job to the DLQ, and removing old result support too early can reject legitimate journal replay.

**Control:** Record each pool's worker version, schema range, and capabilities; support current and previous schemas; publish only their intersection; reject incompatible dispatch before SQS; upgrade consumers before publishers; and test rolling upgrade, downgrade, delayed worker return, and result replay across the full key/queue retention window.

### R-084: Worker-pool provisioning can stop halfway

- **Level:** High
- **Affects:** Phase 1 onward

Queue, policy, database, gateway, certificate, and key operations cannot commit as one transaction. A partially created pool can leak resources, route work incorrectly, or appear usable before all controls exist.

**Control:** Use explicit `provisioning`, `active`, `draining`, `revoked`, `deleting`, and `failed` states. In Phases 1–3, follow a reviewed manual runbook, record queue resources, policies, routing, keys, and certificates in a versioned non-secret deployment manifest, verify those dependencies before activation, compare actual state with the manifest, roll back partial work, audit lifecycle actions, and require drained queues plus explicit confirmation before deletion. Phase 4 creates and verifies separate production IAM roles and trust policies, imports or replaces resources under Terraform, and enables automated drift detection.

### R-085: Restoring PostgreSQL alone can conflict with live queues and keys

- **Level:** Critical
- **Affects:** Phase 4 customer production

A restored database may be older than SQS messages, gateway mappings, worker journals, or current keys. Starting publishers immediately can replay stale outbox rows; missing historical keys can make queued work unverifiable or unreadable. Phases 1–3 do not promise recovery: after material loss, operators may discard queues and rebuild the disposable environment.

**Control:** P4-701 backs up PostgreSQL together with the production deployment manifest, protected Terraform state, exported queue/policy/role attributes, signed gateway configuration, CA records, and active/retired key history. Restore into isolation; keep schedulers and consumers stopped; inventory queues; quarantine unknown identities; reconcile outboxes and accepted results; never republish expired jobs; replay valid results idempotently; and start components in the documented order. Test an older database backup beside newer queue data before admitting external clients.

### R-086: Uptime formulas have undefined boundary cases

- **Level:** High
- **Affects:** All phases

Coverage can be misreported around pause, deletion, interval changes, skipped jobs, late results, partial buckets, or zero denominators. A mathematically undefined ratio can accidentally display as 0% or 100%.

**Control:** Generate expected slots from schedule-period history; exclude inactive periods and manual work; count only unique accepted executed scheduled results as observed; define zero-denominator behavior as `no data`; invalidate and repair rollups after late results; and test interval boundaries, partial windows, skipped jobs, and late correction.

### R-087: Quarantine and redrive can expose secrets or repeat stale work

- **Level:** High
- **Affects:** Phase 1 onward

Ad-hoc inspection can expose decrypted job secrets, and blind DLQ redrive can execute expired checks or loop poison messages forever.

**Control:** Store bounded encrypted quarantine records for 14 days with audited operator-only access. Use a CLI with dry run, bounded batch, identity/key/expiry/state revalidation, preserved IDs, explicit reason and approver, and loop prevention. Expired jobs become unknown without execution; recoverable results replay idempotently.

### R-088: The platform may be unable to alert about its own outage

- **Level:** Critical for operations
- **Affects:** Phase 1 onward

Application metrics and notification outboxes are ineffective when the VM, PostgreSQL, scheduler, or notifier itself is unavailable.

**Control:** This risk is explicitly accepted for the personal/private deployment in Phases 1–3. Expose queue, DLQ, scheduler, database, worker, and disk health through native metrics, health endpoints, logs, dashboards, and a manual operator checklist, but do not claim automated independent alerting. In Phase 4, define measured numeric thresholds, use CloudWatch alarms for AWS signals, publish through environment-specific SNS topics with confirmed email and an independent operator destination, add OCI-native alarms and an external readiness/scheduler-heartbeat dead-man check, assign owners, and test every notification path regularly.

### R-089: Deployment topology can drift from documented queue ownership

- **Level:** High
- **Affects:** Phases 2–3

Older names such as `checker` can hide whether scheduler, publisher, result consumer, or queue gateway still exists and whether a database-free process accidentally gained PostgreSQL access during consolidation.

**Control:** Preserve explicit logical ownership in every deployment profile and Phase 1–3 deployment manifest: control-plane API, scheduler/job publisher, database-free worker/gateway, result consumer, notifier, and telemetry ingestion. Commands may share a process, but dependency and network-policy tests enforce the boundary. Update Phase 2 and Phase 3 diagrams whenever placement changes. Phase 4 Terraform must preserve and test the same boundaries.

---

## 15. Release Gates by Phase

### 15.1 Phase 1 private engineering qualification (not a public beta)

- [ ] URL, redirect, IPv4, IPv6, metadata, and DNS-rebinding tests pass.
- [ ] Hosted pools block private/special destinations and customer pools cannot escape their protected local CIDR allowlists.
- [ ] Cross-organization access tests pass for every resource.
- [ ] Request, queue, worker, and response-size limits are enforced.
- [ ] Queue names follow the `watchtrace-{environment}-...fifo` convention, one explicit AWS Region is recorded per environment, and every Phase 1 queue and DLQ uses SSE-SQS.
- [ ] Due scheduling, stable job insertion, immutable encrypted outbox creation, and schedule advancement commit atomically.
- [ ] FIFO job/result queues and FIFO DLQs use explicit attributes, encryption, and least-privilege policies. Job messages use `job_id` for deduplication and grouping; results use `result_id` for deduplication and `job_id` for grouping.
- [ ] A publisher crash after SQS acceptance is repaired by exact-message retry or result arrival; tests cover in-window suppression, post-expiry publish refusal, and forced late-duplicate handling.
- [ ] Retrying a journaled result reuses its `result_id`, a separate execution attempt gets a new one, and an invalid first result cannot suppress a later valid result.
- [ ] Hosted direct-SQS and customer HTTPS/mTLS workers pass the same contract tests and have no PostgreSQL dependency; customer workers have no direct SQS credentials.
- [ ] Workers verify and decrypt jobs, journal execution, sign results, publish the result before deleting an executed job, use only the documented expired-job acknowledgement exception, and long-poll only available capacity.
- [ ] The gateway verifies identity, lease, schema, identifiers, timestamps, size, and worker signature before result publication or job deletion; malformed results never delete jobs and per-pool traffic limits are enforced.
- [ ] The result consumer verifies job, result ID, snapshot, pool, signature, size, and time bounds; duplicates cannot store a second result or repeat incident/notification side effects.
- [ ] Result-consumer and worker circuit breakers stop new receives during PostgreSQL or result-publication outages without exhausting SQS receive attempts.
- [ ] Expired visibility timeouts are redelivered safely and conflicting results are quarantined.
- [ ] Target failures complete once without internal retry; only internal failures consume job attempts.
- [ ] Job DLQ, result DLQ, oldest-message age, visible/in-flight depth, outbox backlog, gateway health, journal health, and queue hard-limit events are visible in operator health views and the manual runbook.
- [ ] Worker-crash tests demonstrate and document the rare duplicate-request window.
- [ ] Worker clock-skew tests preserve expiry, result validation, and monitor-state ordering.
- [ ] Manual bursts preserve at least 90% of worker slots for scheduled work and never leave more than ten manual jobs waiting.
- [ ] Deterministic-CBOR, signature, encryption, associated-data, nonce, key-rotation, emergency-revocation, mTLS renewal/revocation, and signed-configuration tests pass.
- [ ] Current and previous worker schemas interoperate, incompatible dispatch is blocked before SQS, and signed worker artifacts can be verified.
- [ ] Worker-pool partial provisioning, reconciliation, draining, revocation, and deletion preserve isolation and leave no unintended active resources.
- [ ] Unknown checks, zero-denominator reports, pause/delete/interval boundaries, out-of-order results, late corrections, incident corrections, and rollup repair are shown correctly.
- [ ] Quarantine is encrypted and access-controlled; controlled redrive preserves IDs, refuses expired job execution, prevents loops, and is audited.
- [ ] Incident and notification creation is transactional and idempotent.
- [ ] Email domain, SPF, DKIM, retries, quota alerts, and suppressions are tested.
- [ ] Secrets do not appear in logs, database plaintext, or API responses.
- [ ] Disk, scheduler, database, notification-outbox, queue, and DLQ health can be inspected through metrics, health endpoints, logs, or dashboards.
- [ ] The deployment is access-controlled for owner-only testing, contains no external-client or irreplaceable data, and documents that total loss causes a rebuild/reset with no recovery promise.
- [ ] The deployed topology preserves documented control-plane, publisher, worker/gateway, result-consumer, notifier, and telemetry ownership and network boundaries.
- [ ] Any public signup or unknown-user access remains disabled; terms, privacy, and abuse controls are deferred to the Phase 4 external-client gate.

### 15.2 Phase 2 private tracing qualification (not a public beta)

- [ ] API-key creation, rotation, expiry, and revocation are tested.
- [ ] Duplicate, late, and out-of-order spans are handled.
- [ ] Trace payload, rate, attribute, event, and retention limits are enforced.
- [ ] Sensitive-attribute filters pass test datasets.
- [ ] Sampling and incomplete-trace behavior are explained in the UI.
- [ ] Cross-project trace access tests pass.
- [ ] Telemetry is rejected before it threatens control or uptime storage.
- [ ] Project usage and rejected-data counts are visible.
- [ ] Trace deletion and retention jobs are tested.
- [ ] External-client access remains disabled and all stored traces are disposable owner-controlled test data.

### 15.3 Before Phase 3 internal paid-scale validation

- [ ] Free-tier bottlenecks and paid capacity requirements are measured.
- [ ] Every service and dataset has one clear owner.
- [ ] APIs and events are versioned and have contract tests.
- [ ] Event consumers are idempotent and support replay.
- [ ] PostgreSQL check-job IDs and result idempotency survive migration to the external broker.
- [ ] PostgreSQL-to-ClickHouse migration has count and query comparison tests.
- [ ] Broker backlog, database outage, and regional-worker failure are tested.
- [ ] PostgreSQL failure modes and the proposed high-availability topology have been measured without claiming durability or restore guarantees.
- [ ] Costs per monitoring and telemetry unit are measured.
- [ ] Plan quotas protect the platform during one simulated tenant’s traffic spike.
- [ ] External-client access remains disabled and loss of the environment is still handled as a rebuild/reset.

### 15.4 Before Phase 4 external-client access or commercial commitments

- [ ] Usage and billing reconcile for several complete billing cycles.
- [ ] Enterprise user creation and removal are tested.
- [ ] On-call escalation and maintenance windows work end to end.
- [ ] Private agents cannot receive another tenant’s jobs.
- [ ] Export, retention, and deletion requests are tested.
- [ ] Regional recovery exercises meet documented targets.
- [ ] P4-701 nightly logical, continuous PostgreSQL, volume/database snapshot, and required analytics backups run automatically and meet the approved retention policy.
- [ ] Backup copies and separately encrypted secret/key recovery material exist in the required independent region or failure domain, and access is audited.
- [ ] Backup-age, failed-backup, storage-capacity, and recovery-test alerts reach an independent operator destination.
- [ ] A clean-environment restore of the complete recovery set passes using the documented component start order.
- [ ] An older PostgreSQL backup is reconciled safely with newer queues, configurations, certificates, and retained keys without executing expired jobs.
- [ ] Measured recovery-point and recovery-time results meet the targets approved for customer launch.
- [ ] Terraform manages intended AWS and OCI production resources; manually created resources have been imported or safely replaced, remote state is protected, reviewed plans are required, and drift checks pass.
- [ ] Environment-scoped IAM role names, trust restrictions, effective policy fingerprints, and exact queue/action permissions match the production deployment manifest; customer-VPC workers have no AWS role, and the production security exercises pass.
- [ ] Numeric CloudWatch thresholds and evaluation periods are based on measured capacity and stored with the infrastructure configuration.
- [ ] CloudWatch alarms deliver through environment-specific SNS topics to confirmed email subscriptions and an independent operator destination; OCI alarms and the external dead-man path also pass end-to-end tests.
- [ ] Terms of use, privacy notice, abuse contact, and public-signup controls are complete before external-client access is enabled.
- [ ] Security and legal reviews are complete.
- [ ] Any SLA is supported by measured availability and a support process.

---

## 16. Risks We Explicitly Accept During Private Personal Phases 1–3

The owner-only engineering deployments accept the following limitations:

- One OCI region and one main VM.
- Best-effort availability with no SLA.
- Monitoring from one network location.
- No scheduled backups, tested restore, recovery-point target, or recovery-time target; database, telemetry, queue, certificate, and key loss may require a complete rebuild/reset.
- No external-client or irreplaceable data is stored. Optional manual snapshots are a convenience only and create no recovery promise.
- Published check jobs and results survive ordinary application restarts in SQS, and committed immutable dispatch intent survives in PostgreSQL; this restart resilience is not disaster recovery, and operators may discard state after material loss.
- FIFO suppresses same-job publication retries only within five minutes. Worker crashes, lost journals, visibility expiry, or later retries may still cause a rare duplicate GET or HEAD.
- A job already published may run after its monitor is paused or edited; it is bounded by job expiry and recorded against the scheduled monitor version.
- Customer-VPC workers are trusted execution components with dedicated routing and keys; compromise of a worker can affect checks assigned to its pool.
- The OCI deployment depends on AWS SQS availability, credentials, quotas, networking, and a controlled AWS budget.
- Phase 1.2 development and controlled AWS validation may use one non-production IAM identity. Separate workload roles, trust-policy verification, and production security exercises are deferred to Phase 4; the shared validation identity is never an acceptable production configuration.
- AWS and OCI infrastructure is provisioned through reviewed manual runbooks and a versioned deployment manifest until Terraform is introduced in Phase 4.
- Numeric CloudWatch alarms, SNS/email infrastructure notifications, OCI alarms, and an external dead-man notification path are not provided until Phase 4; Phase 1–3 operations depend on visible health data and manual review.
- A check that reaches dead-letter state becomes unknown coverage until an operator resolves the internal failure.
- Rare duplicate email delivery.
- Strict monitor, trace, payload, and retention limits.
- Sampled and possibly incomplete distributed traces.
- PostgreSQL trace storage intended only for a small pilot.
- No guarantee that email reaches a recipient’s inbox.

These limitations must be visible in private deployment documentation. The environments must not be described as a beta, customer-ready, production-grade, durable, or recoverable. P4-701 and every Phase 4 commercial gate must pass before external clients are admitted.

---

## 17. Risk Review Schedule

Review this register:

- before beginning each phase;
- before opening signup to the public;
- after every serious incident;
- before increasing a capacity limit;
- before introducing a new storage system or message broker;
- before charging customers; and
- at least once every three months during active development.

Close a risk only when a test, measurement, or operating procedure proves the control works. Moving a risk into another document does not close it.
