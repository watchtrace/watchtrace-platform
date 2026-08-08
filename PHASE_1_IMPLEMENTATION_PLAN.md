# Phase 1 Implementation Plan

## Document Purpose

This document is the working checklist for Phase 1 of the real-time API monitoring platform.

- `DESIGN_SPECIFICATION.md` defines what the product must do and how it should grow.
- `RISKS_AND_CAVEATS.md` records important risks and safety controls.
- This document defines the order in which Phase 1 will be implemented and verified.
- GitHub Issues may contain more detailed work, but their task IDs should match this document.

Phase 1 is complete only when every release-gate item near the end of this document passes. Checking every feature box is not enough if the system is unsafe or unreliable.

The implementation order is deliberate: build and verify the complete Go backend first, freeze its API contract, and only then build the React frontend. Frontend tasks must not begin before the backend completion gate passes.

---

## 1. Phase 1 Outcome

At the end of Phase 1, a user must be able to:

1. Create and verify an account.
2. Create an organization, project, and environment.
3. Add a safe HTTP or HTTPS monitor.
4. See scheduled checks and latency information on a dashboard.
5. Receive an email after repeated failures.
6. Acknowledge an incident.
7. See the incident resolve after the monitored API recovers.
8. See missing checks as unknown coverage instead of successful uptime.
9. Restart the application without losing checks that were waiting to run.

The system will run locally with Docker Compose and deploy to one Oracle Cloud Always Free ARM64 VM for personal use and a small beta.

---

## 2. Phase 1 Technical Boundaries

### We will build

- Two Git repositories: one backend repository and one frontend repository.
- One modular Go application in a backend monorepo.
- A React and TypeScript dashboard in the frontend repository.
- PostgreSQL as the source of truth.
- PostgreSQL-backed durable queues for check jobs and notification work.
- Server-Sent Events with a polling fallback.
- Docker Compose for local and Oracle Cloud deployment.
- OCI Email Delivery, Vault, Object Storage, and other free OCI services where suitable.

The Go application may contain API, scheduler, checker, incident, notification, and live-event modules. These are code modules, not independent microservices in Phase 1. All backend modules and commands remain in the backend monorepo. They may run in one application process initially and be separated later without duplicating business rules or creating a repository per service. Scheduler and checker modules communicate through durable PostgreSQL job rows even when they share a process; an in-memory channel is not the source of truth for queued work.

The React application lives in the separate frontend repository. The two repositories integrate only through the documented HTTP and Server-Sent Events contract. Each repository has independent CI. The backend monorepo owns database migrations, backend deployment definitions, and the versioned OpenAPI contract; the frontend repository consumes that contract and produces a versioned build artifact or image for deployment.

### We will not build in Phase 1

- Distributed tracing. It begins in Phase 2.
- Metrics or log ingestion products.
- Multi-region monitoring.
- Redis, Kafka, ClickHouse, Kubernetes, or another external queue server. PostgreSQL provides the Phase 1 durable queues.
- Billing, paid plans, enterprise login, or public status pages.
- POST, PUT, PATCH, or DELETE synthetic checks.
- Response-body storage.
- A commercial availability guarantee.

### Durable queue contract

Phase 1 uses the `check_jobs` table as its check queue. This contract should be implemented before adding scheduler optimizations:

| Part | Phase 1 rule |
|---|---|
| Job identity | Every job has a stable UUID. `health_checks.job_id` is unique, so retrying a job cannot store a second final result. |
| Job types | `scheduled` and `manual_test`. Scheduled work has higher priority. |
| States | `pending`, `running`, `completed`, `cancelled`, and `dead`. |
| Claim | A worker changes a ready `pending` job to `running` in a short transaction and records its worker ID, a new lease token, and lease expiry. |
| Completion | Only the worker holding the current lease token may finish the job. The result and completed state are committed together. |
| Recovery | A detected internal failure returns the job to `pending` with a short bounded delay. A crash is recovered after lease expiry. After three internal attempts the job becomes `dead`. |
| Target failure | DNS, connection, TLS, timeout, or unexpected HTTP status is stored once as an unhealthy result; it is not retried as an internal error. |
| Stale work | Work for a paused, deleted, or changed monitor becomes `cancelled` before a request is sent. |
| Scheduling | Creating a scheduled job and advancing `next_check_at` happen in one database transaction. |
| Wake-up signal | PostgreSQL notifications or an in-process signal may wake workers faster, but polling the table must preserve correctness if that signal is lost. |
| Cleanup | Completed jobs are removed after 48 hours; dead and cancelled jobs after seven days. Monitoring results follow their separate retention rules. |

Allowed state changes:

```text
pending -> running -> completed
pending -> cancelled
running -> cancelled
running -> pending   (internal error or expired lease; attempts remain)
running -> dead      (internal error or expired lease; attempt limit reached)
```

This gives at-least-once request execution and at-most-one stored result per job ID. It does not promise exactly-once HTTP requests.

Email delivery uses a separate durable `notification_outbox` table with its own delivery ID, lease, retry time, attempt limit, and final failed state. A check job and an email job must not share state or retry rules. Milestone 4 defines this flow.

---

## 3. Working Method

### 3.1 Task states

Use these states in GitHub Project:

```text
Backlog -> Ready -> In Progress -> Review -> Done
```

Only one major task should normally be in progress for a single developer. A second task may be active only when it is independent, such as documentation or tests that do not overlap the same files. Do not use parallel work as a reason to start React before the backend completion gate.

### 3.2 Definition of done for every task

A task may be marked complete only when:

- Its acceptance criteria pass.
- Relevant unit or integration tests have been added.
- Existing tests still pass.
- Errors and logs do not expose secrets.
- Tenant-owned queries enforce the organization boundary.
- Documentation or configuration examples are updated when behavior changes.
- The implementation does not add an out-of-scope Phase 2 or Phase 3 dependency.
- The change has been reviewed before merging into `main`.

### 3.3 Task ID rules

Use the same ID in the checklist, GitHub Issue, branch, pull request, and commit where practical.

Example:

```text
Issue:  P1-104 Validate monitor destinations
Branch: p1-104-monitor-url-safety
PR:     P1-104: Validate monitor destinations
```

### 3.4 Implementation rule

Build one small backend path first. Do not finish every database table or future service before executing the first real monitor check through the API.

Use this order:

```text
Backend foundation
  -> Backend monitoring slice
  -> Accounts and tenant safety
  -> Durable monitoring engine
  -> Incidents and email
  -> Complete and freeze backend API
  -> React frontend
  -> Oracle deployment and release verification
```

During backend milestones, use API integration tests, command-line HTTP requests, and controlled test servers instead of temporary UI pages. Do not create throwaway server-rendered pages. All customer-facing Phase 1 screens will be implemented once in React after the backend gate passes.

---

## 4. Milestone 0 — Backend Repository and Foundation

### Goal

Create a repeatable local development environment and a clean foundation for later milestones.

### Checklist

- [x] **P1-001 — Create the backend Git repository and basic project files**
  - Add `README.md`, `.gitignore`, license decision, contribution notes, and editor settings.
  - Document the required local tools and first-run commands.
  - Document that this repository is the backend monorepo and that the React application will live in a separate frontend repository.

- [x] **P1-002 — Initialize the Go application**
  - Create the Go module and the initial application entry point.
  - Add a small health endpoint and graceful shutdown.
  - Establish `cmd`, `internal`, `db`, `deploy`, `tests`, and `docs` directories as they become necessary.

- [x] **P1-003 — Add PostgreSQL with Docker Compose**
  - Start PostgreSQL locally with a health check and persistent development volume.
  - Keep local credentials separate from production credentials.

- [x] **P1-004 — Add application configuration**
  - Load configuration from environment variables.
  - Validate required values during startup.
  - Provide a safe `.env.example` containing no real secrets.

- [x] **P1-005 — Add migrations and SQLC**
  - Add repeatable database migration commands.
  - Configure SQLC and prove one generated query works against PostgreSQL.
  - Add migration tests against a real test database.

- [x] **P1-006 — Add standard API behavior**
  - Add request IDs, structured error responses, JSON validation, panic recovery, and safe logging.
  - Add readiness and liveness endpoint conventions.

- [x] **P1-007 — Add backend continuous integration**
  - Build and test the Go application.
  - Run database-backed tests with PostgreSQL.

- [x] **P1-008 — Create the backend container image**
  - Add a production-style multi-stage build for the Go application.
  - Build for both local development and Linux ARM64.
  - Run containers as a non-root user where practical.

### Milestone 0 gate

- [ ] A new developer can start PostgreSQL and the application from the README.
- [ ] The backend builds and its tests pass locally and in CI.
- [ ] A migration can be applied to a clean database.
- [ ] The Linux ARM64 backend image builds successfully.
- [ ] No real secret is committed to the backend repository.

---

## 5. Milestone 1 — First Backend Monitoring Slice

### Goal

Prove the product's central path as early as possible:

```text
User -> Organization -> Project -> Environment -> Monitor
  -> Durable PostgreSQL check job -> Leased worker
  -> Stored HTTP result -> Authorized API response
```

Account handling in this milestone may be minimal, but passwords must still be hashed securely. Public beta account features are completed in Milestone 2. Verify this slice with API and database integration tests; do not build React pages yet.

### Checklist

- [ ] **P1-101 — Add the minimum account and ownership schema**
  - Add users, organizations, memberships, projects, and environments.
  - Use UUIDs or another documented non-sequential public identifier.
  - Store timestamps in UTC.

- [ ] **P1-102 — Implement minimal signup and login**
  - Hash passwords with Argon2id or the approved bcrypt fallback.
  - Return a secure authenticated session suitable for local development.
  - Do not log passwords, tokens, or cookies.

- [ ] **P1-103 — Create the default ownership path**
  - Let a new user create an organization, project, and production environment.
  - Ensure every child record belongs to the correct organization.

- [ ] **P1-104 — Add the initial monitor schema and API**
  - Create and list a GET monitor with URL, interval, timeout, and expected status range.
  - Default to a five-minute interval and a five-second timeout.
  - Enforce organization and environment ownership.

- [ ] **P1-105 — Add destination safety before making requests**
  - Allow only HTTP and HTTPS on ports 80 and 443.
  - Reject private, loopback, link-local, multicast, and cloud metadata destinations for IPv4 and IPv6.
  - Add tests using controlled local DNS and target servers.

- [ ] **P1-106 — Implement the first durable scheduler path**
  - Add the initial `check_jobs` table and indexes.
  - Load due monitors from PostgreSQL in a small batch.
  - Insert one durable job and advance the monitor schedule in the same transaction.
  - Prevent duplicate jobs with a unique monitor-and-scheduled-time key.
  - Record scheduled time separately from start and completion time.

- [ ] **P1-107 — Execute and store an HTTP check**
  - Claim a pending job with a worker ID and lease using `FOR UPDATE SKIP LOCKED`.
  - Apply the monitor timeout and expected status range.
  - Idempotently store at most one final success or failure, status code, error category, and total duration per job ID.
  - Mark the job completed in the same result transaction.
  - Do not save the response body.

- [ ] **P1-108 — Add the first monitoring read API**
  - Return the monitor's current state and recent check results.
  - Scope every query to the authenticated organization and environment.

- [ ] **P1-109 — Add a backend vertical-slice integration test**
  - Create an account and ownership records.
  - Create a monitor through the API pointing to a controlled test server.
  - Wait for one scheduled result and verify it through the authorized API.

### Milestone 1 gate

- [ ] The complete backend slice works through Docker Compose without manual database edits.
- [ ] The API reports a successful target as healthy.
- [ ] The API reports a controlled failed target as failed.
- [ ] The response body is neither stored nor logged.
- [ ] Basic private-network destination tests pass.
- [ ] A pending check job remains available after an application restart.
- [ ] Restarting the application does not delete monitor configuration or results.

---

## 6. Milestone 2 — Accounts, Organizations, and Tenant Safety

### Goal

Complete the identity and ownership features required for a safe multi-user beta.

### Checklist

- [ ] **P1-201 — Implement production access and refresh sessions**
  - Use short-lived access tokens.
  - Use random, rotated refresh tokens stored only as digests.
  - Put browser refresh tokens in Secure, HttpOnly cookies in production.
  - Revoke the token family when a rotated token is reused.

- [ ] **P1-202 — Implement logout and session revocation**
  - Support current-session and all-session logout.
  - Clean up expired and revoked session records.

- [ ] **P1-203 — Implement email verification**
  - Store only a digest of the verification token.
  - Expire tokens and prevent reuse.
  - Provide a safe local email-development path.

- [ ] **P1-204 — Implement forgot-password and reset-password**
  - Do not reveal whether an email address exists.
  - Expire and invalidate reset tokens after use.
  - Revoke existing sessions after a successful reset.

- [ ] **P1-205 — Implement organization invitations and membership**
  - Invite verified users and handle pending invitations.
  - Prevent duplicate active memberships.

- [ ] **P1-206 — Implement roles and permissions**
  - Support owner, admin, member, and viewer roles.
  - Read current roles from the server instead of trusting old token claims.
  - Define permissions in one backend policy layer.

- [ ] **P1-207 — Enforce tenant isolation everywhere**
  - Include the organization boundary in every tenant-owned query.
  - Validate tenant identifiers again in background jobs.
  - Add database constraints connecting child records to the correct parent tenant.

- [ ] **P1-208 — Add account and tenant security tests**
  - Attempt cross-organization reads and writes for every current resource.
  - Test token rotation, reuse, expiration, logout, and reset behavior.

### Milestone 2 gate

- [ ] A new user can complete signup, verification, login, and logout.
- [ ] Password reset works without exposing whether an account exists.
- [ ] Role permissions pass their API tests.
- [ ] Cross-organization access tests pass for every Phase 1 resource implemented so far.
- [ ] Authentication secrets never appear in logs or API responses.

---

## 7. Milestone 3 — Reliable and Safe Monitoring Engine

### Goal

Turn the first working check into a bounded, restart-safe monitoring engine.

### Checklist

- [ ] **P1-301 — Complete monitor management**
  - Implement get, update, soft delete, pause, resume, and test-now endpoints.
  - Support GET and HEAD only.
  - Support documented intervals, timeouts, and expected status ranges.

- [ ] **P1-302 — Encrypt monitor request headers**
  - Encrypt custom authorization headers before database storage.
  - Keep key material outside the database and support key versions.
  - Redact header values from API responses, errors, and logs.

- [ ] **P1-303 — Complete URL, DNS, connection, and redirect safety**
  - Validate every resolved IPv4 and IPv6 address immediately before connection.
  - Revalidate every redirect destination.
  - Limit redirect count and strip secrets when the host changes.
  - Protect against DNS rebinding and alternate IP representations.

- [ ] **P1-304 — Bound all HTTP resource use**
  - Limit total time, response bytes read, header size, and redirects.
  - Stream and discard response bodies after the small configured limit.
  - Categorize DNS, connection, TLS, timeout, status, and internal failures.

- [ ] **P1-305 — Complete HTTP timing measurements**
  - Record DNS, connection, TLS, first-byte, and total durations where available.
  - Keep missing timing stages nullable instead of inventing zero values.

- [ ] **P1-306 — Implement durable scheduling**
  - Complete `check_jobs` with organization, environment, monitor, monitor version, job type, state, priority, attempts, availability time, lease owner/token/expiry, safe last error, and lifecycle timestamps.
  - Support `pending`, `running`, `completed`, `cancelled`, and `dead` states.
  - Keep `next_check_at` and the durable job table in PostgreSQL as the source of truth.
  - Lock due monitors in small batches with `FOR UPDATE SKIP LOCKED`.
  - Insert the scheduled job and advance `next_check_at` in the same transaction.
  - Enforce an idempotent unique monitor-and-scheduled-time key.
  - Enforce at most one pending or running scheduled job per monitor.
  - Base the next time on the planned time rather than completion time.
  - Spread schedules with a stable small offset.
  - Prevent restart from replaying an unlimited overdue backlog.

- [ ] **P1-307 — Implement leased job claiming and recovery**
  - Claim only enough pending jobs for currently free worker slots using `FOR UPDATE SKIP LOCKED`.
  - Use a configurable 60-second lease that is safely longer than the maximum Phase 1 HTTP timeout.
  - Require the current lease token when committing completion so an expired worker cannot overwrite a newer attempt.
  - Return detected internal failures to pending with bounded retry delay, and reclaim abandoned running jobs after lease expiry.
  - Treat DNS, connection, TLS, timeout, and unexpected status as final target results rather than retryable internal failures.
  - Move jobs to `dead` after three internal attempts and raise an operational alert.
  - Cancel jobs whose monitor was paused, deleted, or changed to another version before execution.
  - Store manual-test results separately from reliability calculations; they must not change uptime, coverage, monitor state, or incidents.
  - Record dead, skipped, or unscheduled checks as unknown coverage.

- [ ] **P1-308 — Add durable queue, worker, and cleanup limits**
  - Use a configurable fixed worker count and never claim more jobs than available execution slots.
  - Enforce at most 1,000 pending/running scheduled jobs and at most 100 waiting manual-test jobs platform-wide for the initial beta.
  - Give scheduled checks priority over manual test jobs and rate-limit manual enqueue requests.
  - Expose pending/running/dead counts, oldest pending age, lease expirations, scheduler delay, queue-limit events, and missed work.
  - Delete completed queue rows after 48 hours and dead/cancelled rows after seven days, only after result and audit needs are satisfied.
  - Remain within fixed memory and disk bounds during many simultaneous timeouts.

- [ ] **P1-309 — Calculate uptime and coverage**
  - Calculate observed uptime from observed checks only.
  - Calculate coverage from observed versus expected checks.
  - Never show an incomplete period as 100% reliable without its coverage.

- [ ] **P1-310 — Add rollups and retention**
  - Keep raw results for seven days.
  - Build hourly summaries for 90 days and daily summaries for one year.
  - Make retention jobs repeatable and safe to retry.

- [ ] **P1-311 — Test scheduler and checker behavior**
  - Test atomic enqueue/schedule advancement, duplicate scheduling, concurrent claims, lease expiry, stale lease completion, dead jobs, priority, hard limits, cleanup, pause/resume, overdue monitors, restart, timeouts, redirects, and database slowdown.
  - Crash a worker after the target responds but before result commit; verify that retry may repeat the request but stores only one result for the job ID.
  - Verify that a target timeout completes as one unhealthy result without consuming another internal job attempt.
  - Load-test only controlled target APIs.

### Milestone 3 gate

- [ ] Monitoring survives an application restart without an unlimited replay storm.
- [ ] Pending jobs survive application and worker restarts.
- [ ] Expired leases are reclaimed and stale lease owners cannot commit over a newer attempt.
- [ ] Duplicate scheduling and worker recovery cannot create duplicate stored results for one job ID.
- [ ] Dead jobs and queue-limit events are visible and lower coverage appropriately.
- [ ] The durable queue stays within its row and disk limits during at least 100 concurrent timeouts.
- [ ] Direct, DNS, redirect, metadata, private-IP, and DNS-rebinding safety tests pass.
- [ ] Missing work lowers coverage and is not counted as success.
- [ ] Stored or logged data never contains plaintext monitor secrets or response bodies.
- [ ] The system approaches the planned 20 checks/second under the documented controlled test conditions, or the measured safe limit is recorded.

---

## 8. Milestone 4 — Incidents and Email Notifications

### Goal

Detect meaningful outages, avoid alerts from single failures, and deliver recoverable email notifications.

### Checklist

- [ ] **P1-401 — Implement the monitor state machine**
  - First and second consecutive failures produce degraded state.
  - Third consecutive failure produces down state.
  - First recovery success keeps an incident open.
  - Second consecutive recovery success restores healthy state.

- [ ] **P1-402 — Implement incident creation and automatic resolution**
  - Allow only one open incident per monitor and rule.
  - Create the incident and related state changes atomically.
  - Resolve the incident automatically after the configured recovery rule.

- [ ] **P1-403 — Implement acknowledge and manual resolution**
  - Record the user, time, and optional reason.
  - Keep acknowledged incidents open until resolved.
  - Ensure manual resolution does not pause future monitoring.

- [ ] **P1-404 — Add the PostgreSQL notification outbox**
  - Write the notification job in the same transaction as the incident change.
  - Claim jobs with `FOR UPDATE SKIP LOCKED`, a worker identity, and a lease so multiple workers cannot normally send the same job simultaneously.
  - Reclaim expired notification leases and move exhausted work to a visible failed state.
  - Keep notification idempotency separate from check-job idempotency because SMTP delivery has different retry behavior.

- [ ] **P1-405 — Implement the email retry worker**
  - Retry immediately, after one minute, after five minutes, and after 25 minutes.
  - Record provider result, attempt count, next attempt, and final failure.
  - Include the incident ID so a rare duplicate is recognizable.

- [ ] **P1-406 — Integrate OCI Email Delivery**
  - Keep provider credentials outside the repository and database.
  - Support a local development mail adapter.
  - Document OCI sender verification and configuration.

- [ ] **P1-407 — Add member notification preferences**
  - Email only verified organization members who enabled incident notifications.
  - Apply tenant and role checks to preference updates.

- [ ] **P1-408 — Add incident and notification tests**
  - Test open, acknowledge, manual resolve, automatic recovery, concurrency, provider failure, retry, and process restart.

### Milestone 4 gate

- [ ] Three controlled failures open exactly one incident.
- [ ] Two controlled recovery successes resolve the incident.
- [ ] Temporary email-provider failure does not lose the notification.
- [ ] Restarting during pending notification work preserves retry state.
- [ ] Incident concurrency tests preserve the one-open-incident rule.

---

## 9. Milestone 5 — Complete and Freeze the Backend API

### Goal

Finish every backend capability needed by the Phase 1 React application. After this milestone, the frontend should be able to use documented APIs without asking for missing business logic or reading the database directly.

### Checklist

- [ ] **P1-501 — Complete account and membership APIs**
  - Expose signup, verification, login, token refresh, logout, password reset, invitations, roles, and alert preferences.
  - Return only the fields needed by the current authenticated user.

- [ ] **P1-502 — Complete organization, project, and environment APIs**
  - Add authorized list, create, read, update, and supported delete operations.
  - Return the current user's effective role and allowed actions.

- [ ] **P1-503 — Complete monitor and check-result APIs**
  - Expose monitor create, edit, test, pause, resume, soft delete, current state, and recent results.
  - Add bounded pagination, filters, time ranges, and stable ordering.

- [ ] **P1-504 — Complete reporting APIs**
  - Return healthy, degraded, down, paused, and unknown counts.
  - Return observed uptime, coverage, latency, and rollups for bounded time ranges.
  - Keep manual tests out of reliability calculations.

- [ ] **P1-505 — Complete incident and notification APIs**
  - Expose incident lists, timelines, delivery state, acknowledge, and manual resolution.
  - Apply role and tenant rules to every read and action.

- [ ] **P1-506 — Implement the Server-Sent Events endpoint**
  - Send only an event ID, event type, and affected record identifiers after database commit.
  - Authorize the connection and prevent events from crossing organization boundaries.
  - Treat live events as refresh hints, not as the source of truth.

- [ ] **P1-507 — Make normal APIs suitable for polling**
  - Support efficient conditional or cursor-based refresh where useful.
  - Ensure a client can refresh relevant state every 15 seconds if live events are unavailable.
  - Keep response sizes and database queries bounded.

- [ ] **P1-508 — Publish and validate the API contract**
  - Document endpoints, request and response shapes, authentication, error codes, pagination, time formats, and SSE events in OpenAPI plus short usage notes.
  - Keep one consistent JSON error format.
  - Mark the Phase 1 API contract as frozen except for compatible fixes.

- [ ] **P1-509 — Add backend rate limits and validation consistency**
  - Apply suitable limits to authentication, manual tests, monitor changes, invitations, reports, and live connections.
  - Return predictable validation and permission errors for the React application.

- [ ] **P1-510 — Run the complete backend system test suite**
  - Cover account-to-monitor, durable scheduling, failure-to-incident, email retry, acknowledge, recovery, reporting, live events, polling, and role restrictions through public APIs.
  - Verify that no test requires a frontend or direct database edit to complete a user operation.

- [ ] **P1-511 — Add backend self-monitoring**
  - Record scheduler delay, durable queue state counts and oldest age, lease expirations, dead jobs, hard-limit events, completed and missed checks, database delay, notification queue age, disk pressure, and retention status.
  - Avoid one application log entry for every successful check.

- [ ] **P1-512 — Complete structured and redacted logging**
  - Include time, level, component, request ID, and safe tenant or record identifiers.
  - Redact authorization values, cookies, tokens, custom headers, personal data, and database secrets.

- [ ] **P1-513 — Complete readiness and graceful shutdown**
  - Stop scheduling and claiming new work before shutdown.
  - Give in-flight API requests and checks a bounded completion period.
  - Commit completed work when possible; otherwise allow its lease to expire for safe reclaim after restart.
  - Report database and essential worker readiness accurately.

- [ ] **P1-514 — Automate backend retention and maintenance**
  - Schedule rollup, deletion, durable-queue cleanup, expired-token cleanup, and notification cleanup jobs.
  - Expose their last successful completion time.

### Backend completion gate

- [ ] Every Phase 1 user action required by the React application has a documented API.
- [ ] OpenAPI validation and backend unit, database, integration, security, and system tests pass.
- [ ] Pagination, time ranges, rate limits, and response sizes are bounded.
- [ ] SSE authorization and cross-organization isolation tests pass.
- [ ] Polling can reconstruct current state after missed or disconnected live events.
- [ ] Uptime, coverage, latency, incident, and notification responses are verified against known test data.
- [ ] Backend health, queue health, retention jobs, logs, readiness, and graceful shutdown are tested.
- [ ] The frontend can be built without direct database access or new backend business rules.

Do not begin Milestone 6 until this gate passes. If frontend development later exposes a missing backend behavior, add and verify it here, update the API contract, and then return to the frontend task.

---

## 10. Milestone 6 — React Frontend

### Goal

Build the entire customer-facing Phase 1 application in React and TypeScript using only the frozen backend API.

### Checklist

- [ ] **P1-601 — Create the frontend repository and initialize the React application**
  - Create the separate frontend Git repository with its own `README.md`, `.gitignore`, contribution notes, and editor settings.
  - Create the React and TypeScript application with routing, Mantine, ECharts, linting, formatting, and tests.
  - Add a responsive application shell and a documented frontend directory structure.
  - Configure the backend API base URL and contract version without importing backend source code.

- [ ] **P1-602 — Build the typed API client**
  - Generate or validate TypeScript request and response types from the OpenAPI contract.
  - Centralize authentication refresh, JSON errors, cancellation, timeouts, and retry-safe reads.
  - Do not duplicate backend validation or business-state rules in the browser.

- [ ] **P1-603 — Build authentication screens and protected routing**
  - Add signup, verification, login, logout, forgot-password, and reset-password flows.
  - Restore sessions safely and redirect unauthorized users without exposing protected content.

- [ ] **P1-604 — Build account and member pages**
  - Add invitations, member lists, roles, and alert preferences.
  - Hide or disable unavailable actions for clarity, while relying on backend authorization for security.

- [ ] **P1-605 — Build organization, project, and environment navigation**
  - Provide clear selectors and preserve the selected environment.
  - Handle empty organizations, invitations, and removed access cleanly.

- [ ] **P1-606 — Build the dashboard overview**
  - Show healthy, degraded, down, paused, and unknown counts.
  - Show recent incidents, observed uptime, and monitoring coverage.

- [ ] **P1-607 — Build monitor list and management pages**
  - Support create, edit, test, pause, resume, and soft delete.
  - Clearly show validation, rate-limit, permission, and stale-update errors returned by the backend.

- [ ] **P1-608 — Build monitor details and charts**
  - Show recent checks, uptime, coverage, and latency using ECharts.
  - Make time range, timezone, loading state, and missing data clear.
  - Use summary endpoints for large time ranges.

- [ ] **P1-609 — Build incident pages**
  - Show incident lists, state changes, notifications, acknowledgements, and resolution.
  - Support acknowledge and manual resolution according to the current role.

- [ ] **P1-610 — Add live updates and polling fallback**
  - Use Server-Sent Events only as a signal to re-fetch authorized data through the normal API.
  - Poll at least every 15 seconds when live events are unavailable.
  - Recover after browser sleep, expired sessions, missed events, and network interruption.

- [ ] **P1-611 — Complete accessibility and user-facing failure states**
  - Add keyboard navigation, useful labels, focus handling, loading, empty, error, unknown-data, and reconnect states.
  - Test key pages at common mobile and desktop sizes.

- [ ] **P1-612 — Add frontend CI and browser end-to-end tests**
  - Build, lint, type-check, and test the React application in the frontend repository's CI.
  - Cover signup-to-monitor, failure-to-incident, acknowledge, recovery, live update, polling fallback, and role restrictions.
  - Use the real backend API with controlled test dependencies rather than mocked business behavior for critical end-to-end tests.
  - Verify compatibility with the backend's versioned OpenAPI contract and publish a versioned frontend build artifact or image.

### Frontend completion gate

- [ ] A beta user can complete the full Phase 1 flow without database edits or command-line tools.
- [ ] Every customer-facing screen is implemented in React and uses the documented API.
- [ ] The frontend repository builds and tests independently against the versioned backend API contract.
- [ ] The dashboard distinguishes unknown coverage from healthy uptime.
- [ ] Live updates work and polling restores state when the event stream is interrupted.
- [ ] Critical browser flows, accessibility checks, type checks, and production builds pass.
- [ ] Viewer and member restrictions are enforced by the backend and represented clearly in the UI.
- [ ] No business-critical state exists only inside the browser.

---

## 11. Milestone 7 — Operations, Recovery, and Oracle Deployment

### Goal

Package, secure, test, and deploy the completed backend and React frontend for a small, best-effort beta on Oracle Cloud Always Free. This milestone may adjust deployment configuration, but it must not introduce unfinished product behavior that belongs in the backend or frontend milestones.

### Checklist

- [ ] **P1-701 — Implement backups**
  - Create nightly logical backups of control, configuration, and incident data.
  - Configure block-volume backups.
  - Store an encrypted copy in OCI Object Storage within available limits.

- [ ] **P1-702 — Perform and document a restore test**
  - Restore into a clean environment.
  - Verify users, monitors, schedules, incidents, and notification state.
  - Record recovery time, data loss window, date, and result.

- [ ] **P1-703 — Create the production Docker Compose stack**
  - Keep the production Docker Compose definition in the backend monorepo and pin the frontend build artifact or image version it deploys.
  - Run Nginx, the Go application, and PostgreSQL, with Nginx serving the pinned React build.
  - Add container health checks, restart policies, resource limits, and persistent storage.
  - Keep PostgreSQL data on a separate OCI block volume.

- [ ] **P1-704 — Configure Nginx and HTTPS**
  - Terminate TLS, serve React routes correctly, proxy API and SSE requests, redirect HTTP to HTTPS, set security headers, and apply basic request-size and rate limits.
  - Document certificate renewal.

- [ ] **P1-705 — Integrate OCI secrets and free services**
  - Store encryption keys and important secrets in OCI Vault.
  - Configure Email Delivery, Object Storage, Logging, Monitoring, and Bastion only within verified current allowances.

- [ ] **P1-706 — Create Oracle Cloud infrastructure instructions**
  - Document VM, network, firewall, block volume, DNS, deployment, upgrade, rollback, and recovery procedures.
  - Verify the current Oracle Free Tier allowance before provisioning.

- [ ] **P1-707 — Run ARM64 load and endurance tests**
  - Test ordinary fast checks, simultaneous timeouts, durable scheduler backlog, lease expiry, worker crash/reclaim, database slowdown, notification backlog, queue cleanup, API traffic, and live connections.
  - Record resource use and the actual safe operating limit.

- [ ] **P1-708 — Run the security test suite**
  - Test SSRF protections, redirects, DNS rebinding, tenant isolation, session security, request limits, secret redaction, frontend security headers, and production CORS behavior.

- [ ] **P1-709 — Add beta operations documentation**
  - Add deployment, rollback, incident response, backup, restore, capacity, known limitations, and support expectations.
  - State clearly that the free-tier beta has no commercial SLA.

### Milestone 7 gate

- [ ] The complete backend and React production build runs on Linux ARM64 using the tested deployment configuration.
- [ ] HTTPS, React routing, API proxying, SSE proxying, container restart, health checks, and persistent storage work.
- [ ] A clean-environment PostgreSQL restore has succeeded and is documented.
- [ ] Load-test results state the measured safe capacity.
- [ ] Security tests pass.
- [ ] Disk, memory, queue, scheduler, notification, and backup health are visible.
- [ ] Queue depth, oldest pending age, lease expiry, dead jobs, and hard-limit events are visible.
- [ ] Deployment and rollback have each been tested at least once.

---

## 12. Final Phase 1 Release Gate

Phase 1 is complete only when all of the following are true:

- [ ] A new user can complete the full flow without manual database changes.
- [ ] Monitoring survives application and container restarts.
- [ ] Durable check and notification jobs survive application and worker restarts.
- [ ] The system demonstrates at-least-once check execution with one stored result per stable job ID.
- [ ] PostgreSQL queue hard limits, cleanup, dead-job handling, and operator alerts pass their tests.
- [ ] Email alerts survive temporary provider failure.
- [ ] Private IP, metadata, redirect, IPv6, and DNS-rebinding safety tests pass.
- [ ] Cross-organization access tests pass for every tenant-owned resource.
- [ ] The frozen backend API contract covers every React operation and passes contract tests.
- [ ] Every customer-facing screen is implemented in React without direct database access.
- [ ] Critical React browser flows and the production frontend build pass.
- [ ] Missing checks appear as unknown coverage, not healthy uptime.
- [ ] The stated safe check rate has been demonstrated on ARM64 and documented.
- [ ] A PostgreSQL backup has been restored successfully.
- [ ] Secrets and response bodies are absent from stored results and logs.
- [ ] Data retention and rollup jobs have run successfully.
- [ ] The Oracle deployment, upgrade, rollback, and recovery instructions are usable.
- [ ] Product limitations and the absence of a commercial SLA are documented for beta users.

When this gate passes, tag the release as the Phase 1 beta and begin Phase 2 planning. Do not begin Phase 2 implementation while required Phase 1 safety or recovery items remain incomplete.

---

## 13. GitHub Issue Template

Use this template for each implementation issue:

```markdown
# P1-NNN — Short task name

## Goal

Describe the user-visible or operational outcome.

## Dependencies

- P1-NNN, or `None`

## Scope

- Work included in this task

## Acceptance Criteria

- [ ] Observable requirement
- [ ] Important error case
- [ ] Authorization or safety requirement
- [ ] Documentation updated where needed

## Tests

- [ ] Unit tests
- [ ] PostgreSQL integration tests, if applicable
- [ ] API or browser test, if applicable
- [ ] Manual verification, only when automation is impractical

## Out of Scope

- Related work deliberately excluded from this issue

## Notes and Decisions

- Record important implementation decisions here
```

---

## 14. Recommended Codex Workflow

Work on one checklist task or one small related group at a time.

Example request:

```text
Implement P1-004 from PHASE_1_IMPLEMENTATION_PLAN.md.

First inspect DESIGN_SPECIFICATION.md, RISKS_AND_CAVEATS.md, the task's
dependencies, and the existing repository. Explain any assumption that would
change the design. Implement the task, add relevant tests, run verification,
and update the checklist only after its acceptance criteria pass. Do not add
Phase 2 or Phase 3 infrastructure.
```

For each task, Codex should:

1. Read the task, its dependencies, and the relevant design sections.
2. Inspect existing code and uncommitted changes.
3. State the intended implementation briefly.
4. Implement the smallest complete change.
5. Add or update tests.
6. Run focused tests, then the broader relevant test suite.
7. Review tenant safety, secrets, error handling, and migration impact.
8. Update the checkbox only when the task is actually complete.
9. Summarize changed files, verification results, and any remaining concern.

Start with **P1-001**. Complete the Milestone 0 gate before moving to Milestone 1.

Complete milestones in document order. In particular, do not start **P1-601** or another React task until the Milestone 5 backend completion gate passes.
