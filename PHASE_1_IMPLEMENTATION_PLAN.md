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

The system will run locally with Docker Compose and deploy as a small hybrid-cloud beta: one Oracle Cloud Always Free ARM64 VM for the application and PostgreSQL, plus managed Amazon SQS queues in one AWS Region. AWS usage is budgeted and is not assumed to be permanently free.

---

## 2. Phase 1 Technical Boundaries

### We will build

- Two Git repositories: one backend repository and one frontend repository.
- One modular Go application in a backend monorepo.
- A React and TypeScript dashboard in the frontend repository.
- PostgreSQL as the source of truth for configuration, job identity and state, results, and transactional outboxes.
- Amazon SQS FIFO queues as the durable transport for encrypted check jobs and signed check results.
- A modular database-free Go check worker with direct-SQS and outbound-HTTPS transport adapters.
- A stateless HTTPS queue gateway that translates authenticated worker operations to SQS without reading PostgreSQL.
- Server-Sent Events with a polling fallback.
- Docker Compose for local and Oracle Cloud deployment.
- OCI Email Delivery, Vault, Object Storage, and other free OCI services where suitable.

The backend monorepo may contain API, scheduler, FIFO publisher, result consumer, check worker, queue gateway, incident, notification, and live-event commands and modules. These are not separate repositories or independently owned microservices in Phase 1. Control-plane modules may share one deployment initially. The check worker and queue gateway remain process and dependency boundaries: neither contains a PostgreSQL driver, SQL queries, database credentials, or product-data access. Amazon SQS owns durable transport between the control plane and workers; an in-memory channel is never the source of truth.

The React application lives in the separate frontend repository. The two repositories integrate only through the documented HTTP and Server-Sent Events contract. Each repository has independent CI. The backend monorepo owns database migrations, backend deployment definitions, and the versioned OpenAPI contract; the frontend repository consumes that contract and produces a versioned build artifact or image for deployment.

### We will not build in Phase 1

- Distributed tracing. It begins in Phase 2.
- Metrics or log ingestion products.
- Multi-region monitoring.
- Redis, Kafka, ClickHouse, Kubernetes, or a self-managed queue server. Amazon SQS is the managed Phase 1 queue transport.
- Billing, paid plans, enterprise login, or public status pages.
- POST, PUT, PATCH, or DELETE synthetic checks.
- Response-body storage.
- A commercial availability guarantee.
- Terraform or another infrastructure-as-code implementation for AWS or OCI; Phase 1 uses reviewed manual runbooks and a versioned non-secret deployment manifest.
- Numeric CloudWatch alarm thresholds, Amazon SNS operational topics, automated infrastructure email notifications, or an external dead-man notification path; these begin in Phase 4.

### Durable queue contract

Phase 1 uses two logical Amazon SQS FIFO flows: encrypted check jobs from the control plane to workers, and signed results from workers to the control plane. Each main FIFO has a FIFO DLQ. PostgreSQL `check_jobs` and `check_dispatch_outbox` rows provide stable identity, immutable publication intent, and final result idempotency. The first hosted worker pool uses one job FIFO; each approved customer-VPC worker pool uses isolated job routing and keys. This contract must be implemented before scheduler optimization:

| Part | Phase 1 rule |
|---|---|
| Job and result identity | Every job has one stable `job_id`. Job publication uses `MessageDeduplicationId = job_id` and `MessageGroupId = job_id`. Every durable execution result has a stable `result_id`; retrying that same journaled result reuses it, while a different execution attempt gets a new one. Result publication uses `MessageDeduplicationId = result_id` and `MessageGroupId = job_id`. PostgreSQL uniquely constrains both accepted `job_id` and `result_id`. |
| Job types | `scheduled` and `manual_test` share the same protocol. At most ten manual jobs may wait, at least 90% of worker capacity is reserved for scheduled work, and manual admission closes before scheduled coverage is threatened. Manual results never affect uptime or incidents. |
| Job states | `pending_publish`, `published`, `completed`, `dead`, and `expired`. Delivery attempts are observations, not database locks held by the worker. |
| Atomic intent | Job creation, the exact encrypted FIFO body, bounded safe attributes, FIFO identifiers, and `next_check_at` advancement commit in one PostgreSQL transaction. |
| Publication | The publisher retries the identical stored body and identifiers. Three fast attempts stay inside the five-minute FIFO deduplication window; uncertain rows wait for result-based repair and are never automatically republished after the two-minute job-start expiry. |
| Job envelope | A versioned, signed, worker-pool-audience-bound execution snapshot is encrypted for that pool. It contains required URL/check settings but no database, user, or AWS credentials. Its bytes and hash never change for the job ID. |
| Worker boundary | The worker verifies and decrypts the envelope, executes through a shared check engine, and signs a result. Direct SQS is limited to WatchTrace-hosted workers; customer-VPC workers use the rate-limited HTTPS/mTLS gateway. Both adapters follow the same state machine. Neither the worker nor gateway connects to PostgreSQL. |
| Local journal | SQLite records accepted job IDs, snapshot hashes, and completed signed results. Same-worker redelivery republishes the saved result instead of repeating the request. |
| Completion | Before publishing or deleting a job, the gateway verifies the mTLS identity, lease, bounded schema, job/pool/snapshot/result IDs, timestamps, and worker signature. A malformed result never deletes the job. The worker publishes a valid result to the result FIFO before deletion; a signed valid unstarted-expired acknowledgement is the only exception. The result consumer commits one valid result per job before deleting the result message. |
| Recovery | Visibility expiry redelivers internal failures. A worker stops receiving new jobs while result publication is unhealthy and republishes its journal first. The result consumer stops receiving while PostgreSQL is unhealthy. Job messages move to their DLQ after five receives; result messages move after ten. Job DLQ reconciliation marks unrecoverable scheduled work unknown; result DLQ entries remain recoverable and urgent. |
| Target failure | DNS, connection, TLS, timeout, or unexpected HTTP status is stored once as an unhealthy result; it is not retried as an internal error. |
| Already-published work | Pausing or editing stops future jobs. An already published self-contained job may run until its short expiry and is recorded against its scheduled monitor version. |
| Security | Deterministic CBOR, Ed25519 signatures, X25519 plus HKDF-SHA-256 key derivation, and AES-256-GCM encryption protect the protocol; routing fields are authenticated as associated data and every derived key uses a fresh 96-bit nonce. Customer gateways require 30-day mTLS certificates issued beneath an offline root and renewed before ten days remain. SQS encryption, key rotation/revocation, signed configuration, and redaction remain mandatory in the application. Phase 1.2 development and queue validation may use one non-production IAM identity; creation of split least-privilege workload roles, trust-policy assurance, and production security exercises are deferred to Phase 4. |
| Compatibility | Control-plane consumers support the current and previous schema. Each worker pool records its worker version, supported schema range, and capabilities; dispatch occurs only when there is a compatible intersection. Consumers are upgraded before publishers. |
| Worker-pool lifecycle | Pools move through `provisioning`, `active`, `draining`, `revoked`, `deleting`, or `failed`. A reviewed manual runbook and versioned non-secret deployment manifest record and verify queues, policies, keys, certificates, and gateway mappings before activation; reconciliation repairs or rolls back partial state. Phase 4 adds split-role creation, role/trust fingerprint verification, production security exercises, and Terraform automation. |
| Cleanup | SQS retention and DLQ retention are configured explicitly. Completed ledger rows are removed after 48 hours; dead and expired rows after seven days. Monitoring results follow separate retention rules. |

Allowed state changes:

```text
pending_publish -> published -> completed
pending_publish -> completed   (result proves an ambiguously recorded publish succeeded)
pending_publish -> expired
published -> dead              (job DLQ or unrecoverable internal failure)
published -> expired           (no accepted result by the deadline)
expired -> completed           (a valid late executed result corrects provisional expiry)
dead -> completed              (a valid late executed result corrects provisional failure)
```

This gives durable publication intent, best-effort FIFO producer deduplication, and at most one accepted result per job ID. It does not promise exactly-once HTTP requests. A crash after a target response but before durable local journaling can still cause a duplicate GET or HEAD.

Email delivery uses a separate transactional PostgreSQL `notification_outbox` with its own delivery ID, database lease, and retry rules. Email work never shares the check-job or check-result FIFOs. Phase 1.3 defines this flow.

### Phase 1 AWS SQS configuration contract

The repository stores queue settings and example names, never AWS secrets. Phase 1.2 requires:

- AWS account ID, environment name (`dev`, `stg`, or `prod`), and one explicit Region. The reference Region is `ap-south-1`; any override is recorded in the environment deployment manifest before queue creation and later Region changes require a migration;
- physical queue names following this contract:

  ```text
  watchtrace-{environment}-check-jobs-hosted.fifo
  watchtrace-{environment}-check-jobs-hosted-dlq.fifo
  watchtrace-{environment}-check-results.fifo
  watchtrace-{environment}-check-results-dlq.fifo
  watchtrace-{environment}-check-jobs-{pool_id_short}.fifo
  watchtrace-{environment}-check-jobs-{pool_id_short}-dlq.fifo
  ```

  Queue names must not contain customer names, organization names, URLs, or sensitive data;
- hosted check-job FIFO URL and ARN plus its FIFO DLQ URL and ARN;
- shared check-result FIFO URL and ARN plus its FIFO DLQ URL and ARN;
- optional dedicated job FIFO and FIFO DLQ routing for approved customer worker pools;
- queue type fixed to FIFO, content-based deduplication disabled, SSE-SQS enabled on every source queue and DLQ, retention, long polling, job visibility 90 seconds, result visibility 60 seconds, and redrive counts 5 and 10;
- a two-minute job-start expiry and seven-day terminal worker-journal retention;
- application message size capped at 64 KiB;
- a local-development SQS endpoint override and non-production queue names;
- the target Phase 4 production role names `watchtrace-{environment}-job-publisher`, `watchtrace-{environment}-hosted-worker`, `watchtrace-{environment}-queue-gateway`, `watchtrace-{environment}-result-consumer`, `watchtrace-{environment}-dlq-reconciler`, and `watchtrace-{environment}-infrastructure-operator`; Phase 1.2 validation may map these logical responsibilities to one explicitly non-production IAM identity;
- Phase 4 trust restrictions for temporary OCI workload credentials, federated/MFA human operator access, exact queue ARNs and allowed actions, and proof that customer-VPC workers receive no AWS role or credentials;
- platform signing-key IDs, per-worker-pool encryption and result-signing key IDs, rotation state, and safe local-development keys; and
- HTTPS gateway mTLS trust and signed revocation/configuration, pool-to-queue mapping, authenticated-encrypted lease-token lifetime, and per-pool request, byte, concurrent-pull, and result-publication limits. Customer-VPC workers do not receive direct SQS credentials in Phase 1.

The application loads AWS Region and credentials through the AWS SDK for Go v2 default configuration chain. Phase 1.2 local validation uses LocalStack credentials, and controlled AWS validation may use one operator-provided non-production profile. Phase 4 production uses temporary workload-role credentials. Access key IDs, secret access keys, session tokens, SSO cache files, and production `.env` files must never be committed or pasted into documentation, tests, logs, issues, or chat.

The code preserves these logical permission boundaries: the publisher sends to assigned job FIFOs; direct workers receive/change visibility/delete only from their assigned job FIFO and send to the result FIFO; the stateless gateway has the same queue-limited responsibility but no PostgreSQL access; the result consumer receives/deletes from the result FIFO; and reconcilers handle assigned DLQs. Phase 1.2 may validate these paths with one non-production AWS identity. Phase 4 creates and verifies separate least-privilege roles and trust policies. Phase 1 does not grant customer-managed KMS administration or configure CloudWatch/SNS alarm delivery.

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
Issue:  P1-306 FIFO scheduling, modular workers, and result processing
Commit: feat(p1-306): add immutable fifo job publication
Commit: feat(p1-306): add database-free worker transports
Commit: test(p1-306): verify ambiguous send and result replay
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
  -> Durable PostgreSQL job/outbox -> FIFO job message
  -> Database-free worker -> FIFO result message
  -> Idempotent PostgreSQL result -> Authorized API response
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

  This completed PostgreSQL-only path is the initial compatibility slice. P1-306 replaces its delivery and claim mechanism with the immutable FIFO job/result contract while retaining stable job IDs and database idempotency.

- [x] **P1-107 — Execute and store an HTTP check**
  - Claim a pending job with a worker ID and lease using `FOR UPDATE SKIP LOCKED`.
  - Apply the monitor timeout and expected status range.
  - Idempotently store at most one final success or failure, status code, error category, and total duration per job ID.
  - Mark the job completed in the same result transaction.
  - Do not save the response body.

  This completed PostgreSQL lease path remains historical foundation work. P1-306 moves execution into the database-free worker and moves result commits into the central result consumer. The old worker lease is removed from the final Phase 1 path.

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

- [x] A new user can complete signup, verification, login, and logout.
- [x] Password reset works without exposing whether an account exists.
- [x] Role permissions pass their API tests.
- [x] Cross-organization access tests pass for every Phase 1 resource implemented so far.
- [x] Authentication secrets never appear in logs or API responses.

---

## 8. Phase 1.2 — Reliable and Safe Monitoring Engine

### Goal

Turn the first working check into a bounded, restart-safe monitoring engine.

### Execution packages

- [x] **P1-301 — Complete secure monitor management and HTTP execution**

  **Combines former tasks:** P1-301 through P1-305.

  **Depends on:** P1-205 and the Phase 1.1 gate.

  **Deliverables:**
  - Implement monitor get, update, soft delete, pause, resume, and test-now APIs for GET and HEAD checks.
  - Support the documented intervals, timeouts, expected status ranges, and encrypted custom headers with key versions.
  - Keep encryption keys outside PostgreSQL and redact secrets from responses, errors, and logs.
  - Validate every resolved IPv4 and IPv6 address before connection and every redirect destination.
  - In hosted mode, block private, loopback, link-local, multicast, metadata, alternate-address, and DNS-rebinding paths.
  - In customer-VPC mode, allow private destinations only inside the worker's protected local CIDR allowlist; never let a job widen it; and still block metadata, loopback, link-local, multicast, unsafe redirects, and DNS-rebinding escapes.
  - Limit total request time, bytes read, header size, and redirect count; strip secrets when the host changes.
  - Record DNS, connection, TLS, first-byte, and total timing where available, leaving unavailable stages null.
  - Categorize target and internal failures without storing response bodies.
  - Keep HTTP execution behind a database-independent interface so P1-306 can reuse it inside the modular worker without importing API or persistence code.

  **Relevant specification:** Sections 8.2–8.3, 12.2–12.3, and 14.1–14.2; risks R-013–R-022.

  **Package verification:**
  - Monitor lifecycle, encryption, redaction, request limits, timing, redirect, IPv4, IPv6, metadata, and DNS-rebinding tests pass.
  - Only controlled target APIs are used by network tests.

- [x] **P1-306 — Complete FIFO scheduling, modular workers, and result processing**

  **Combines former tasks:** P1-306 through P1-308.

  **Depends on:** P1-301.

  **Deliverables:**
  - Implement `worker_pools`, `worker_pool_credentials`, the complete job ledger, `check_dispatch_outbox`, accepted-result identifiers, monitor evaluation position, and `check_result_conflicts`. Store stable job/result IDs, monitor version, worker-pool routing, immutable encrypted body, bounded safe attributes, snapshot hash, FIFO identifiers, publish state, attempts, safe errors, SQS message ID, and lifecycle timestamps. Enforce unique accepted `job_id` and `result_id` values.
  - Define versioned job and result envelopes with strict size/field limits using deterministic CBOR, Ed25519 signatures, X25519 plus HKDF-SHA-256 key derivation, and AES-256-GCM. Use a fresh 96-bit nonce per derived key, bind routing fields as associated data, bind job ID, result ID, snapshot hash, audience, expiry, network-policy version, and key IDs, and require exact matches with immutable safe SQS attributes. Use reviewed cryptographic libraries; test nonce handling, rotation, mismatch, tampering, downgrade, rollback, and key loss.
  - Provision FIFO job/result queues and their FIFO DLQs through a reviewed manual runbook using the documented names, one recorded AWS Region, explicit deduplication, SSE-SQS, long polling, retention, 90/60-second visibility, 5/10 receive limits, tags, and explicit queue policies. Record and verify the resulting non-secret queue attributes in the deployment manifest. LocalStack and controlled AWS validation may use one non-production IAM identity. Split-role creation, trust-policy verification, Terraform, numeric CloudWatch thresholds, SNS topics, infrastructure email alerts, and production security exercises are Phase 4 work.
  - Insert the job, exact encrypted outbox message, identifiers, and next schedule atomically while preventing duplicate or unlimited overdue scheduling.
  - Publish the exact stored message with `MessageDeduplicationId = job_id` and `MessageGroupId = job_id`; make three fast exact-message attempts; preserve ambiguous rows; repair missing confirmation from valid results; refuse automatic job publication after the two-minute expiry; and test forced late duplicates through downstream idempotency.
  - Enforce one outstanding scheduled job per monitor, stable schedule spreading, global admission limits, no more than ten waiting manual tests, at least 90% worker capacity reserved for scheduled work, a two-minute job-start expiry, and seven-day terminal journal retention. Admit manual work only while scheduled queue age and capacity are healthy. Editing or pausing stops future jobs; already published jobs retain their scheduled version.
  - Extract the bounded GET/HEAD logic from P1-301 into a database-independent check engine and add `X-WatchTrace-Job-ID`.
  - Build a modular worker with no PostgreSQL driver or database configuration. Add direct-SQS and HTTPS transport adapters behind one interface, free-capacity long polling, signature/decryption checks, a durable SQLite journal, safe local retention, and identical adapter contract tests.
  - Add operator-only worker-pool registration and lifecycle commands with `provisioning`, `active`, `draining`, `revoked`, `deleting`, and `failed` states. Reconcile the versioned deployment manifest against queues, policies, keys, certificates, and signed gateway mappings; verify every Phase 1.2 dependency before activation, roll back partial provisioning, audit transitions, and require a drained queue plus explicit confirmation before deletion. Role/trust fingerprint reconciliation is retained as a Phase 4 production control. Do not add a worker-facing PostgreSQL control API.
  - Build a stateless HTTPS queue gateway with no PostgreSQL access. Customer-VPC workers use mTLS through this gateway; only WatchTrace-hosted workers use direct SQS. Resolve queue access and current/previous protocol support from signed non-database configuration; wrap receipt handle, job ID, pool, and trusted expiry in a short-lived authenticated-encrypted lease token; apply per-pool request, byte, concurrent-pull, and result-publication limits; and translate pull, extend, expired acknowledgement, and result operations to SQS. Phase 4 production assigns separate temporary workload roles; Phase 1.2 controlled validation may use one non-production profile.
  - Before publishing a result or deleting a job, make the gateway verify mTLS identity, lease, bounded schema, job/pool/snapshot/result IDs, timestamps, and worker signature. A malformed result must never delete the job. Delete ordinary jobs only after valid result publication; accept an expired acknowledgement only after gateway time confirms expiry within tolerance.
  - Package the worker as Linux ARM64/AMD64 binaries and a non-root container with direct-SQS hosted and HTTPS customer examples, outbound-only networking, health/readiness output, graceful shutdown, persistent-journal instructions, key protection, revocation, and upgrade documentation. Produce checksums, an SBOM, and signed binaries/images with documented verification steps.
  - Require UTC clock synchronization, expose clock-offset health, apply a small configured expiry tolerance, and validate remote timestamps without allowing them to reorder newer monitor state.
  - Give each durable result a stable `result_id`. Retry the same journaled result with `MessageDeduplicationId = result_id` and `MessageGroupId = job_id`; give a different execution attempt a new result ID. Publish before deleting executed job messages. Limit no-result deletion to signed, valid, unstarted expired jobs. On same-worker redelivery, republish the journaled result instead of repeating the target request.
  - Build the central result consumer to validate signature, pool, job, result ID, snapshot, expiry/time bounds, and sizes; store at most one valid result per job and result ID; update durable monitor state and correction audit input transactionally; repair dispatch state; quarantine conflicting valid results; and delete the result message only after commit. P1-401 and P1-404 extend that transaction with incidents and notification work. Stop result polling while PostgreSQL is unhealthy so receive attempts are not exhausted.
  - Add dependency circuit breakers: while result publication is unhealthy, workers publish their journals first and stop receiving new jobs; recover with bounded backoff and readiness signals. Verify extended SQS/result-path and PostgreSQL outages do not burn messages into a DLQ merely because a dependency is unavailable.
  - Publish an independent versioned worker OpenAPI contract covering long polling up to 20 seconds, bounded leases and extensions, retryable versus terminal errors, `Retry-After`, schema/capability ranges, and idempotent result submission. Support current and previous protocol schemas, record each pool's supported range, dispatch only a compatible schema, and test consumer-first rolling upgrades, downgrade rejection, delayed worker return, and journal replay.
  - Treat target failures as executed results. If a valid job has not started before expiry, acknowledge it without calling the target or publishing a check result; a central deadline sweep marks result-less jobs expired and unknown. Accept a later valid executed result as correction while protecting newer monitor state. Let only internal failures expire for retry; reconcile job DLQ entries to `dead`/unknown and result DLQ entries to recoverable urgent operations.
  - Add bounded encrypted quarantine records retained for 14 days and an audited operator CLI for dry-run inspection and controlled redrive. Revalidate identity, keys, expiry, and state; preserve IDs; prevent loops; never execute expired jobs; and replay recoverable results idempotently.
  - Add cleanup, journal/outbox/queue/DLQ/gateway/result-consumer metrics, health views, safe logs, and a local SQS-compatible service through Docker Compose. Production uses Amazon SQS through the AWS SDK default credential chain. Phase 4 adds automated threshold alarms and SNS/email infrastructure notifications.

  **Relevant specification:** Sections 8.2, 8.4–8.6, 12.2–12.3, 13.1–13.3, and 14.1–14.5; risks R-018 and R-060–R-089.

  **Package verification:**
  - Atomic immutable-outbox creation, exact publisher retry, accepted-send crash, five-minute job-deduplication boundary, result-ID retry, invalid-then-valid result attempts, post-expiry publish refusal, forced late duplicates, envelope cryptography, key/certificate rotation and emergency revocation, clock skew, queue attributes, protocol compatibility, transport parity, journal replay/loss, visibility expiry, conflicting results, dependency circuit breakers, partial provisioning recovery, quarantine/redrive, both DLQs, per-pool limits, cleanup, pause/edit, overdue work, restart, manual bursts, timeout storms, SQS outage, and PostgreSQL outage tests pass.
  - Static and runtime checks prove that the worker and queue gateway operate without PostgreSQL dependencies or network access.
  - A crash after target response but before journal commit may repeat the GET or HEAD, but one valid result and one set of currently implemented durable side effects are stored per job ID. Incident and notification idempotency are extended and verified by P1-401 and P1-404.

  **Completion record:** The PostgreSQL and LocalStack suite and a controlled Amazon SQS FIFO/DLQ run using one non-production IAM identity verified the Phase 1.2 build and recovery path. Creating separate AWS workload roles, verifying their trust policies, and running production security exercises are explicitly deferred to Phase 4 and are not P1-306 completion conditions.

- [x] **P1-309 — Complete reliability reporting, retention, and engine verification**

  **Combines former tasks:** P1-309 through P1-311.

  **Depends on:** P1-306.

  **Deliverables:**
  - Generate expected UTC slots from `monitor_schedule_periods`, excluding paused time, time before creation, and time after effective deletion. Preserve history across interval and worker-pool changes; manual tests never create expected or observed slots.
  - Calculate observed uptime from unique accepted executed scheduled checks and coverage from observed versus expected checks. Define zero-denominator output: no expected checks means `no data`; expected but no observed checks means `0%` coverage and `no data` uptime.
  - Treat missing, skipped, expired, and dead scheduled work as unknown coverage rather than success, including partial report buckets.
  - Serialize state evaluation per monitor and order slots by scheduled time then job ID. Persist the last evaluated scheduled slot, recompute the bounded failure/recovery window after an out-of-order result, and ensure unknown slots pause rather than silently reset consecutive counters.
  - Accept a valid late result as a raw-data correction, invalidate affected rollups, and repair them repeatably. Correct durable monitor evaluation state only inside the documented ten-minute correction window and preserve an idempotent audit event. P1-401 extends that ordered correction input to incident state and notification side effects once those tables exist; it must never erase or silently resend a notification.
  - Keep raw results for seven days, hourly summaries for 90 days, and daily summaries for one year.
  - Make rollup and retention work repeatable and safe to retry.
  - Add the full scheduler, FIFO publisher, modular-worker, result-consumer, crash-recovery, safety, and controlled load-test suite.

  **Relevant specification:** Sections 8.2, 8.6, 8.12–8.13, 13.2, and 14.4–14.5; risks R-011–R-018, R-035–R-036, R-076, and R-086.

  **Package verification:**
  - Known datasets produce the expected uptime, coverage, unknown count, durable monitor state, and rollups across pause/delete/interval/worker-pool boundaries, zero denominators, out-of-order delivery, and late correction.
  - Retention preserves required summaries and stays within configured bounds.
  - The complete Phase 1 monitoring-engine test suite passes.

### Phase 1.2 gate

- [x] Monitoring survives an application restart without an unlimited replay storm.
- [x] The deployment manifest records approved queue names, the environment's explicit AWS Region, SSE-SQS on every source queue and DLQ, and queue-policy fingerprints; queue verification against AWS passes. Separate IAM role/trust verification is a Phase 4 production gate.
- [x] Pending jobs and accepted results survive application and worker restarts in their respective FIFO queues.
- [x] A publisher crash after SQS acceptance is recovered with the exact same body and job ID; tests prove in-window deduplication, post-expiry publish refusal, and safe forced-late-duplicate handling.
- [x] Result retries reuse a stable result ID, distinct execution attempts get distinct result IDs, and an invalid first attempt cannot cause FIFO to suppress a later valid result.
- [x] Hosted direct-SQS and customer HTTPS/mTLS workers pass the same behavior suite and run without PostgreSQL dependencies; customer workers have no direct SQS credentials.
- [x] Expired visibility timeouts are redelivered; the local journal prevents normal same-worker re-execution and one valid result is stored per job ID.
- [x] Duplicate scheduling, publication, worker recovery, and result delivery cannot create duplicate stored results or currently implemented durable side effects. P1-401 and P1-404 extend this gate to incidents and notifications.
- [x] Forged, expired, wrong-pool, tampered, oversized, and conflicting envelopes are rejected or quarantined.
- [x] The gateway never deletes a job for a malformed result and enforces per-pool pull, byte, concurrency, and publication limits.
- [x] During PostgreSQL or result-publication outages, readiness gates stop new receives before SQS receive counts are exhausted; recovery drains durable journals and queues safely.
- [x] Manual bursts cannot use more than 10% of worker slots or leave more than ten manual jobs waiting while scheduled work is due.
- [x] Current and previous worker schemas interoperate; incompatible dispatch is rejected before SQS, and signed worker artifacts can be verified.
- [x] Partial worker-pool provisioning, reconciliation, draining, revocation, and deletion tests preserve isolation and do not leak active resources.
- [x] Controlled quarantine and redrive preserve identity, block expired job execution, prevent loops, and do not expose plaintext secrets.
- [x] Job-DLQ work lowers coverage appropriately; result-DLQ work is visible and recoverable without being mislabeled as target failure.
- [x] Job/result backlogs, in-flight work, both DLQs, the worker journal, and PostgreSQL ledger stay within configured limits during at least 100 concurrent timeouts.
- [x] Hosted public-address blocking and customer-VPC private-CIDR allowlisting pass direct, DNS, redirect, metadata, alternate-address, and DNS-rebinding tests.
- [x] Missing work lowers coverage and is not counted as success.
- [x] Out-of-order and late results update raw data and rollups deterministically without corrupting newer monitor state; P1-401 consumes the idempotent correction audit input without silently duplicating notifications.
- [x] Stored or logged data never contains plaintext monitor secrets or response bodies.
- [x] The system approaches the planned 20 checks/second under the documented controlled test conditions, or the measured safe limit is recorded.

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

  **Relevant specification:** Sections 8.7–8.8 and 12.3; risks R-031–R-034.

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
  - Return state counts, observed uptime, coverage, latency, rollup freshness/correction state, and explicit `no data` values for zero denominators while excluding manual tests from reliability.
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
  - Add self-monitoring for scheduler delay, immutable-outbox age, FIFO job/result state and age, both DLQs, gateway and worker-journal health, dead jobs, missed checks, result-consumer delay, database delay, notification-outbox age, disk pressure, and retention.
  - Complete structured, redacted logging with request and safe record identifiers.
  - Stop new scheduling and claims during graceful shutdown, bound in-flight completion, and report readiness accurately.
  - Automate rollup, deletion, durable-queue, expired-token, and notification cleanup with last-success visibility.
  - Freeze the Phase 1 backend contract after all verification passes.

  **Relevant specification:** Sections 8.12–8.13, 13.4–13.6, and 14; risks R-009, R-035–R-038, R-046, R-061–R-064, R-087, and R-089. Risk R-088 is explicitly accepted until Phase 4.

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

This combines former tasks P1-701 and P1-702. Production deployment must add nightly logical backups, block-volume backups, encrypted Object Storage copies, and a documented isolated restore test covering users, monitors, schedules, incidents, notification state, the versioned cloud-resource deployment manifest, exported queue/policy/role attributes, signed gateway configuration, CA records, and active/retired key history. The exercise must restore an older database beside newer queue data, keep schedulers/publishers/consumers stopped during inventory and reconciliation, quarantine unknown identities, refuse expired job republication, replay valid results idempotently, and start components in the documented order.

### Deferred package P1-703 — Oracle and AWS production deployment

This combines former tasks P1-703 through P1-706. Production deployment must provide a version-pinned Docker Compose stack, Nginx with HTTPS and correct React/API/SSE/worker-gateway routing, persistent PostgreSQL and worker-journal storage, OCI Vault and other approved OCI integrations, and reviewed manual AWS/OCI runbooks for FIFO job/result queues, per-pool job routing, FIFO DLQs, SSE-SQS, resource inventory, budgets, worker-pool lifecycle, and cross-cloud failure handling. Every created resource and non-IAM security-relevant attribute is recorded in the versioned non-secret deployment manifest and verified after setup. Production network policy must prove the worker and queue gateway cannot reach PostgreSQL. Separate least-privilege AWS workload roles, trust-policy verification, the production security exercise suite, Terraform, numeric CloudWatch alarm thresholds, SNS topics, and infrastructure email alerting are Phase 4 work. Current Oracle and AWS allowances and pricing must be verified at deployment time.

### Deferred package P1-707 — Release qualification and beta operations

This combines former tasks P1-707 through P1-709. Before public use, run ARM64 load and endurance tests, the production security suite, deployment and rollback exercises, and finish beta operations documentation, known limitations, capacity, incident response, and support expectations.

These deferred packages are mandatory before production or a public beta. Deferral does not mean acceptance, completion, or removal of their release requirements.

### Deferred production deployment gate

- [ ] The complete backend and React production build runs on Linux ARM64 using the tested deployment configuration.
- [ ] HTTPS, React routing, API proxying, SSE proxying, container restart, health checks, and persistent storage work.
- [ ] A clean-environment PostgreSQL restore has succeeded and is documented.
- [ ] An isolated cross-system restore with an older database and newer queue state has reconciled queues, infrastructure configuration, certificates, and retained keys without republishing expired work.
- [ ] Load-test results state the measured safe capacity.
- [ ] Security tests pass.
- [ ] Production uses temporary AWS workload credentials; no static AWS secret is stored in source, images, PostgreSQL, SQS messages, or production Compose files.
- [ ] Queue names, AWS Region, SSE-SQS attributes, role names, trust restrictions, and exact queue/action permissions match the reviewed deployment manifest.
- [ ] Disk, memory, queue, scheduler, notification, and backup health are visible.
- [ ] FIFO job/result visible and in-flight depth, oldest-message age, visibility expiry, both DLQs, ambiguous dispatch outbox, result-consumer delay, gateway health, worker-journal health, and hard-limit events are visible.
- [ ] Production network and dependency checks prove that check workers and the queue gateway cannot access PostgreSQL.
- [ ] Deployment and rollback have each been tested at least once.
- [ ] Infrastructure drift and a partially provisioned worker pool are detected and reconciled safely.

### Phase 4 infrastructure and alerting handoff

Phase 4 must create the separate environment-scoped job-publisher, hosted-worker, queue-gateway, result-consumer, DLQ-reconciler, and infrastructure-operator roles; restrict every trust policy and queue/action permission; verify role, trust, and policy fingerprints; and run the production security exercises. It must also import or safely replace the manually created AWS and OCI resources and then manage them with Terraform, protect and lock remote state, pin providers, require reviewed plans, separate environments, detect drift, and test rollback. Phase 4 defines numeric CloudWatch thresholds from measured load and recovery data, creates environment-specific SNS topics with confirmed email subscriptions and an independent operator destination, and adds OCI alarms plus an external readiness/scheduler-heartbeat dead-man path. These items are not Phase 1.2 or P1-306 completion requirements.

---

## 13. Phase 1 Development Completion Gate

The active Phase 1 implementation is development-complete only when all of the following are true. This does not authorize production deployment; the deferred production deployment gate must pass separately.

- [ ] A new user can complete the full flow without manual database changes.
- [ ] Monitoring survives application and container restarts.
- [ ] Durable check and notification jobs survive application and worker restarts.
- [ ] The system demonstrates bounded retry behavior with one accepted result and one set of incident/notification side effects per stable job ID; the rare duplicate GET/HEAD window is documented.
- [ ] Job-ID and result-ID FIFO identifiers, five-minute job-deduplication boundary, immutable outbox, worker journal, result idempotency, invalid-then-valid result attempts, admission limits, dependency circuits, cleanup, both DLQs, quarantine/redrive, and operator-visibility tests pass.
- [ ] Hosted direct-SQS and customer HTTPS/mTLS modes complete the same checks without worker or gateway PostgreSQL access or customer direct-SQS credentials.
- [ ] Current and previous worker protocol schemas interoperate, incompatible dispatch is blocked, certificate/key lifecycle tests pass, and worker artifact signatures are verified.
- [ ] Email alerts survive temporary provider failure.
- [ ] Hosted workers block private/special targets, customer workers cannot escape their local CIDR allowlists, and metadata, redirect, IPv6, alternate-address, and DNS-rebinding tests pass.
- [ ] Cross-organization access tests pass for every tenant-owned resource.
- [ ] The frozen backend API contract covers every React operation and passes contract tests.
- [ ] Every customer-facing screen is implemented in React without direct database access.
- [ ] Critical React browser flows and the production frontend build pass.
- [ ] Missing checks appear as unknown coverage, not healthy uptime.
- [ ] Pause/delete/interval boundaries, zero denominators, out-of-order results, late corrections, rollup repair, and incident correction produce deterministic reporting.
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
