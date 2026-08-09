# Real-Time API Monitoring and Distributed Tracing Platform

## Design Specification

- **Status:** Source of truth for implementation
- **Project type:** Personal project designed to grow into a startup
- **Initial hosting:** Oracle Cloud Infrastructure Always Free for compute/database plus managed Amazon SQS in one AWS Region
- **Growth plan:** Buy infrastructure when real usage requires it
- **Last updated:** 2026-08-04

Companion document: [Risks, Caveats, and Constraints](./RISKS_AND_CAVEATS.md)

---

## 1. Purpose of This Document

This document explains what we are building, how the main parts work, and the order in which we will build them.

The product will begin as a small monitoring platform running primarily on Oracle Cloud Free Tier, with Amazon SQS providing managed durable delivery. It will later grow into a scalable startup product without requiring a complete rewrite.

The work is divided into four phases:

1. **API uptime monitoring:** Regularly call APIs, measure response time, detect failures, and send alerts.
2. **Distributed tracing:** Follow one request as it moves through several services and show where it became slow or failed.
3. **Startup scale:** Add paid infrastructure, multiple checking locations, high-volume data storage, logs, metrics, and integrations.
4. **Mature SaaS product:** Add billing, enterprise security, on-call tools, stronger availability, and advanced analysis.

Each phase must produce a useful working product. We will not build Phase 3 infrastructure before Phase 1 and Phase 2 have real users and measured limits.

---

## 2. Product Vision

The platform should help a development team answer five questions:

1. Is our API available from the public internet?
2. How long is the API taking to respond?
3. When did a failure begin, and when did it recover?
4. Which service or database call made a real request slow or fail?
5. Who was notified, and what happened during the incident?

The product combines two kinds of monitoring.

### 2.1 Uptime monitoring

Our platform sends a request to a configured URL every minute or every few minutes.

Example:

```text
GET https://api.example.com/health
Expected status: 200–299
Timeout: 5 seconds
Alert after: 3 failures
```

This tells us whether the endpoint is reachable and how quickly it responds.

### 2.2 Distributed tracing

A real user request may travel through several services:

```text
Mobile application
      │
      ▼
API Gateway
      │
      ▼
Order Service ──▶ Payment Service ──▶ Database
```

A **trace** represents the complete request. A **span** represents one step inside that request.

```text
Trace: Create Order                       820 ms
├── API Gateway                            25 ms
├── Order Service                         760 ms
│   ├── Validate order                     20 ms
│   ├── Payment Service                   610 ms  ← slow step
│   └── Save order                         85 ms
└── Return response                        35 ms
```

We will use OpenTelemetry so customers can use existing tools and libraries instead of installing a custom monitoring library created by us.

### 2.3 Product principles

- Start small, but keep clear boundaries between parts of the system.
- Use open standards and avoid locking customers into a custom SDK.
- Never call user-provided URLs without strong network safety checks.
- Never count missing monitoring data as successful uptime.
- Store important jobs in the database so a restart does not lose alerts.
- Add expensive infrastructure only after measurements show that it is needed.
- Make limits visible. Do not claim capacity or availability that we have not tested.

---

## 3. Simple Product Model

The product uses the following hierarchy:

```text
User
  └── Organization
        ├── Members and roles
        └── Project
              └── Environment
                    ├── API monitors
                    ├── Services
                    ├── Traces
                    ├── Alert rules
                    └── Incidents
```

### 3.1 Organization

An organization represents a team or company. It owns projects, members, and billing information.

### 3.2 Project

A project represents one product or system, such as “E-commerce Platform.”

### 3.3 Environment

An environment separates production, staging, and development data.

Examples:

```text
E-commerce Platform
├── Production
├── Staging
└── Development
```

Monitors, API keys, services, and traces belong to an environment. This separation must exist from Phase 1 so we do not have to redesign tenant data later.

### 3.4 Roles

| Role | Main permissions |
|---|---|
| Owner | Full access, organization settings, ownership, and later billing |
| Admin | Manage members, projects, monitors, alerts, and incidents |
| Member | Manage monitors and incidents, view all monitoring data |
| Viewer | View dashboards, traces, and incidents only |

---

## 4. Product at the End of Each Phase

| Phase | Working result | Hosting plan |
|---|---|---|
| Phase 1 | Uptime checks, incidents, email alerts, and live dashboard | Oracle Always Free |
| Phase 2 | Distributed traces, service map, slow request search, and traffic alerts | Oracle Always Free with strict limits |
| Phase 3 | Multi-region checks, high-volume traces, metrics, logs, integrations, and status pages | Paid infrastructure starts |
| Phase 4 | Billing, enterprise login, on-call schedules, strong availability, and advanced analysis | Multi-region paid platform |

### 4.1 Quality targets

These are engineering targets, not customer promises until production measurements prove them.

| Target | Phase 1–2 | Phase 3 | Phase 4 direction |
|---|---:|---:|---:|
| Normal API request time | p95 below 300 ms | p95 below 200 ms | p95 below 200 ms |
| Dashboard query time | p95 below 700 ms | p95 below 500 ms | p95 below 500 ms |
| Alert evaluation | Within 5 seconds after qualifying data is stored | Within 5 seconds | Within 5 seconds |
| First notification attempt | Within 30 seconds | Within 30 seconds | Based on escalation policy |
| Backup recovery point | 24 hours or better | 15 minutes or better | 5 minutes or better |
| Recovery time | 4 hours or better | 1 hour or better | 30 minutes or better |
| Availability | Best-effort beta | Internal 99.9% goal | Possible 99.9–99.95% SLA after proof |

Phase 2 additionally targets trace-batch acceptance below one second and recent trace searches below two seconds at the stated pilot load.

---

## 5. Main Technology Choices

| Area | Choice | Reason |
|---|---|---|
| Backend | Go | Good network performance and simple concurrency |
| HTTP API | Gin | Small and easy to understand |
| Database access | SQLC | Generates safe Go code from explicit SQL |
| Main database | PostgreSQL | Reliable transactions and enough capacity for the first two phases |
| Phase 1 durable delivery | Amazon SQS Standard queues | Managed at-least-once delivery, visibility timeouts, long polling, and DLQs |
| Queue transaction ledger | PostgreSQL job and dispatch-outbox tables | Atomic schedule advancement, stable identity, state, and idempotent results across the PostgreSQL/SQS boundary |
| Frontend | React and TypeScript | Good dashboard ecosystem |
| UI components | Mantine | Speeds up accessible UI development |
| Charts | Apache ECharts | Suitable for time-series and trace charts |
| Live dashboard | Server-Sent Events | Simpler than WebSockets for one-way updates |
| Public proxy | Nginx | HTTPS, static files, and basic request limits |
| Trace format | OpenTelemetry OTLP | Open standard supported by many languages |
| Email | OCI Email Delivery | Uses the deployment provider's email integration |
| Secrets | OCI Vault | Keeps encryption keys outside the database |
| Initial deployment | Docker Compose | Simple enough for one VM |
| Large telemetry storage | ClickHouse in Phase 3 | Designed for large analytical datasets |
| Event buffer | Kafka-compatible broker in Phase 3, only if needed | Protects ingestion during traffic spikes |

We will use supported versions at implementation time and pin exact versions in the repository that owns each dependency.

---

## 6. Architecture Growth

### 6.1 Phase 1 architecture

```text
Browser ──▶ Nginx ──▶ Go API
                         │
                         ▼
                    PostgreSQL
                    ├── configuration and results
                    ├── job ledger and check dispatch outbox
                    └── notification outbox
                              │
                              ▼
                    SQS dispatchers ──▶ Amazon SQS source queues + DLQs
                                             │                 │
                                             ▼                 ▼
                                        check workers      email worker

scheduler ── atomically records job, dispatch intent, and schedule advance ──▶ PostgreSQL
```

This is one modular application, not a collection of microservices. The scheduler, dispatchers, and workers may run in the same Go process at first, but durable delivery crosses Amazon SQS and transactional state remains in PostgreSQL. They can therefore become separate processes later without changing message or idempotency contracts.

### 6.2 Phase 2 architecture

```text
Instrumented customer applications
          │ OpenTelemetry traces
          ▼
OTLP ingestion endpoint
├── Authenticate project key
├── Check limits
├── Validate and reduce data
└── Store trace batches
          │
          ▼
PostgreSQL trace partitions
          │
          ▼
Trace search, waterfall, service map, and alerts
```

The uptime system and trace system still share one PostgreSQL database and the backend monorepo, but they run as separate Go commands when useful. The React application remains in the separate frontend repository:

```text
api
checker
ingester
notifier
```

On the free VM, these commands may run as separate containers with strict memory limits. They still use the same codebase and database.

### 6.3 Phase 3 architecture

```text
                         ┌── Check workers: Region A
Scheduler ── Job queue ──┼── Check workers: Region B
                         └── Check workers: Region C

Applications ──▶ OTLP gateways ──▶ Event buffer ──▶ Trace processors
                                                     │
                                      ┌──────────────┴──────────────┐
                                      ▼                             ▼
                               ClickHouse                       Object storage

Browser ──▶ Load balancer ──▶ API instances ──▶ PostgreSQL
                                      │
                                      └── Redis for shared short-lived state
```

PostgreSQL keeps accounts, projects, settings, monitors, alert rules, incidents, and billing data. ClickHouse stores large volumes of traces, metrics, logs, and check results.

### 6.4 Phase 4 architecture

Phase 4 runs important services across more than one failure zone or region. It adds automatic failover, regional data rules, private monitoring agents, enterprise identity, and customer-specific limits.

Kubernetes is optional. We will adopt it only when managing many service instances manually becomes harder than managing a cluster.

---

## 7. Shared Data Model

This section lists the important tables and their purpose. Detailed SQL migrations will be written during each phase.

### 7.1 Accounts and ownership

| Table | Purpose | Important fields |
|---|---|---|
| `users` | User identity | id, email, password hash, verified state |
| `refresh_tokens` | Browser sessions | token digest, user, expiry, revoked time, token family |
| `user_action_tokens` | Email verification and password reset | purpose, digest, expiry, used time |
| `organizations` | Team/company | id, name, slug, deleted time |
| `org_members` | User role in an organization | organization, user, role, alert preference |
| `org_invitations` | Pending team invitations | email, role, token digest, expiry |
| `projects` | Product or system | organization, name, description |
| `environments` | Production/staging/development separation | project, name, type |

There must be exactly one organization owner. Creating an organization, project, default production environment, and owner membership happens in one database transaction.

### 7.2 Uptime monitoring

| Table | Purpose | Important fields |
|---|---|---|
| `monitors` | Endpoint configuration | environment, URL, method, interval, timeout, expected status |
| `monitor_secrets` | Encrypted request headers | monitor, encrypted value, key version |
| `monitor_state` | Latest known state | healthy/degraded/down/unknown, counters, last check |
| `monitor_schedule_periods` | History of interval and pause changes | monitor, interval, active period |
| `check_jobs` | Transactional job ledger for scheduled and manual checks | monitor, scheduled time, state, receive count, processing token, SQS message ID |
| `check_dispatch_outbox` | Atomic SQS publication intent | job, queue kind, publish state, attempt, safe error, SQS message ID |
| `health_checks` | Individual results | unique job ID, monitor, scheduled time, timings, status, error |
| `monitor_rollups_hourly` | Hourly summary | expected, observed, healthy, failed, latency |
| `monitor_rollups_daily` | Daily summary | expected, observed, healthy, failed, latency |
| `alert_rules` | Failure and recovery settings | monitor, failure count, recovery count |
| `incidents` | An outage or performance problem | start, status, reason, resolved time |
| `incident_events` | Incident timeline | event type, actor, time, safe details |
| `notification_outbox` | Emails waiting to be sent | incident, recipient, attempts, next attempt |

### 7.3 Distributed tracing

| Table | Purpose | Important fields |
|---|---|---|
| `project_api_keys` | Authenticate incoming telemetry | environment, key prefix, key digest, scopes, expiry |
| `services` | Known application services | environment, service name, latest seen time |
| `traces` | One complete request summary | trace ID, root service, start, duration, status, span count |
| `spans` | One step in a trace | trace ID, span ID, parent ID, service, operation, duration, status |
| `span_events` | Important events inside a span | span, name, time, limited attributes |
| `service_edges` | Calls between services | source service, target service, count, errors, latency |
| `trace_rollups_hourly` | Traffic summary | service, operation, requests, errors, latency percentiles |
| `telemetry_alert_rules` | Real-traffic alert settings | error rate, latency, no-data window |
| `ingestion_usage` | Usage and limit tracking | environment, minute/day, accepted and rejected counts |

Trace and span IDs are stored in their compact binary form. Large or sensitive attributes are not copied blindly into the database.

### 7.4 Startup operations

| Table | Added in | Purpose |
|---|---:|---|
| `notification_channels` | 3 | Email, Slack, webhook, and later other destinations |
| `status_pages` | 3 | Public service status |
| `status_page_components` | 3 | Services shown on a status page |
| `usage_daily` | 3 | Checks, spans, metrics, logs, and storage used by a customer |
| `subscriptions` | 4 | Plan and billing state |
| `on_call_schedules` | 4 | Who should respond at a given time |
| `escalation_policies` | 4 | Who to notify when an incident is not acknowledged |
| `private_agents` | 4 | Customer-installed workers for private endpoints |
| `audit_logs` | 1 | Security-sensitive user and system actions |

### 7.5 Tenant safety

Every customer-owned row contains an organization ID directly or can be connected to one through a required parent. Every query checks the organization, even if IDs are globally unique.

Database constraints must prevent a monitor, trace, incident, or API key from accidentally pointing to another organization’s project.

---

## 8. Phase 1 — API Uptime Monitoring Foundation

### 8.1 Goal

Build a complete, useful uptime monitoring product on Oracle Always Free.

At the end of Phase 1, a user can:

1. Create an account and organization.
2. Create a project and production environment.
3. Add an API URL.
4. See checks appear on a live dashboard.
5. Receive an email after repeated failures.
6. Acknowledge an incident.
7. See the incident resolve when the API recovers.
8. Restart the application without losing checks waiting in the durable queue.

### 8.2 Initial limits

These limits are starting safety settings. Load tests decide whether they can be increased.

| Item | Starting limit |
|---|---:|
| Organizations | Small beta |
| Monitors per organization | 100 |
| Total active monitors | 1,000 |
| Smallest normal interval | 60 seconds |
| Default interval | 5 minutes |
| Normal dispatch rate | 20 checks/second |
| Check workers | Up to 100 |
| Outstanding scheduled jobs per monitor | 1 |
| Global scheduled jobs pending or running | At most 1,000 |
| Manual test jobs waiting | At most 100 platform-wide |
| SQS visibility timeout | 60 seconds, configurable and safely above request timeout |
| SQS receive wait | 20-second long polling |
| Internal receives | 3 before SQS DLQ redrive |
| SQS source-message retention | 4 days |
| SQS DLQ retention | 14 days |
| Completed ledger-row retention | 48 hours |
| Dead/cancelled ledger-row retention | 7 days |
| Default timeout | 5 seconds |
| Maximum timeout | 10 seconds |
| Raw checks | 7 days |
| Hourly summaries | 90 days |
| Daily summaries | 1 year |

The 20 checks/second goal assumes ordinary endpoints respond in about two seconds or less. If many endpoints use the full timeout, the SQS backlog and PostgreSQL job ledger will grow. Hard admission limits, one outstanding scheduled job per monitor, explicit SQS retention, and CloudWatch alarms prevent silent unlimited growth.

### 8.3 Monitor configuration

Phase 1 supports:

- GET and HEAD requests only;
- HTTP and HTTPS;
- ports 80 and 443;
- encrypted custom request headers;
- status-code ranges such as 200–299;
- intervals of 60 seconds, 2 minutes, 5 minutes, 10 minutes, and 30 minutes;
- timeouts from 1 to 10 seconds; and
- pause, resume, test, and soft delete.

Phase 1 does not store response bodies. Later we may add small text matching with strict size limits.

### 8.4 Durable check queue and scheduler

Phase 1 uses Amazon SQS Standard queues as the durable delivery transport. PostgreSQL remains the transactional source of truth for schedules, stable job identity, job state, coverage, and results. No in-memory channel owns work.

The scheduler polls due monitors in small batches. For each due monitor, one database transaction:

1. Locks the due monitor so another scheduler cannot schedule the same time slot.
2. Inserts a `check_jobs` ledger row with a stable job ID and scheduled time.
3. Inserts a `check_dispatch_outbox` row containing only safe routing data and the job ID.
4. Advances `next_check_at` from the planned time, not from completion time.
5. Commits all three changes together.

The job insert is idempotent through a unique monitor-and-scheduled-time key. If the transaction fails, the job, dispatch intent, and schedule advance all roll back. A repeatable dispatcher reads unpublished outbox rows, sends the stable job ID to the appropriate SQS queue, and records the returned SQS message ID. If a publish response is lost, republishing can create a duplicate Standard-queue message; worker idempotency makes that safe.

The PostgreSQL ledger stores:

- organization, environment, monitor, and monitor version;
- `scheduled` or `manual_test` job type;
- scheduled time and dispatch state;
- `pending`, `running`, `completed`, `cancelled`, or `dead` state;
- SQS queue kind and message ID;
- approximate receive count, worker identity, processing token, and visibility deadline;
- last internal error; and
- created, started, and completed times.

Important rules:

- Only one scheduled job may be pending or running for a monitor.
- A job stores the monitor version, not a plaintext URL or decrypted headers.
- If a monitor is paused, deleted, or changed before execution, a stale job is cancelled safely.
- Manual test results are shown separately and do not change uptime, coverage, monitor state, or incidents.
- Small stable timing offsets spread jobs so they do not all become due at once.
- The scheduler advances an overdue monitor to the first future slot instead of replaying every missed interval.
- When the queue is at its hard limit, new work is not added blindly. The skipped time lowers coverage and raises an operational alert.
- Scheduled and manual checks use separate Standard queues and DLQs. Workers always reserve scheduled capacity before polling manual work.
- SQS messages contain no URL, custom header, credential, response body, or tenant secret. Workers load current monitor data from PostgreSQL after receiving the stable job ID.
- Server-side encryption is mandatory. Queue policies and IAM roles are least privilege; public access is prohibited.
- SQS is the delivery source of truth and PostgreSQL is the state/idempotency source of truth. Reconciliation detects unpublished outbox rows, messages moved to DLQs, and non-terminal ledger rows with no viable delivery.

#### 8.4.1 SQS resources, configuration, and identity

Phase 1.2 provisions four check queues in one AWS account and Region:

| Queue | Purpose |
|---|---|
| Scheduled source | Normal scheduled checks; workers reserve capacity for this queue |
| Scheduled DLQ | Scheduled messages that exceed the receive limit |
| Manual source | User-requested test-now checks with a separate global admission limit |
| Manual DLQ | Manual messages that exceed the receive limit |

All four are Standard queues. Source queues use 20-second long polling, a 60-second default visibility timeout, four-day retention, server-side encryption, and a redrive policy with `maxReceiveCount` 3. DLQs use 14-day retention and a redrive-allow policy restricted to their source queue. Exact values remain configuration and infrastructure code, and tests assert deployed attributes rather than assuming console defaults.

Required non-secret application settings are AWS Region, source queue URLs, DLQ URLs for reconciliation, an optional SQS endpoint override for the local emulator, and alarm thresholds. Queue ARNs, AWS account ID, encryption choice, and optional KMS key ARN are required by infrastructure and IAM policy configuration.

The Go application uses the AWS SDK for Go v2 default configuration and credential chain. Production uses temporary workload-role credentials whenever possible. Local development uses an authenticated AWS profile/Identity Center session or short-lived environment credentials. Static access keys are never embedded in source, images, migrations, database rows, queue messages, or committed `.env` files.

IAM is split by responsibility:

- scheduler/dispatcher: `sqs:SendMessage` and the minimum queue-attribute read permissions on the two source queues;
- checker workers: `sqs:ReceiveMessage`, `sqs:ChangeMessageVisibility`, `sqs:DeleteMessage`, and minimum queue-attribute reads on source queues;
- DLQ reconciler: receive/delete/attribute access only on the two check DLQs;
- infrastructure operator: queue creation, tagging, policies, redrive configuration, encryption, and CloudWatch alarms; and
- KMS permissions only when a customer-managed key is selected, scoped to the queue encryption use case.

CloudWatch alarms cover oldest-message age, visible and not-visible counts, DLQ depth, dispatch-outbox age, publish failures, and non-terminal ledger reconciliation failures. Metrics must not use job, monitor, user, or organization IDs as dimensions.

Implementation references: [SQS at-least-once delivery](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/standard-queues-at-least-once-delivery.html), [visibility timeouts](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html), [dead-letter queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html), [long polling](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/best-practices-setting-up-long-polling.html), [server-side encryption](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-server-side-encryption.html), [least-privilege IAM examples](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-basic-examples-of-iam-policies.html), and [AWS SDK for Go v2 configuration and credential chain](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html).

### 8.5 Worker receiving, visibility, and result flow

A worker long-polls only when it has free execution slots. For each received SQS message it starts a short PostgreSQL transaction that locks the stable job row, rejects malformed or stale work, records the worker ID, SQS receive metadata, a random processing token, and the visibility deadline, then changes an eligible `pending` job to `running`. The SQS receipt handle is held only in worker memory and never logged. A redelivery of a completed, cancelled, or dead job is deleted without sending another request.

The HTTP request happens outside the claim transaction. After the request finishes, one result transaction:

1. Inserts one `health_checks` result keyed by job ID.
2. Updates monitor state.
3. Creates or resolves an incident when required.
4. Inserts any notification outbox rows.
5. Marks the check job completed while comparing the current processing token.

Only after that transaction commits does the worker call `DeleteMessage` with the current receipt handle. If deletion fails, SQS may redeliver the message; the terminal database state prevents another request and the duplicate delivery is deleted.

Target outcomes such as DNS failure, timeout, connection failure, TLS failure, or an unexpected status are valid unhealthy results. They complete the job and are **not** retried.

Only internal failures that prevent a valid result from being stored—such as a worker crash, SQS API interruption, or temporary database failure—are retried. When possible, the worker returns the database job to `pending` and uses `ChangeMessageVisibility` for a short bounded delay. Otherwise the visibility timeout expires and SQS redelivers the message. After three receives, the SQS redrive policy moves the message to its DLQ; a reconciler marks the database job `dead`, creates an operational alert, and counts the scheduled check as unknown coverage.

The execution guarantee is **at least once**, not exactly once. SQS Standard queues can deliver a message more than once. If a worker crashes after sending the HTTP request but before committing the result, another delivery may send the same GET or HEAD request again. The unique job ID and processing-token comparison prevent duplicate stored results and stale completion, but they cannot prevent the monitored server from seeing a rare duplicate request.

### 8.6 Check flow

```text
Scheduler commits job, dispatch outbox, and schedule advance
        │
        ▼
Dispatcher publishes stable job ID to SQS
        │
        ▼
Worker receives message with a visibility timeout
        │
        ▼
Worker validates destination and monitor version
        │
        ▼
Worker sends request and records timings
        │
        ▼
One database transaction:
  save one result for the job ID
  update monitor state
  create or resolve incident
  create notification outbox work
  mark check job completed
        │
        ▼
Delete the SQS message with its current receipt handle
        │
        ▼
Send live dashboard event after commit
```

### 8.7 Incident rules

Default behavior:

```text
Failure 1 → degraded
Failure 2 → degraded
Failure 3 → down and open incident
Success 1 → incident remains open
Success 2 → resolve incident
```

Only one open incident may exist for the same monitor and rule. An acknowledged incident is still open. Manual resolution records the user and reason but does not stop future monitoring.

### 8.8 Email delivery

Incident emails are written to a PostgreSQL outbox table in the same transaction as the incident. A dispatcher publishes only the delivery ID to a separate SQS notification queue, and a separate worker sends through OCI Email Delivery. The notification queue and DLQ do not share check-job policies or retry settings.

Retry schedule:

```text
Immediately → 1 minute → 5 minutes → 25 minutes → failed
```

Mail goes only to verified organization members who enabled incident notifications. Rare duplicate email delivery is possible if the SMTP server accepts a message immediately before our process crashes; the incident ID is included so the duplicate is recognizable.

### 8.9 Live dashboard

The Go application sends a Server-Sent Event when a monitor or incident changes. The event contains an ID, not the full private record. The React application then reloads the affected data.

A 15-second polling fallback covers lost or disconnected live events.

Dashboard pages:

- Organization and project selector.
- Overview with healthy, degraded, down, and unknown counts.
- Monitor list.
- Monitor detail with uptime and latency chart.
- Recent checks.
- Incident list and incident timeline.
- Organization members and alert preferences.

### 8.10 API endpoints

```text
POST /api/v1/auth/signup
POST /api/v1/auth/login
POST /api/v1/auth/refresh
POST /api/v1/auth/logout
POST /api/v1/auth/verify-email
POST /api/v1/auth/forgot-password
POST /api/v1/auth/reset-password

GET  /api/v1/organizations
POST /api/v1/organizations
GET  /api/v1/organizations/:orgId/members
POST /api/v1/organizations/:orgId/invitations

GET  /api/v1/organizations/:orgId/projects
POST /api/v1/organizations/:orgId/projects
POST /api/v1/projects/:projectId/environments

GET    /api/v1/environments/:environmentId/monitors
POST   /api/v1/environments/:environmentId/monitors
GET    /api/v1/environments/:environmentId/monitors/:monitorId
PUT    /api/v1/environments/:environmentId/monitors/:monitorId
DELETE /api/v1/environments/:environmentId/monitors/:monitorId
POST   /api/v1/environments/:environmentId/monitors/:monitorId/test
POST   /api/v1/environments/:environmentId/monitors/:monitorId/pause
POST   /api/v1/environments/:environmentId/monitors/:monitorId/resume

GET  /api/v1/environments/:environmentId/incidents
GET  /api/v1/environments/:environmentId/incidents/:incidentId
POST /api/v1/environments/:environmentId/incidents/:incidentId/acknowledge
POST /api/v1/environments/:environmentId/incidents/:incidentId/resolve

GET /api/v1/environments/:environmentId/dashboard
GET /api/v1/environments/:environmentId/events
```

### 8.11 Oracle Free Tier deployment

Initial production containers:

```text
nginx
api-monitor
postgres
```

Use one ARM64 Ampere A1 VM with the current free allocation. Keep PostgreSQL data on a separate block volume. Use OCI Vault, Object Storage, Monitoring, Logging, Bastion, and Email Delivery where suitable. The VM must also reach the selected regional Amazon SQS endpoint over HTTPS. SQS requests, data transfer, CloudWatch alarms, and optional KMS use are budgeted AWS dependencies and are not assumed to be permanently free.

Oracle currently documents 2 A1 OCPUs and 12 GB memory for an Always Free tenancy. These limits can change, so verify them before deployment: [OCI Free Tier](https://docs.oracle.com/iaas/Content/FreeTier/freetier.htm).

### 8.12 Phase 1 build order

1. Repository, local Docker Compose, migrations, SQLC, CI.
2. Users, sessions, email verification, password reset.
3. Organizations, projects, environments, members, and roles.
4. Monitor CRUD and strong URL safety.
5. PostgreSQL job ledger and dispatch outbox, Amazon SQS source queues and DLQs, scheduler, SQS workers, and test endpoints.
6. Result transaction, state, incidents, and recovery.
7. Notification outbox, separate SQS notification queue and DLQ, and OCI Email Delivery.
8. Dashboard APIs and React pages.
9. Live events and polling fallback.
10. Rollups, retention, audit records, metrics, logs, and backups.
11. Load tests, security tests, restore test, OCI deployment, AWS SQS infrastructure, workload identity, budgets, and cross-cloud recovery exercises.

### 8.13 Phase 1 completion rules

Phase 1 is complete when:

- A new user can complete the full flow without database edits.
- Monitoring survives an application restart.
- Pending check jobs survive application and worker restarts.
- An expired SQS visibility timeout is redelivered without allowing stale completion or duplicate stored results.
- A target timeout becomes one unhealthy result and is not retried as an internal job failure.
- Queue hard limits and dead jobs produce visible operational alerts.
- Alert and recovery emails are not lost when email delivery is temporarily unavailable.
- Private IP, metadata, redirect, and DNS-rebinding safety tests pass.
- Cross-organization access tests pass for every resource.
- The system sustains its stated check rate in an ARM64 load test.
- A PostgreSQL backup has been restored successfully.
- Missing checks appear as unknown, not healthy.

---

## 9. Phase 2 — Distributed Tracing and Real-Traffic Monitoring

### 9.1 Goal

Add distributed tracing as a core product feature while staying on Oracle Free Tier for personal use and a small closed beta.

At the end of Phase 2, a developer can:

1. Create a project API key.
2. Configure an OpenTelemetry SDK or Collector.
3. Send traces to our standard OTLP endpoint.
4. Search recent traces.
5. Open a trace and see its waterfall.
6. See which services call each other.
7. Find slow and failed operations.
8. Create alerts from real traffic error rate or latency.
9. Link uptime incidents to traces from the same service and time window.

### 9.2 How applications send traces

Applications use an official OpenTelemetry SDK or a local OpenTelemetry Collector.

Example configuration:

```text
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.example.com
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20PROJECT_API_KEY
OTEL_SERVICE_NAME=payment-service
```

Phase 2 supports OTLP over HTTPS with Protocol Buffers first:

```text
POST /v1/traces
```

OTLP defines `/v1/traces`, `/v1/metrics`, and `/v1/logs`. Traces are implemented first; metrics and logs arrive in Phase 3. Reference: [OTLP specification](https://opentelemetry.io/docs/specs/otlp/).

We will publish setup guides for Go, Java, Node.js, Python, and .NET. These guides use official OpenTelemetry packages, not a custom SDK maintained by us.

Applications must propagate the standard W3C trace context between services; otherwise the platform will receive separate unrelated traces instead of one connected request. `service.name` is required. The project API key decides the organization and environment, so a client-provided environment attribute can never move data into another environment.

### 9.3 Ingestion flow

```text
Customer application or Collector
          │ compressed OTLP batch
          ▼
Trace ingestion endpoint
          │
          ├── Validate API key and environment
          ├── Check request size and rate limit
          ├── Decode OTLP
          ├── Remove blocked or oversized attributes
          ├── Build trace and span records
          └── Save one batch transaction
          │
          ▼
PostgreSQL trace partitions
```

The endpoint returns success only after the accepted batch is safely stored. Temporary database failures return a retryable response so OpenTelemetry exporters or Collectors can retry.

Exporters may retry the same batch, and one trace may arrive in several batches or in a different order. A span is therefore unique by environment, trace ID, and span ID. Repeated spans update the existing record instead of creating duplicates. A trace summary remains open for a short quiet period and is then marked complete or incomplete based on the spans received.

Using a Collector beside the customer application is recommended because it can batch, retry, filter, and remove sensitive data before sending. Reference: [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/).

### 9.4 Free-tier limits

Tracing creates much more data than uptime checks. Phase 2 therefore has strict pilot limits:

| Item | Starting pilot limit |
|---|---:|
| Sustained accepted spans platform-wide | 10 spans/second |
| Short burst | 50 spans/second for 10 seconds |
| Raw trace retention | 3 days |
| Trace summaries | 30 days |
| Maximum compressed request | 1 MB |
| Maximum spans in one request | 2,000 |
| Maximum attributes per span | 64 |
| Maximum stored attribute value | 2 KB |

These values are configuration, not permanent product promises. Load and disk tests determine final free-beta limits.

Customers must enable sampling. For example, they may keep 10% of normal requests while keeping more errors in a local Collector. The platform also rejects data after project limits are reached instead of allowing the VM to run out of disk.

### 9.5 Trace storage

PostgreSQL uses time partitions for `traces`, `spans`, and `span_events`.

A trace summary stores:

- trace ID;
- environment and root service;
- root operation;
- start and end time;
- total duration;
- success or error state;
- number of spans; and
- whether the trace may be incomplete.

A span stores:

- trace ID and span ID;
- parent span ID;
- service and operation name;
- start time and duration;
- client/server/internal type;
- success or error state;
- selected attributes; and
- selected events.

We do not store every possible attribute without limits. Passwords, authorization headers, cookies, database parameters, personal data, and configured blocked keys are removed before storage.

### 9.6 Trace pages

Phase 2 UI pages:

- Trace search by time, service, operation, duration, status, and trace ID.
- Trace detail waterfall.
- Span detail with safe attributes and events.
- Services list with request count, error rate, and latency.
- Service map showing recent calls between services.
- Slow operations page.
- Error operations page.
- Telemetry usage and rejected-data page.

Phase 2 management and query endpoints:

```text
GET    /api/v1/environments/:environmentId/api-keys
POST   /api/v1/environments/:environmentId/api-keys
DELETE /api/v1/environments/:environmentId/api-keys/:keyId

GET /api/v1/environments/:environmentId/services
GET /api/v1/environments/:environmentId/services/:serviceId
GET /api/v1/environments/:environmentId/traces
GET /api/v1/environments/:environmentId/traces/:traceId
GET /api/v1/environments/:environmentId/service-map
GET /api/v1/environments/:environmentId/telemetry/usage

GET  /api/v1/environments/:environmentId/telemetry-alert-rules
POST /api/v1/environments/:environmentId/telemetry-alert-rules
PUT  /api/v1/environments/:environmentId/telemetry-alert-rules/:ruleId
```

### 9.7 Service map

The service map is calculated from parent and child spans belonging to different services.

```text
order-service ── 2,400 calls ──▶ payment-service
      │                              │
      └── 0.5% errors                └── p95 420 ms
```

An hourly job updates call count, error count, and latency summaries. The UI does not scan all raw spans every time it loads the map.

### 9.8 Real-traffic alerts

Phase 2 adds three alert types:

- Error rate above a percentage for a time window.
- p95 latency above a limit for a time window.
- No telemetry received for a service that normally sends data.

Example:

```text
Alert when payment-service error rate is above 5%
for 5 minutes, with at least 100 requests.
```

The minimum request count prevents an alert based on one failed request. Recovery also requires a complete healthy window.

### 9.9 Uptime and trace connection

Monitors may be linked to one service. When an uptime incident opens, its detail page shows:

- failed synthetic checks;
- traces from the linked service around the incident start;
- slow operations during that window; and
- error changes before and after the incident.

This is time-based help for investigation. It is not proof that a particular trace caused the uptime incident.

### 9.10 Phase 2 deployment

Containers on the same free VM:

```text
nginx
api
checker
ingester
notifier
postgres
otel-collector (for monitoring our own platform)
```

Each container receives a memory limit. PostgreSQL and ingestion have priority. The system must stop accepting trace data before low disk or memory threatens uptime monitoring and account access.

### 9.11 Phase 2 build order

1. Project API keys with prefix, hashed secret, scopes, expiry, and rotation.
2. OTLP/HTTP trace receiver using official OpenTelemetry definitions.
3. Request limits, usage counters, attribute filtering, and safe errors.
4. Partitioned trace, span, and event storage.
5. Trace summary processor.
6. Trace search and waterfall APIs.
7. Trace search and waterfall UI.
8. Service discovery and service pages.
9. Service map and hourly trace summaries.
10. Error-rate, latency, and no-data alert rules.
11. Uptime monitor to service linking.
12. Setup guides and sample instrumented applications.
13. Trace ingestion load, retention, privacy, and recovery tests.

### 9.12 Phase 2 completion rules

Phase 2 is complete when:

- At least three sample services can produce one connected trace.
- Go, Java, Node.js, Python, and .NET setup examples send valid traces.
- A user can search a trace and identify its slowest span.
- Invalid or expired project keys cannot ingest data.
- Cross-project trace searches are impossible.
- Trace limits protect uptime monitoring during an ingestion burst.
- Sensitive test attributes are removed before storage.
- Real-traffic alerts open and recover correctly.
- Trace partitions expire automatically without blocking ingestion.

---

## 10. Phase 3 — Startup Scale and Full Observability

### 10.1 When Phase 3 begins

We start paying for infrastructure when one or more of these conditions is true:

- Free VM CPU remains above 70% during ordinary traffic.
- PostgreSQL storage remains above 70% after retention cleanup.
- Check scheduler delay repeatedly breaks its target.
- Trace ingestion regularly rejects valid customer traffic.
- A VM restart causes unacceptable customer impact.
- Customers require monitoring from more than one location.
- A growing number of paying beta customers need predictable availability.

We scale before the free environment becomes an emergency.

### 10.2 Goal

Turn the working product into a scalable startup platform.

Phase 3 adds:

- Multiple API and worker instances.
- Checks from several regions.
- ClickHouse for large monitoring and telemetry data.
- A durable event buffer for ingestion spikes.
- OpenTelemetry metrics and logs.
- Slack and signed webhook alerts.
- Public status pages.
- Usage measurement and plan limits.
- High availability for the main database and API.

### 10.3 Separate control and data work

The system now has two broad areas.

**Control area:** accounts, projects, monitor settings, alert rules, incidents, permissions, and billing records.

**Data area:** check jobs, check results, spans, metrics, logs, summaries, and large searches.

PostgreSQL remains the main database for the control area. ClickHouse becomes the main database for the large data area.

### 10.4 Service split

The modular Go codebase is separated into deployable programs only where independent scaling is useful:

```text
api-service
scheduler-service
check-worker
telemetry-gateway
trace-processor
metrics-processor
log-processor
alert-evaluator
notification-worker
rollup-worker
```

These programs remain in the backend monorepo. Splitting backend programs for deployment or scaling does not require a repository per service; the React application is the only separately versioned codebase.

### 10.5 Check scheduling at scale

Phase 1 already provides SQS delivery, PostgreSQL job identity, and transactional dispatch outboxes. Phase 3 keeps the same job identity and at-least-once rules while adding regional routing and, only if measured throughput requires it, migrating to a higher-volume broker:

1. Scheduler instances claim due monitors through PostgreSQL leases.
2. A transactional outbox records the broker message before the schedule transaction is acknowledged.
3. A dispatcher publishes jobs to the external durable broker.
4. Regional workers consume jobs intended for their region.
5. Results are published and stored in batches.
6. Idempotent job and result IDs make broker replay safe.

Any Phase 3 broker migration runs SQS and replacement-broker paths in a controlled compatibility period. Dispatch intent must remain durably recorded before broker publication, and results remain idempotent by stable job ID.

Multi-region checks include at least three probe locations. Alert rules can require failure from two locations before opening an incident, which reduces alerts caused by one regional network problem.

### 10.6 Trace ingestion at scale

```text
Applications
     │
     ▼
Load balancer
     │
     ▼
OTLP gateway instances
     │
     ▼
Durable event buffer
     │
     ▼
Trace processors
     │
     ▼
ClickHouse
```

Gateway instances authenticate and apply hard limits. Processors build trace summaries, service links, and alert data.

If we add server-side tail sampling—choosing whether to keep a trace after seeing its result—all spans for the same trace must reach the same sampling worker. This routing is based on trace ID. OpenTelemetry documents this gateway pattern: [Collector gateway deployment](https://opentelemetry.io/docs/collector/deploy/gateway/).

### 10.7 Metrics and logs

Add the standard OTLP endpoints:

```text
POST /v1/metrics
POST /v1/logs
```

Metrics features:

- Request rate, error rate, latency, CPU, memory, and custom application measurements.
- Dashboards and threshold alerts.
- Controlled label limits to prevent unexpected storage growth.

Log features:

- Search by service, time, level, trace ID, and selected fields.
- Open logs related to a trace.
- Strict limits on line size, fields, retention, and sensitive values.

The initial logs product is structured log search, not an unlimited file-storage service.

### 10.8 Integrations

Phase 3 notification channels:

- Email.
- Slack.
- Discord if customer demand exists.
- Signed outgoing webhooks.

Webhook safety requires HTTPS, destination checks, signed payloads, retry limits, and protection against calls to private/internal addresses.

### 10.9 Public status pages

Customers can publish current component status, active incidents, and incident history.

Features:

- Public URL.
- Project components.
- Automatic incident connection.
- Manual maintenance message.
- Uptime history.
- Custom domain later in Phase 4.

Status-page reads are cached and separated from the main authenticated API so public traffic cannot overload customer dashboards.

### 10.10 Capacity milestones

Capacity is increased in tested steps.

| Milestone | Monitors | Trace ingestion | Meaning |
|---|---:|---:|---|
| Phase 3A | 10,000 at 60 seconds | 1,000 spans/second | First paid startup architecture |
| Phase 3B | 100,000 at 60 seconds | 10,000 spans/second | Proven horizontal scaling |
| Phase 3C | Selected 30-second plans | Based on demand | Requires another capacity test |

The original goal of 100,000 monitors at a 30-second interval means about 3,333 checks per second. It is not advertised until a failure-storm test proves the full path can handle it.

### 10.11 Data retention

Retention becomes plan-based.

Example starting plans:

| Data | Free/Developer | Team | Business |
|---|---:|---:|---:|
| Raw checks | 7 days | 30 days | 90 days |
| Traces | 3 days | 14 days | 30 days |
| Logs | 1 day | 7 days | 30 days |
| Metric detail | 7 days | 30 days | 90 days |
| Daily summaries | 1 year | 2 years | 3 years |

Final commercial plans are a business decision and can change without changing the storage design.

### 10.12 Phase 3 build order

1. Measure Phase 2 bottlenecks and define paid capacity needs.
2. Move PostgreSQL to a highly available setup with tested backups.
3. Introduce ClickHouse and copy telemetry with a verified migration pipeline.
4. Keep Amazon SQS or migrate to a measured higher-volume broker with the same transactional outbox, replay tests, and rollback contract.
5. Split and scale checker and ingestion programs.
6. Add multi-region check workers.
7. Add OTLP metrics and metrics dashboards.
8. Add limited structured log ingestion and search.
9. Add Slack and signed webhooks.
10. Add public status pages.
11. Add daily usage measurement and enforced plan limits.
12. Run regional failure, broker outage, database failure, and restore exercises.

### 10.13 Phase 3 completion rules

- No single checker or ingestion process can stop the entire data path.
- API instances can be added without changing customer configuration.
- A regional check-worker failure is visible and recovered automatically.
- Event buffering protects short database outages without unlimited growth.
- ClickHouse retention removes old data automatically.
- Cross-tenant tests cover every telemetry signal.
- Usage totals match stored and rejected data closely enough for future billing.
- The platform meets the Phase 3A load target before accepting corresponding customers.

---

## 11. Phase 4 — Mature SaaS and Enterprise Product

### 11.1 Goal

Turn the startup platform into a dependable commercial product for larger teams.

Phase 4 focuses on customer operations, enterprise security, predictable availability, and advanced investigation.

### 11.2 Billing and plans

- Stripe subscriptions and invoices.
- Free, Team, Business, and Enterprise plans.
- Usage limits for checks, spans, metrics, logs, users, and retention.
- Trial periods and safe downgrade behavior.
- Usage dashboard and warning emails.
- Internal admin tools for credits and support adjustments.

Billing is based on the trusted usage records created in Phase 3, not expensive live counts over telemetry tables.

### 11.3 Enterprise login and user management

- Google and GitHub login for ordinary teams.
- SAML/OIDC single sign-on for enterprise customers.
- Automatic user creation and removal from the customer identity system.
- Required multi-factor authentication.
- Session and API-key management UI.
- Detailed audit-log export.

### 11.4 On-call and escalation

Customers create schedules describing who is responsible.

Example:

```text
Incident opens
   │
   ├── Immediately: email and Slack primary engineer
   ├── After 10 minutes unacknowledged: notify backup engineer
   └── After 20 minutes: notify team lead
```

Features:

- On-call schedules and rotations.
- Escalation steps.
- Maintenance windows.
- Alert grouping and quiet periods.
- Incident comments and handoff notes.
- Mobile push or SMS only through an explicitly priced provider.

### 11.5 Private monitoring agents

Some customers need to monitor APIs that are not public.

We provide a small customer-installed agent that:

- makes checks from inside the customer network;
- opens an outbound encrypted connection to our platform;
- receives only jobs belonging to that customer;
- sends results back in batches;
- can be revoked and upgraded safely; and
- never requires inbound firewall access.

Private agents use short-lived credentials and are visible in an agent health page.

### 11.6 Advanced analysis

The platform connects uptime checks, traces, metrics, and logs by service and time.

Features:

- Detect unusual changes compared with the service’s normal pattern.
- Suggest likely slow services during an incident.
- Compare a failing release with the previous release.
- Link traces to related logs and metrics.
- Group similar errors.
- Show deployment markers on charts.

These features begin with clear statistical rules. Machine learning is added only when it improves results and can explain why it made a suggestion.

### 11.7 Enterprise controls

- Customer-selected retention periods.
- Data-region selection where infrastructure exists.
- Customer-managed encryption keys for large contracts.
- IP allowlists.
- Fine-grained project roles.
- Export and deletion tools.
- Security and privacy documentation.
- Vulnerability management and incident response process.
- Work toward SOC 2 or similar assurance when customer demand justifies the cost.

### 11.8 Availability and disaster recovery

Phase 4 removes major single points of failure:

- API and ingestion instances in multiple failure zones.
- Replicated PostgreSQL with automatic failover.
- Replicated ClickHouse data according to plan.
- Event buffer with multiple brokers.
- Backups copied to another region.
- Regular recovery exercises.
- Public platform status page.

An availability SLA such as 99.9% or 99.95% is offered only after monitoring proves the platform can meet it and legal/business review defines credits and exclusions.

### 11.9 Phase 4 completion rules

- Billing and plan enforcement agree with usage records.
- Enterprise login can add and remove users safely.
- An on-call escalation works from incident creation through acknowledgement.
- A private agent cannot receive another customer’s work.
- A regional recovery exercise meets the published recovery target.
- Customer export and deletion requests are tested.
- Security review finds no critical unresolved issue.
- Any published SLA is supported by at least several months of measured availability.

---

## 12. Security Requirements for Every Phase

### 12.1 Passwords and sessions

- Hash passwords with Argon2id or a properly configured bcrypt fallback.
- Access tokens last about 15 minutes.
- Refresh tokens are random, rotated, and stored only as a digest.
- Reuse of an old rotated token revokes the token family.
- Browser refresh tokens use Secure, HttpOnly cookies.
- Organization roles are read from the server, not trusted from old token claims.

### 12.2 Monitor URL safety

For every check and redirect:

- allow only HTTP and HTTPS;
- resolve and validate all IPv4 and IPv6 addresses;
- reject private, loopback, link-local, multicast, and metadata destinations;
- validate the actual IP immediately before connection to stop DNS rebinding;
- allow only ports 80 and 443 in Phase 1;
- limit redirects, headers, response bytes, and total time;
- strip secrets when the redirect host changes; and
- never disable TLS certificate validation in production.

### 12.3 Secrets

- Never store plaintext monitor authorization headers.
- Keep encryption keys in OCI Vault first and a dedicated key service later.
- Cache key versions safely so every check does not call Vault.
- Redact tokens, cookies, authorization values, database statements, and personal data from logs.
- Store API keys with a visible prefix and a non-reversible secret digest.

### 12.4 Trace privacy

- Publish a default blocked-attribute list.
- Limit attribute count, key size, value size, event count, and request size.
- Allow customers to add project-specific blocked keys.
- Encourage filtering in the customer’s OpenTelemetry Collector.
- Do not treat trace data as harmless; it may contain URLs, user IDs, and error details.
- Support deletion and retention from the beginning, even before enterprise features.

### 12.5 Tenant isolation

- Every request is connected to an organization and environment.
- Every query includes the organization boundary.
- Database constraints connect child rows to parents from the same tenant.
- Background jobs carry tenant identifiers and validate them again before writes.
- Tests deliberately attempt to access data from another tenant.

---

## 13. Reliability and Data Rules

### 13.1 Source of truth

PostgreSQL is the source of truth for customer configuration, permissions, schedules, check jobs, incidents, notification work, and billing records.

Live events, caches, and in-memory queues may improve speed, but losing them must not corrupt important business state.

### 13.2 Unknown data

Uptime reports include both uptime and monitoring coverage:

```text
Observed uptime = successful checks / observed checks
Coverage        = observed checks / expected checks
Unknown checks  = expected checks - observed checks
```

A period with missing checks is incomplete. It is never silently shown as 100% available.

### 13.3 Time storage

- Store timestamps in UTC.
- Display them in the user’s selected timezone.
- Record scheduled, started, and completed times separately.
- Treat application clock differences carefully when displaying distributed spans.

### 13.4 Backups

Phase 1 and Phase 2:

- Nightly logical backup of important control and incident data.
- OCI block-volume backups for full database recovery.
- Encrypted copy in OCI Object Storage within current free limits.
- Monthly restore exercise.

Phase 3 and Phase 4:

- Automated PostgreSQL continuous backup.
- ClickHouse backup according to customer retention.
- Cross-region copies.
- Written and tested recovery procedures.

### 13.5 Self-monitoring

The platform records:

- scheduler delay and queue size;
- completed, failed, and missed checks;
- trace batches and spans accepted/rejected;
- database connection and query delay;
- notification queue age;
- disk, memory, and CPU pressure;
- last successful backup;
- data-retention job status; and
- live endpoint readiness.

Successful checks and spans are data records, not a reason to produce one application log line each.

---

## 14. Testing Strategy

### 14.1 Unit tests

- Scheduler ordering and missed-check behavior.
- Alert open, acknowledge, manual resolve, and automatic recovery.
- URL and IP blocking.
- Token and API-key rotation.
- Attribute filtering and trace summary logic.
- Usage and plan-limit calculations.

### 14.2 Database tests

- SQLC queries against real PostgreSQL.
- Organization isolation.
- One-open-incident rule.
- Notification claiming and retry.
- Partition creation and deletion.
- Rollup correctness.
- Concurrent schedule and incident updates.

### 14.3 API and UI tests

- Full signup-to-monitor flow.
- Full API-key-to-trace flow.
- Role permissions.
- Live update and polling fallback.
- Trace search and waterfall rendering.
- Billing and enterprise login when those phases begin.

### 14.4 Load tests

Use local controlled target APIs. Do not load-test random public websites.

Test:

- ordinary fast checks;
- 100 concurrent timeouts;
- scheduler restart with overdue monitors;
- trace batches at normal and burst rates;
- database slowdown;
- event-buffer backlog in Phase 3; and
- popular public status-page traffic.

### 14.5 Recovery and security tests

- Restore the database to a clean environment.
- Stop the email provider and verify later retry.
- Stop one worker region and verify recovery.
- Attempt private-IP and metadata requests through direct URLs, DNS, and redirects.
- Send oversized and deeply nested trace payloads.
- Verify secrets never appear in logs or API output.
- Attempt cross-organization access for every resource type.

---

## 15. Repository Structure

Use two Git repositories. The backend remains a monorepo containing every Go command, shared backend module, database asset, test suite, and deployment definition. The frontend repository contains the React application and its browser-facing tests.

Backend repository:

```text
api-monitor-backend/
├── cmd/
│   ├── api/
│   ├── checker/
│   ├── ingester/
│   ├── notifier/
│   └── worker/
├── internal/
│   ├── auth/
│   ├── organization/
│   ├── project/
│   ├── monitor/
│   ├── checker/
│   ├── alert/
│   ├── incident/
│   ├── notification/
│   ├── telemetry/
│   │   ├── otlp/
│   │   ├── trace/
│   │   ├── metric/
│   │   └── log/
│   ├── dashboard/
│   ├── realtime/
│   ├── retention/
│   ├── usage/
│   └── platform/
│       ├── crypto/
│       ├── database/
│       ├── clock/
│       ├── response/
│       └── observability/
├── db/
│   ├── migrations/
│   ├── queries/
│   └── sqlc.yaml
├── deploy/
│   ├── local/
│   ├── oracle-free/
│   └── production/
├── examples/
│   └── opentelemetry/
├── tests/
│   ├── integration/
│   ├── e2e/
│   └── load/
├── docs/
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── README.md
```

Frontend repository:

```text
api-monitor-frontend/
├── src/
├── public/
├── tests/
│   ├── component/
│   └── e2e/
├── docs/
├── Dockerfile
├── package.json
└── README.md
```

The repositories integrate through the documented, versioned HTTP and Server-Sent Events contract. Backend changes that affect the frontend must update the OpenAPI contract and pass compatibility checks before merge. Each repository has independent CI. Deployment definitions remain in the backend monorepo and consume a pinned frontend build artifact or image rather than a frontend source directory.

Rules:

- HTTP handlers call application services, not SQL directly.
- Modules do not read another module’s tables through hidden queries.
- Shared platform code contains technical helpers, not product rules.
- Commands may be deployed together in Phase 1 or separately later.
- Backend commands and modules remain in the backend monorepo; do not create a repository per service.
- The frontend repository accesses backend behavior only through the documented API contract.
- Service separation must not duplicate the same business rules in several places.

---

## 16. Important Decisions Already Made

1. **We are building both uptime monitoring and distributed tracing.** Tracing is Phase 2, not an optional future idea.
2. **OpenTelemetry is the customer integration standard.** We will not maintain custom language SDKs.
3. **Phase 1 is a modular Go application.** Microservices are not required to demonstrate good architecture.
4. **PostgreSQL handles the first two phases.** ClickHouse is added when paid scale and measured data volume justify it.
5. **Redis is not needed on one application instance.** It may be added in Phase 3 for shared short-lived state.
6. **Phase 1 uses Amazon SQS Standard queues for durable delivery and PostgreSQL outboxes/job ledgers for atomic state.** Kafka is not required. A different broker is considered only when measured regional delivery or telemetry throughput outgrows SQS or needs broker features SQS does not provide.
7. **GET and HEAD are the only Phase 1 check methods.** Side-effecting synthetic workflows require a later safety design.
8. **Missing checks are unknown, not healthy.** Coverage is shown with uptime.
9. **The free deployment is a beta, not a 99.9% SLA.** A commercial SLA comes only after paid high-availability infrastructure is proven.
10. **All four phases remain in one product plan.** The infrastructure changes over time, but organizations, projects, environments, services, incidents, and OpenTelemetry remain stable concepts.

---

## 17. First Implementation Milestone

Development should begin with this small vertical slice:

```text
Create user
   │
Create organization, project, and production environment
   │
Create one GET monitor
   │
Scheduler commits a stable job and SQS dispatch intent
   │
Dispatcher publishes to SQS
   │
Worker receives and executes the job under a visibility timeout
   │
Result appears in PostgreSQL
   │
Dashboard shows healthy or failed
```

Do not begin by building every authentication screen or all future database tables. Build this vertical slice with clean migrations and tests, then add alerting, incidents, email, and the rest of Phase 1 in the documented order.

The first milestone is complete when it works locally through Docker Compose and the same ARM64 images can run on Oracle Cloud.

---

## 18. Final Product Direction

The finished platform will combine:

- outside-in API uptime checks;
- inside-out distributed traces;
- application metrics and logs;
- incident detection and communication;
- public status pages;
- team and on-call workflows; and
- usage-based SaaS plans.

The four phases protect us from two common mistakes: building a toy that cannot grow, and building expensive distributed infrastructure before anyone uses the product.

Phase 1 proves reliable monitoring. Phase 2 proves the central distributed-tracing idea. Phase 3 proves that the architecture can support startup traffic. Phase 4 turns the proven system into a dependable commercial product.
