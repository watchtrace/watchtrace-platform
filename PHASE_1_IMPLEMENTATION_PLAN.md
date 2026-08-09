# Phase 1 Implementation Plan

## Document Purpose

This document is the working checklist for Phase 1 of the real-time API monitoring platform.

- `DESIGN_SPECIFICATION.md` defines what the product must do and how it should grow.
- `RISKS_AND_CAVEATS.md` records important risks and safety controls.
- This document defines the order in which Phase 1 will be implemented and verified.
- GitHub Issues may contain more detailed work, but their task IDs should match this document.

Phase 1 development is complete only when every active package and development-gate item passes. Production or public-beta readiness additionally requires the deferred production deployment gate and the Phase 1 release gate in `RISKS_AND_CAVEATS.md`.

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

Email delivery uses a separate durable `notification_outbox` table with its own delivery ID, lease, retry time, attempt limit, and final failed state. A check job and an email job must not share state or retry rules. Phase 1.3 defines this flow.

---

## 3. Working Method

### 3.1 Execution-package states

Use these states in GitHub Project:

```text
Backlog -> Ready -> In Progress -> Review -> Done
```

Only one execution package should normally be in progress for a single developer. A package may use several commits or agent continuations, but it has one completion checkbox. Do not start Phase 1.5 React work before the Phase 1.4 backend completion gate.

### 3.2 Definition of done for every execution package

An execution package may be marked complete only when:

- Its acceptance criteria pass.
- Relevant unit or integration tests have been added.
- Existing tests still pass.
- Errors and logs do not expose secrets.
- Tenant-owned queries enforce the organization boundary.
- Documentation or configuration examples are updated when behavior changes.
- The implementation does not add an out-of-scope Phase 2 or Phase 3 dependency.
- The change has been reviewed before merging into `main`.

### 3.3 Package ID and commit rules

Use the same package ID in the checklist, GitHub Issue, and every related commit. Work directly on `main` unless a risky change genuinely needs a temporary branch. A large package may have several focused commits; only its final verified commit changes the package checkbox to `[x]`.

Example:

```text
Issue:  P1-306 Durable scheduling, leased workers, and queue controls
Commit: feat(p1-306): add durable job claiming
Commit: test(p1-306): verify lease recovery
```

### 3.4 Implementation rule

Build one small backend path first. Do not finish every database table or future service before executing the first real monitor check through the API.

Use this order:

```text
Previously completed work through P1-204
  -> Phase 1.1 Membership, authorization, and tenant security
  -> Phase 1.2 Reliable and safe monitoring engine
  -> Phase 1.3 Incidents and durable notifications
  -> Phase 1.4 Complete and freeze the backend
  -> Phase 1.5 React frontend
  -> Deferred production deployment work when production begins
```

During backend phases, use API integration tests, command-line HTTP requests, and controlled test servers instead of temporary UI pages. Do not create throwaway server-rendered pages. All customer-facing Phase 1 screens are implemented together in Phase 1.5 after the backend gate passes.

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

- [x] A new developer can start PostgreSQL and the application from the README.
- [x] The backend builds and its tests pass locally and in CI.
- [x] A migration can be applied to a clean database.
- [x] The Linux ARM64 backend image builds successfully.
- [x] No real secret is committed to the backend repository.

---

## 5. Milestone 1 — First Backend Monitoring Slice

### Goal

Prove the product's central path as early as possible:

```text
User -> Organization -> Project -> Environment -> Monitor
  -> Durable PostgreSQL check job -> Leased worker
  -> Stored HTTP result -> Authorized API response
```

Account handling in this milestone may be minimal, but passwords must still be hashed securely. Public-beta account features are completed across Phase 1.0 and Phase 1.1. Verify this slice with API and database integration tests; do not build React pages yet.

### Checklist

- [x] **P1-101 — Add the minimum account and ownership schema**
  - Add users, organizations, memberships, projects, and environments.
  - Use UUIDs or another documented non-sequential public identifier.
  - Store timestamps in UTC.

- [x] **P1-102 — Implement minimal signup and login**
  - Hash passwords with Argon2id or the approved bcrypt fallback.
  - Return a secure authenticated session suitable for local development.
  - Do not log passwords, tokens, or cookies.

- [x] **P1-103 — Create the default ownership path**
  - Let a new user create an organization, project, and production environment.
  - Ensure every child record belongs to the correct organization.

- [x] **P1-104 — Add the initial monitor schema and API**
  - Create and list a GET monitor with URL, interval, timeout, and expected status range.
  - Default to a five-minute interval and a five-second timeout.
  - Enforce organization and environment ownership.

- [x] **P1-105 — Add destination safety before making requests**
  - Allow only HTTP and HTTPS on ports 80 and 443.
  - Reject private, loopback, link-local, multicast, and cloud metadata destinations for IPv4 and IPv6.
  - Add tests using controlled local DNS and target servers.

- [x] **P1-106 — Implement the first durable scheduler path**
  - Add the initial `check_jobs` table and indexes.
  - Load due monitors from PostgreSQL in a small batch.
  - Insert one durable job and advance the monitor schedule in the same transaction.
  - Prevent duplicate jobs with a unique monitor-and-scheduled-time key.
  - Record scheduled time separately from start and completion time.

- [x] **P1-107 — Execute and store an HTTP check**
  - Claim a pending job with a worker ID and lease using `FOR UPDATE SKIP LOCKED`.
  - Apply the monitor timeout and expected status range.
  - Idempotently store at most one final success or failure, status code, error category, and total duration per job ID.
  - Mark the job completed in the same result transaction.
  - Do not save the response body.

- [x] **P1-108 — Add the first monitoring read API**
  - Return the monitor's current state and recent check results.
  - Scope every query to the authenticated organization and environment.

- [x] **P1-109 — Add a backend vertical-slice integration test**
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

## 6. Phase 1.0 — Account Security Work Through P1-204

### Goal

Complete the account-security work that precedes the newly consolidated execution packages. The implementation has progressed through P1-204; the completed checkboxes below preserve the repository's verified implementation state.

### Checklist

- [x] **P1-201 — Implement production access and refresh sessions**
  - Use short-lived access tokens.
  - Use random, rotated refresh tokens stored only as digests.
  - Put browser refresh tokens in Secure, HttpOnly cookies in production.
  - Revoke the token family when a rotated token is reused.

- [x] **P1-202 — Implement logout and session revocation**
  - Support current-session and all-session logout.
  - Clean up expired and revoked session records.

- [x] **P1-203 — Implement email verification**
  - Store only a digest of the verification token.
  - Expire tokens and prevent reuse.
  - Provide a safe local email-development path.

- [x] **P1-204 — Implement forgot-password and reset-password**
  - Do not reveal whether an email address exists.
  - Expire and invalidate reset tokens after use.
  - Revoke existing sessions after a successful reset.

---

## 7. Phase 1.1 — Membership, Authorization, and Tenant Security

### Goal

Finish the organization and authorization foundation before adding more tenant-owned monitoring features.

### Execution package

- [x] **P1-205 — Complete membership, authorization, and tenant security**

  **Combines former tasks:** P1-205 through P1-208.

  **Depends on:** P1-204.

  **Deliverables:**
  - Invite verified users, handle pending invitations, and prevent duplicate active memberships.
  - Support owner, admin, member, and viewer roles.
  - Read current roles from the server instead of trusting old token claims.
  - Define permissions in one backend policy layer.
  - Include the organization boundary in every tenant-owned query.
  - Validate tenant identifiers again in background jobs.
  - Add database constraints connecting child records to the correct parent tenant.
  - Test cross-organization reads and writes for every current resource.
  - Test token rotation, reuse, expiration, logout, and password-reset behavior.

  **Relevant specification:** Sections 3.1–3.4, 7.1, 7.5, 12.1, and 12.5 of `DESIGN_SPECIFICATION.md`; risks R-048, R-050, R-051, and R-054.

  **Package verification:**
  - Membership and invitation integration tests pass.
  - The permission matrix passes for owner, admin, member, and viewer.
  - Cross-organization attempts fail for every implemented tenant-owned resource.
  - Authentication secrets do not appear in logs or API responses.

### Phase 1.1 gate

- [ ] A new user can complete signup, verification, login, and logout.
- [ ] Password reset works without exposing whether an account exists.
- [ ] Role permissions pass their API tests.
- [ ] Cross-organization access tests pass for every Phase 1 resource implemented so far.
- [ ] Authentication secrets never appear in logs or API responses.

---

## 8. Phase 1.2 — Reliable and Safe Monitoring Engine

### Goal

Turn the first working check into a bounded, restart-safe monitoring engine.

### Execution packages

- [ ] **P1-301 — Complete secure monitor management and HTTP execution**

  **Combines former tasks:** P1-301 through P1-305.

  **Depends on:** P1-205 and the Phase 1.1 gate.

  **Deliverables:**
  - Implement monitor get, update, soft delete, pause, resume, and test-now APIs for GET and HEAD checks.
  - Support the documented intervals, timeouts, expected status ranges, and encrypted custom headers with key versions.
  - Keep encryption keys outside PostgreSQL and redact secrets from responses, errors, and logs.
  - Validate every resolved IPv4 and IPv6 address before connection and every redirect destination.
  - Block private, loopback, link-local, multicast, metadata, alternate-address, and DNS-rebinding paths.
  - Limit total request time, bytes read, header size, and redirect count; strip secrets when the host changes.
  - Record DNS, connection, TLS, first-byte, and total timing where available, leaving unavailable stages null.
  - Categorize target and internal failures without storing response bodies.

  **Relevant specification:** Sections 8.2–8.3, 12.2–12.3, and 14.1–14.2; risks R-013–R-022.

  **Package verification:**
  - Monitor lifecycle, encryption, redaction, request limits, timing, redirect, IPv4, IPv6, metadata, and DNS-rebinding tests pass.
  - Only controlled target APIs are used by network tests.

- [ ] **P1-306 — Complete durable scheduling, leased workers, and queue controls**

  **Combines former tasks:** P1-306 through P1-308.

  **Depends on:** P1-301.

  **Deliverables:**
  - Implement the complete `check_jobs` schema, states, indexes, stable IDs, monitor version, priorities, attempts, availability time, leases, safe errors, and lifecycle timestamps.
  - Insert a job and advance `next_check_at` atomically while preventing duplicate or unlimited overdue scheduling.
  - Enforce one outstanding scheduled job per monitor and stable schedule spreading.
  - Claim only available worker capacity using `FOR UPDATE SKIP LOCKED`, worker identity, lease token, and lease expiry.
  - Retry only internal failures with bounded delay; reclaim expired work; reject stale completion; move exhausted jobs to `dead`.
  - Store target failures once, cancel stale monitor work, and keep manual tests out of uptime and incident calculations.
  - Enforce worker, scheduled-job, and manual-job limits, priority, cleanup, disk bounds, queue metrics, and operational alerts.

  **Relevant specification:** Sections 8.2, 8.4–8.6, 13.1–13.3, and 14.2; risks R-018 and R-060–R-065.

  **Package verification:**
  - Atomic enqueue, duplicate prevention, concurrent claim, lease expiry, stale completion, retry, dead-job, priority, hard-limit, cleanup, pause, overdue, restart, timeout-storm, and database-slowdown tests pass.
  - A crash after target response may repeat the request but stores at most one result per job ID.

- [ ] **P1-309 — Complete reliability reporting, retention, and engine verification**

  **Combines former tasks:** P1-309 through P1-311.

  **Depends on:** P1-306.

  **Deliverables:**
  - Calculate observed uptime from observed checks and coverage from observed versus expected checks.
  - Treat missing, skipped, cancelled, and dead scheduled work as unknown coverage rather than success.
  - Keep raw results for seven days, hourly summaries for 90 days, and daily summaries for one year.
  - Make rollup and retention work repeatable and safe to retry.
  - Add the full scheduler and checker integration, crash-recovery, safety, and controlled load-test suite.

  **Relevant specification:** Sections 8.2, 8.6, 8.12–8.13, 13.2, and 14.4–14.5; risks R-011–R-018 and R-035–R-036.

  **Package verification:**
  - Known datasets produce the expected uptime, coverage, unknown count, and rollups.
  - Retention preserves required summaries and stays within configured bounds.
  - The complete Phase 1 monitoring-engine test suite passes.

### Phase 1.2 gate

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

## 9. Phase 1.3 — Incidents and Durable Notifications

### Goal

Detect meaningful outages, avoid alerts from single failures, and deliver recoverable email notifications.

### Execution packages

- [ ] **P1-401 — Complete the incident lifecycle**

  **Combines former tasks:** P1-401 through P1-403.

  **Depends on:** P1-309 and the Phase 1.2 gate.

  **Deliverables:**
  - Implement degraded, down, recovery, and healthy transitions using the documented consecutive-result rules.
  - Create at most one open incident per monitor and rule, atomically with monitor-state changes.
  - Resolve incidents automatically after the configured recovery rule.
  - Support acknowledge and manual resolution with user, time, and optional reason.
  - Keep acknowledged incidents open until resolved and ensure manual resolution does not pause monitoring.

  **Relevant specification:** Sections 8.7, 8.10, and 13.1; risks R-016, R-034, and R-042.

  **Package verification:**
  - State-transition, concurrency, duplicate-open, acknowledge, manual-resolution, and automatic-recovery tests pass.

- [ ] **P1-404 — Complete durable notification delivery and integrated incident verification**

  **Combines former tasks:** P1-404 through P1-408.

  **Depends on:** P1-401.

  **Deliverables:**
  - Write notification work in the same transaction as incident changes.
  - Claim outbox work with worker identity and leases, reclaim expired work, and expose exhausted delivery.
  - Keep email delivery identity and retry rules separate from check-job rules.
  - Retry immediately, then after one, five, and 25 minutes while recording provider response and final failure.
  - Include the incident ID so rare duplicate delivery is recognizable.
  - Support a local mail adapter and OCI Email Delivery without storing provider credentials in code or PostgreSQL.
  - Notify only verified members who enabled notifications, with tenant and role checks on preferences.
  - Test the complete incident-to-notification flow, provider failure, retry, concurrency, and restart.

  **Relevant specification:** Sections 8.7–8.8 and 12.3; risks R-031–R-034 and R-060–R-065.

  **Package verification:**
  - A temporary provider failure and application restart do not lose pending notification work.
  - Incident creation, outbox insertion, retry state, recipient selection, and final delivery tests pass.

### Phase 1.3 gate

- [ ] Three controlled failures open exactly one incident.
- [ ] Two controlled recovery successes resolve the incident.
- [ ] Temporary email-provider failure does not lose the notification.
- [ ] Restarting during pending notification work preserves retry state.
- [ ] Incident concurrency tests preserve the one-open-incident rule.

---

## 10. Phase 1.4 — Complete and Freeze the Backend

### Goal

Finish every backend capability needed by the Phase 1 React application. After this milestone, the frontend should be able to use documented APIs without asking for missing business logic or reading the database directly.

### Execution packages

- [ ] **P1-501 — Complete account and tenant APIs**

  **Combines former tasks:** P1-501 and P1-502.

  **Depends on:** P1-404 and the Phase 1.3 gate.

  **Deliverables:**
  - Expose signup, verification, login, refresh, logout, password reset, invitations, roles, and alert preferences.
  - Complete authorized list, create, read, update, and supported delete operations for organizations, projects, and environments.
  - Return only fields permitted for the authenticated user, including effective role and allowed actions.

  **Relevant specification:** Sections 3, 7.1, 8.10, 12.1, and 12.5; risks R-048, R-050, R-051, and R-054.

  **Package verification:**
  - Account, membership, role, organization, project, and environment API contract and authorization tests pass.

- [ ] **P1-503 — Complete monitoring, reporting, incident, and notification APIs**

  **Combines former tasks:** P1-503 through P1-505.

  **Depends on:** P1-501.

  **Deliverables:**
  - Expose monitor create, edit, test, pause, resume, soft delete, current state, and recent results.
  - Add bounded pagination, filters, time ranges, and stable ordering.
  - Return state counts, observed uptime, coverage, latency, and rollups while excluding manual tests from reliability.
  - Expose incident lists, timelines, notification delivery state, acknowledge, and manual resolution.
  - Apply role and tenant rules to every read and action.

  **Relevant specification:** Sections 8.3, 8.7, 8.9–8.10, and 13.2; risks R-017, R-047, and R-049.

  **Package verification:**
  - Monitoring, reporting, incident, notification, pagination, time-range, role, and tenant tests pass against known data.

- [ ] **P1-506 — Complete realtime delivery and consistent API behavior**

  **Combines former tasks:** P1-506 through P1-509.

  **Depends on:** P1-503.

  **Deliverables:**
  - Implement an authorized Server-Sent Events endpoint that emits only event ID, type, and affected record identifiers after commit.
  - Prevent events from crossing tenant boundaries and treat events only as refresh hints.
  - Make normal APIs efficient and bounded for a 15-second polling fallback.
  - Publish and validate OpenAPI documentation for requests, responses, authentication, errors, pagination, time, and SSE events.
  - Use one JSON error shape and predictable validation and permission errors.
  - Apply rate limits to authentication, manual tests, monitor changes, invitations, reports, and live connections.

  **Relevant specification:** Sections 8.9–8.10, 13.1, and 14.3; risks R-046–R-049.

  **Package verification:**
  - OpenAPI validation, SSE authorization, cross-tenant event, reconnect, polling, rate-limit, pagination, and error-contract tests pass.

- [ ] **P1-510 — Complete backend hardening and final verification**

  **Combines former tasks:** P1-510 through P1-514.

  **Depends on:** P1-506.

  **Deliverables:**
  - Run the complete public-API system path from account creation through monitoring, incident, notification, reporting, live events, polling, and role restrictions.
  - Add self-monitoring for scheduler delay, queue state and age, leases, dead jobs, missed checks, database delay, notification age, disk pressure, and retention.
  - Complete structured, redacted logging with request and safe record identifiers.
  - Stop new scheduling and claims during graceful shutdown, bound in-flight completion, and report readiness accurately.
  - Automate rollup, deletion, durable-queue, expired-token, and notification cleanup with last-success visibility.
  - Freeze the Phase 1 backend contract after all verification passes.

  **Relevant specification:** Sections 8.12–8.13, 13.4–13.5, and 14; risks R-009, R-035–R-038, R-046, and R-061–R-064.

  **Package verification:**
  - Backend unit, database, API, system, security, shutdown, recovery, maintenance, logging-redaction, and contract tests pass.
  - No user operation requires frontend code or a direct database edit.

### Phase 1.4 backend completion gate

- [ ] Every Phase 1 user action required by the React application has a documented API.
- [ ] OpenAPI validation and backend unit, database, integration, security, and system tests pass.
- [ ] Pagination, time ranges, rate limits, and response sizes are bounded.
- [ ] SSE authorization and cross-organization isolation tests pass.
- [ ] Polling can reconstruct current state after missed or disconnected live events.
- [ ] Uptime, coverage, latency, incident, and notification responses are verified against known test data.
- [ ] Backend health, queue health, retention jobs, logs, readiness, and graceful shutdown are tested.
- [ ] The frontend can be built without direct database access or new backend business rules.

Do not begin Phase 1.5 until this gate passes. If frontend development later exposes missing backend behavior, reopen the applicable Phase 1.4 package, implement and verify the change, update the API contract, and then resume frontend work.

---

## 11. Phase 1.5 — React Frontend

### Goal

Build the entire customer-facing Phase 1 application in React and TypeScript using only the frozen backend API.

### Execution packages

- [ ] **P1-601 — Create the frontend foundation and typed API client**

  **Combines former tasks:** P1-601 and P1-602.

  **Depends on:** P1-510 and the Phase 1.4 backend completion gate.

  **Deliverables:**
  - Create the separate frontend repository with its project files, React, TypeScript, routing, Mantine, ECharts, linting, formatting, and tests.
  - Add a responsive application shell and feature-oriented directory structure.
  - Configure the backend base URL and contract version without importing backend source.
  - Generate or validate TypeScript types from OpenAPI and centralize authentication refresh, errors, cancellation, timeouts, and retry-safe reads.
  - Keep backend validation and business rules out of the browser.

  **Relevant specification:** Sections 5, 6.1, 8.9–8.10, and 15.

  **Package verification:**
  - The frontend repository independently installs, lints, type-checks, tests, and builds against the frozen API contract.

- [ ] **P1-603 — Complete authentication, account, and tenant navigation**

  **Combines former tasks:** P1-603 through P1-605.

  **Depends on:** P1-601.

  **Deliverables:**
  - Build signup, verification, login, logout, forgot-password, reset-password, protected-route, and safe session-restoration flows.
  - Build invitations, members, roles, and notification-preference pages.
  - Build organization, project, and environment navigation with empty, invited, and removed-access states.
  - Show role-appropriate actions for clarity while relying on backend authorization for security.

  **Relevant specification:** Sections 3, 8.9–8.10, 12.1, and 12.5; risks R-046, R-048, and R-050.

  **Package verification:**
  - Authentication, session-expiry, protected-route, membership, role, and tenant-navigation browser tests pass.

- [ ] **P1-606 — Complete monitoring and incident experience**

  **Combines former tasks:** P1-606 through P1-609.

  **Depends on:** P1-603.

  **Deliverables:**
  - Build the dashboard with state counts, recent incidents, observed uptime, and coverage.
  - Build monitor list, create, edit, test, pause, resume, and delete experiences with backend error handling.
  - Build monitor details with recent checks, uptime, coverage, latency charts, time range, timezone, loading, and missing-data states.
  - Use summary APIs for large time ranges.
  - Build incident lists and timelines with notification, acknowledge, manual-resolution, and recovery states.

  **Relevant specification:** Sections 8.3, 8.7, 8.9–8.10, and 13.2; risks R-017, R-047, and R-049.

  **Package verification:**
  - Dashboard, monitor lifecycle, reporting, chart, incident, role, empty-state, and error-state browser tests pass against the real backend.

- [ ] **P1-610 — Complete realtime behavior, accessibility, and frontend verification**

  **Combines former tasks:** P1-610 through P1-612.

  **Depends on:** P1-606.

  **Deliverables:**
  - Use SSE only to trigger authorized API refresh and poll at least every 15 seconds when unavailable.
  - Recover after browser sleep, session expiry, missed events, and network interruption.
  - Complete keyboard navigation, labels, focus behavior, responsive layouts, and loading, empty, error, unknown, and reconnect states.
  - Add frontend CI for build, lint, type-check, unit, contract, and browser tests.
  - Cover signup-to-monitor, failure-to-incident, acknowledge, recovery, realtime, polling, and role restrictions using the real backend with controlled dependencies.
  - Publish a versioned frontend build artifact or image compatible with the backend contract.

  **Relevant specification:** Sections 8.9, 8.13, and 14.3–14.5; risks R-046–R-049.

  **Package verification:**
  - Reconnect, polling, accessibility, responsive, contract, critical browser, and production-build checks pass.

### Phase 1.5 frontend completion gate

- [ ] A beta user can complete the full Phase 1 flow without database edits or command-line tools.
- [ ] Every customer-facing screen is implemented in React and uses the documented API.
- [ ] The frontend repository builds and tests independently against the versioned backend API contract.
- [ ] The dashboard distinguishes unknown coverage from healthy uptime.
- [ ] Live updates work and polling restores state when the event stream is interrupted.
- [ ] Critical browser flows, accessibility checks, type checks, and production builds pass.
- [ ] Viewer and member restrictions are enforced by the backend and represented clearly in the UI.
- [ ] No business-critical state exists only inside the browser.

---

## 12. Deferred Production Deployment Work

### Goal

Package, secure, test, and deploy the completed backend and React frontend to Oracle Cloud. This work is intentionally excluded from the 14 active Phase 1 execution packages and will be performed later when production deployment begins.

### Deferred package P1-701 — Backup and restore

This combines former tasks P1-701 and P1-702. Production deployment must add nightly logical backups, block-volume backups, encrypted Object Storage copies, and a documented clean-environment restore test covering users, monitors, schedules, incidents, and notification state.

### Deferred package P1-703 — Oracle production deployment

This combines former tasks P1-703 through P1-706. Production deployment must provide a version-pinned Docker Compose stack, Nginx with HTTPS and correct React/API/SSE routing, persistent PostgreSQL storage, OCI Vault and other approved OCI integrations, and documented provisioning, deployment, upgrade, rollback, and recovery procedures. Current Oracle Free Tier allowances must be verified at that time.

### Deferred package P1-707 — Release qualification and beta operations

This combines former tasks P1-707 through P1-709. Before public use, run ARM64 load and endurance tests, the production security suite, deployment and rollback exercises, and finish beta operations documentation, known limitations, capacity, incident response, and support expectations.

These deferred packages are mandatory before production or a public beta. Deferral does not mean acceptance, completion, or removal of their release requirements.

### Deferred production deployment gate

- [ ] The complete backend and React production build runs on Linux ARM64 using the tested deployment configuration.
- [ ] HTTPS, React routing, API proxying, SSE proxying, container restart, health checks, and persistent storage work.
- [ ] A clean-environment PostgreSQL restore has succeeded and is documented.
- [ ] Load-test results state the measured safe capacity.
- [ ] Security tests pass.
- [ ] Disk, memory, queue, scheduler, notification, and backup health are visible.
- [ ] Queue depth, oldest pending age, lease expiry, dead jobs, and hard-limit events are visible.
- [ ] Deployment and rollback have each been tested at least once.

---

## 13. Phase 1 Development Completion Gate

The active Phase 1 implementation is development-complete only when all of the following are true. This does not authorize production deployment; the deferred production deployment gate must pass separately.

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
- [ ] Secrets and response bodies are absent from stored results and logs.
- [ ] Data retention and rollup jobs have run successfully.

When this gate passes, the application is a Phase 1 development release candidate. Do not call it production-ready or open it as a public beta until the deferred production deployment and risk-document gates pass.

---

## 14. GitHub Issue Template

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

## 15. Recommended Codex Workflow

Work on one execution package at a time. Use its embedded relevant-section references instead of rereading unrelated parts of every document.

Example request:

```text
Implement package P1-205 from PHASE_1_IMPLEMENTATION_PLAN.md.

Read the package, its relevant specification and risk sections, dependencies,
and the existing repository. Implement every deliverable, run the listed
package verification and relevant regression tests, and mark only P1-205 [x]
after all requirements pass. Work on main, use focused commits carrying the
p1-205 ID, and do not start P1-301. Do not add Phase 2, Phase 3, or deferred
production infrastructure.
```

For each package, Codex should:

1. Read the package, its dependencies, and only the relevant routed design and risk sections.
2. Inspect existing code and uncommitted changes.
3. State the intended implementation briefly.
4. Implement every deliverable in the package using focused internal steps.
5. Add or update tests.
6. Run focused tests, then the broader relevant test suite.
7. Review tenant safety, secrets, error handling, and migration impact.
8. Use one or more focused commits with the package ID; do not mix another package.
9. Update the package checkbox only when every deliverable and verification item passes.
10. Summarize changed files, commits, verification results, and any remaining concern.

Continue with **P1-205**, because implementation has progressed through P1-204. Complete subphases in document order. Do not start **P1-601** or another React package until the Phase 1.4 backend completion gate passes. Do not execute P1-701, P1-703, or P1-707 until production deployment begins.
