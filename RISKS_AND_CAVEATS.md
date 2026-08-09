# Risks, Caveats, and Constraints

- **Companion to:** `DESIGN_SPECIFICATION.md`
- **Purpose:** Record what can go wrong, what the product cannot promise yet, and what must be tested before each release
- **Last updated:** 2026-08-04

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

1. Oracle Free Tier is suitable for development and a small beta, not startup-scale production.
2. A single VM and database are a single point of failure.
3. Monitoring results can be wrong or incomplete if our own scheduler is unhealthy.
4. User-provided URLs create serious server-side request forgery and abuse risks.
5. Trace and log data can accidentally contain passwords, tokens, personal data, and database statements.
6. Distributed traces are often incomplete because of sampling, missing instrumentation, or broken trace context.
7. Telemetry data grows much faster than ordinary business data.
8. Email acceptance does not guarantee delivery to a mailbox.
9. Amazon SQS survives application restarts, but Standard queues provide at-least-once delivery and can duplicate or reorder messages; PostgreSQL remains required for stable job state and idempotency.
10. Splitting into microservices adds network failures and data-consistency problems.
11. A technically successful product may still have poor business economics because telemetry storage and support are expensive.

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

**Control:** Plan work in small vertical slices. Review scope every two weeks. Cut optional features before reducing security or recovery work.

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

**Control:** Keep infrastructure instructions and backups ready so the service can be recreated. Do not offer an availability agreement from this deployment.

### R-007: One VM is one failure point

- **Level:** High
- **Affects:** Phases 1–2

VM failure, operating-system failure, full disk, Docker failure, PostgreSQL failure, or maintenance can stop the API, scheduling, workers, traces, and notifications together. SQS retains already published messages independently, but they cannot be processed until application and database access recover.

**Control:** Use readiness monitoring, external health checks, automatic container restart, block-volume backups, and a tested manual restore. Accept best-effort beta availability until paid infrastructure is introduced.

### R-008: ARM64 compatibility can cause deployment problems

- **Level:** Medium
- **Affects:** Phases 1–2

Some container images, native libraries, browser-test tools, or monitoring agents may not provide ARM64 builds.

**Control:** Build and test `linux/arm64` images in CI from the beginning. Do not add a dependency until its ARM64 path has been verified.

### R-009: Disk growth can stop the entire platform

- **Level:** Critical
- **Affects:** Phases 1–2

PostgreSQL raw checks, job-ledger and outbox rows, indexes, write-ahead logs, traces, temporary files, and Docker logs share limited storage. A full disk can stop dispatch recording, result commits, and new writes even while SQS still holds published messages.

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

**Control:** Validate scheme, port, every IPv4/IPv6 result, actual dialed address, and every redirect. Block private, loopback, link-local, multicast, metadata, and special-use destinations. Test DNS rebinding.

### R-020: The platform can be used to attack third parties

- **Level:** Critical
- **Affects:** Public beta onward

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

**Control:** Use an idempotent outbox for job creation, include a stable incident ID, and document rare duplicate delivery.

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
- **Affects:** All phases

A backup helps recover data, but restoration takes time and recent data after the backup may be lost.

**Control:** Publish recovery point and recovery time targets separately from availability. Run restore tests. Add replication and continuous backup only when paid infrastructure begins.

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
- **Affects:** Phases 1–3

The free deployment uses one OCI home region. Customers cannot choose where data is stored.

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

**Control:** Patch dependencies and operating systems, rotate secrets, scan images, review access, test backups, handle vulnerability reports, and maintain an incident-response process.

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

### R-060: Durable does not mean exactly once

- **Level:** High
- **Affects:** Phase 1 onward

Amazon SQS Standard queues can deliver a message more than once. If a worker sends a request and crashes before committing its result, the visibility timeout expires and another delivery may send the GET or HEAD request again. PostgreSQL can prevent a second stored result for the same stable job ID, but it cannot undo the duplicate request received by the monitored server.

**Control:** Permit only GET and HEAD, use stable job IDs, make result writes idempotent, document at-least-once execution, and test a crash between network completion and database commit.

### R-061: PostgreSQL and SQS cannot commit atomically

- **Level:** High
- **Affects:** Phases 1–2

A scheduler cannot atomically advance a PostgreSQL schedule and send an SQS message. Sending first can create orphan messages; committing PostgreSQL first without durable dispatch intent can lose work. A lost `SendMessage` response can also make publication outcome ambiguous.

**Control:** Commit the stable job, dispatch-outbox row, and schedule advance in one database transaction. Publish the outbox asynchronously, tolerate duplicate Standard-queue messages, record the SQS message ID, and reconcile old unpublished rows. Test rollback, publisher crash, lost response, duplicate publication, and SQS outage.

### R-062: Visibility can expire while a healthy worker is still running

- **Level:** High
- **Affects:** Phase 1 onward

A long pause, overloaded process, network interruption, or badly chosen visibility timeout can make SQS redeliver a job while the first worker is still executing it. Receipt handles change on redelivery.

**Control:** Keep request timeout safely below visibility timeout, receive only available worker capacity, extend visibility when justified, never log or persist receipt handles, identify every worker, and make completion compare the current PostgreSQL processing token. Test stale completion after redelivery.

### R-063: Poison jobs can retry forever or block useful work

- **Level:** High
- **Affects:** Phase 1 onward

A malformed monitor record or repeatable internal bug can fail every time the job is claimed.

**Control:** Separate target failures from internal failures, set source-queue redrive `maxReceiveCount` to three, use dedicated DLQs, reconcile DLQ messages to visible database `dead` state and unknown coverage, record only a safe error category, alert operators, and provide a controlled redrive tool after the bug is fixed.

### R-064: A durable backlog can grow without using much application memory

- **Level:** Critical for availability
- **Affects:** Phase 1 onward

Moving delivery to SQS prevents an in-memory crash but can hide overload while SQS messages, in-flight work, dispatch-outbox rows, and PostgreSQL ledger rows grow. SQS retention can then expire work that was never processed. Durability is not permission for an unlimited queue.

**Control:** Enforce one outstanding scheduled job per monitor and database-backed global admission limits; monitor visible, not-visible, oldest-message, DLQ, and outbox age; advance overdue schedules without replaying every slot; configure explicit retention; clean terminal ledger rows; and reject manual work before scheduled work.

### R-065: Queue priority can become unfair

- **Level:** Medium
- **Affects:** Public beta onward

SQS Standard queues do not provide application priority. Manual tests or one large organization may consume worker capacity and delay regular checks for everyone else.

**Control:** Use separate scheduled and manual source queues and DLQs, reserve worker capacity for scheduled checks, rate-limit manual jobs, keep manual results out of uptime and incident calculations, apply organization quotas, and measure delay by organization without putting organization IDs into high-cardinality metrics.

### R-066: AWS credentials or receipt handles can leak

- **Level:** Critical
- **Affects:** Phase 1 onward

Long-lived access keys, temporary session tokens, SSO cache material, or SQS receipt handles can grant queue access. Logs, committed `.env` files, images, error responses, and diagnostic dumps are common leak paths.

**Control:** Use the AWS SDK default credential chain and temporary IAM workload roles in production; use authenticated profiles or short-lived credentials locally; never embed credentials in code; redact AWS authorization data and receipt handles; grant least privilege per module; rotate exposed credentials immediately; and run secret scanning plus log/API redaction tests.

### R-067: Cross-cloud SQS dependency can stop dispatch or increase cost

- **Level:** High
- **Affects:** Phases 1–2

The initial application runs on OCI while SQS runs in AWS. Internet routing, DNS, TLS, either provider, AWS account configuration, quotas, or billing controls can interrupt queue access. SQS requests, CloudWatch, KMS, and cross-cloud data transfer may create costs even when the OCI VM is free.

**Control:** Use a regional HTTPS endpoint, short bounded SDK retries, outbox backlog alarms, queue-age alarms, AWS budgets, cost-allocation tags, explicit quotas, and a runbook for AWS or network failure. Queue payloads stay minimal. Test SQS unavailability without losing dispatch intent or replaying unlimited overdue schedules.

### R-068: SQS retention or DLQ misconfiguration can lose visible work

- **Level:** Critical for coverage
- **Affects:** Phase 1 onward

Messages can expire under the source retention period, move to the wrong DLQ, or become inaccessible through an incorrect queue policy. A DLQ with shorter effective retention may delete diagnostic work before operators respond.

**Control:** Manage queue attributes and policies as versioned infrastructure, keep DLQ retention longer than source retention, restrict redrive sources, reconcile database non-terminal jobs against dispatch and DLQ state, alarm before retention windows are approached, and count unrecoverable work as unknown coverage.

---

## 15. Release Gates by Phase

### 15.1 Before a public Phase 1 beta

- [ ] URL, redirect, IPv4, IPv6, metadata, and DNS-rebinding tests pass.
- [ ] Cross-organization access tests pass for every resource.
- [ ] Request, queue, worker, and response-size limits are enforced.
- [ ] Due scheduling, stable job insertion, dispatch-outbox creation, and schedule advancement commit atomically; SQS publication and reconciliation tests pass.
- [ ] Workers long-poll only available capacity and duplicate deliveries cannot overwrite a newer processing attempt.
- [ ] Expired SQS visibility timeouts are redelivered and duplicate stored results are prevented by job ID.
- [ ] Target failures complete once without internal retry; only internal failures consume job attempts.
- [ ] DLQ jobs, oldest-message age, visible/in-flight depth, outbox backlog, and queue hard-limit events create operational alerts.
- [ ] Worker-crash tests demonstrate and document the rare duplicate-request window.
- [ ] Unknown checks and coverage are shown correctly.
- [ ] Incident and notification creation is transactional and idempotent.
- [ ] Email domain, SPF, DKIM, retries, quota alerts, and suppressions are tested.
- [ ] Secrets do not appear in logs, database plaintext, or API responses.
- [ ] Disk, scheduler, database, email queue, and backup age have alerts.
- [ ] A backup has been restored on a clean machine.
- [ ] Terms of use, privacy notice, and abuse contact exist before accepting unknown public users.

### 15.2 Before a public Phase 2 tracing beta

- [ ] API-key creation, rotation, expiry, and revocation are tested.
- [ ] Duplicate, late, and out-of-order spans are handled.
- [ ] Trace payload, rate, attribute, event, and retention limits are enforced.
- [ ] Sensitive-attribute filters pass test datasets.
- [ ] Sampling and incomplete-trace behavior are explained in the UI.
- [ ] Cross-project trace access tests pass.
- [ ] Telemetry is rejected before it threatens control or uptime storage.
- [ ] Project usage and rejected-data counts are visible.
- [ ] Trace deletion and retention jobs are tested.

### 15.3 Before Phase 3 paid scale

- [ ] Free-tier bottlenecks and paid capacity requirements are measured.
- [ ] Every service and dataset has one clear owner.
- [ ] APIs and events are versioned and have contract tests.
- [ ] Event consumers are idempotent and support replay.
- [ ] PostgreSQL check-job IDs and result idempotency survive migration to the external broker.
- [ ] PostgreSQL-to-ClickHouse migration has count and query comparison tests.
- [ ] Broker backlog, database outage, and regional-worker failure are tested.
- [ ] Highly available PostgreSQL and restore procedures meet their targets.
- [ ] Costs per monitoring and telemetry unit are measured.
- [ ] Plan quotas protect the platform during one customer’s traffic spike.

### 15.4 Before Phase 4 commercial commitments

- [ ] Usage and billing reconcile for several complete billing cycles.
- [ ] Enterprise user creation and removal are tested.
- [ ] On-call escalation and maintenance windows work end to end.
- [ ] Private agents cannot receive another tenant’s jobs.
- [ ] Export, retention, and deletion requests are tested.
- [ ] Regional recovery exercises meet documented targets.
- [ ] Security and legal reviews are complete.
- [ ] Any SLA is supported by measured availability and a support process.

---

## 16. Risks We Explicitly Accept During the Free Beta

The initial beta accepts the following limitations:

- One OCI region and one main VM.
- Best-effort availability with no SLA.
- Monitoring from one network location.
- Manual recovery within the documented target.
- Published check jobs survive application restarts in SQS, and committed dispatch intent survives in PostgreSQL; PostgreSQL failure still stops new scheduling and safe execution until recovery.
- A worker crash or duplicate SQS delivery may cause a rare duplicate GET or HEAD after visibility expiry.
- The OCI deployment depends on AWS SQS availability, credentials, quotas, networking, and a controlled AWS budget.
- A check that reaches dead-letter state becomes unknown coverage until an operator resolves the internal failure.
- Rare duplicate email delivery.
- Strict monitor, trace, payload, and retention limits.
- Sampled and possibly incomplete distributed traces.
- PostgreSQL trace storage intended only for a small pilot.
- No guarantee that email reaches a recipient’s inbox.

These limitations must be visible in beta documentation. They must not be hidden behind the word “production-grade.”

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
