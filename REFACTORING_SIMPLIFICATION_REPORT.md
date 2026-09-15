# WatchTrace Platform Refactoring and Simplification Report

## Executive verdict

WatchTrace is **partly over-engineered, but not broadly over-architected**.

The main architecture matches `DESIGN_SPECIFICATION.md`:

- PostgreSQL is the source of truth.
- SQS FIFO provides durable delivery.
- Workers have no PostgreSQL access.
- Direct-SQS and HTTPS/mTLS workers share the same execution engine.
- The transactional outbox, signed/encrypted envelopes, SQLite worker journal, destination safety, idempotency, tenant isolation, ordered evaluation, incidents, and notification outbox all implement explicit requirements.

Those areas are necessarily more complex than an ordinary CRUD Go service.

The genuine over-engineering is mostly evolutionary residue:

1. Two monitoring implementations exist at the same time.
2. Services can be partially configured, causing routes to appear or disappear at runtime.
3. Database logic is divided between SQLC files and roughly 176 inline query calls.
4. Several packages have very large, multi-purpose files.
5. SMTP and process-configuration code is duplicated.
6. A few compatibility paths no longer have clear removal dates.

The codebase is therefore harder to follow than necessary, but the main problem is **duplicate paths and inconsistent organization**, not widespread misuse of interfaces or dependency injection.

## Scope and measurements

The review covered:

- All 14 `cmd` entry points.
- All 30 original top-level `internal` packages. Task 1 reduced the active set to 28.
- All SQLC query files and generated code.
- All 14 migrations.
- Deployment definitions and Phase 1 documentation.
- Worker, SQS, reliability, incident, notification, security, ownership, API, and operations tests.

Approximate size:

| Area | Lines/count |
|---|---:|
| Hand-written non-test Go | 12,993 lines |
| Generated SQLC Go | 1,414 lines |
| Go tests | 10,106 lines |
| Hand-written interfaces | 35 |
| Top-level internal packages | 28 |
| Commands | 14 |

Thirty-five interfaces in a backend of this size is not inherently excessive. Most represent PostgreSQL, SQS, SMTP, HTTP, DNS, or worker transport boundaries.

After Task 1, the full race-enabled Go test suite, `go vet`, `go build`, SQLC generation, and the Docker/PostgreSQL migration and integration workflow all pass.

## Task 1 implementation report — completed 2026-09-14

Task 1 removed the obsolete PostgreSQL-only scheduler and checker while preserving the current FIFO/SQS implementation and historical migration coverage.

### Removed

- `internal/scheduler`, including its unit tests.
- `internal/checker`, including its unit tests.
- `db/queries/scheduler.sql` and `db/queries/checker.sql`.
- The generated SQLC scheduler/checker files and their methods in `Querier`.
- Integration tests that executed only the retired PostgreSQL scheduling/leasing path.
- The duplicate `docs/CHECKER.md` document.
- README descriptions and links that presented the retired implementation as active.

### Preserved or replaced

- The production `internal/fifo` scheduler, publisher, consumer, DLQ, and recovery paths remain unchanged.
- The database-free `cmd/worker`, `internal/modworker`, `internal/checkengine`, `internal/workqueue`, and SQLite journal remain unchanged.
- Shared PostgreSQL integration fixtures were moved into `tests/integration/monitoring_fixtures_test.go` and given neutral monitoring names.
- The rollback check for historical migration `000006_http_check_worker` was retained in `tests/integration/legacy_migration_test.go`.
- Both shell and PowerShell database workflows now run the renamed historical migration test.
- The existing production FIFO tests continue to cover encrypted immutable dispatch, publisher recovery, worker execution, journal replay, result idempotency, reliability, DLQ handling, and load behavior.

### Size impact

The tracked diff removes 2,801 lines and adds 75 changed/replacement lines. Two new focused integration support files add 164 lines, for a net reduction of approximately 2,560 lines. No database migration or deployed schema was changed.

### Verification completed

- Pinned SQLC generation: passed.
- `go test -race ./... -count=1`: passed.
- `go vet ./...`: passed.
- `go build ./...`: passed.
- `git diff --check`: passed.
- `tests/integration/postgres-database.sh`: passed, including migration rollback checks and the final full PostgreSQL integration suite.

## Important execution flow

The production flow is reasonably coherent once the obsolete implementation is ignored:

```text
Create/configure a monitor
cmd/api
  -> internal/httpapi
  -> internal/monitor
  -> SQLC/PostgreSQL

Run scheduled checks
cmd/monitor-engine
  -> fifo.Scheduler
  -> PostgreSQL job + immutable dispatch outbox
  -> fifo.Publisher
  -> SQS FIFO
  -> cmd/worker
  -> workqueue transport
  -> envelope verification/decryption
  -> workerjournal
  -> checkengine
  -> signed result
  -> result SQS FIFO
  -> fifo.ResultConsumer
  -> reliability
  -> incident
  -> notification outbox
  -> cmd/notification-worker
  -> SMTP
```

This corresponds closely to the architecture required in `DESIGN_SPECIFICATION.md`.

## Major findings

### 1. The original PostgreSQL scheduler and checker should be removed

**Classification: Can remove**

**Implementation status: Completed by Task 1 on 2026-09-14.**

Removed files:

- `internal/scheduler/service.go`
- `internal/checker/service.go`
- `db/queries/scheduler.sql`
- `db/queries/checker.sql`
- `internal/platform/database/sqlc/scheduler.sql.go`
- `internal/platform/database/sqlc/checker.sql.go`
- `docs/CHECKER.md`

`docs/SCHEDULER.md` was retained and updated to describe only the active FIFO scheduler.

The documentation itself identifies these as compatibility paths. The old scheduler writes directly to PostgreSQL:

```go
due, err := queries.LockDueMonitors(...)
created, err := queries.CreateScheduledCheckJob(...)
```

The old checker then leases that PostgreSQL row and performs the HTTP request:

```go
job, err := service.claimNext(ctx, workerID)
result, err := service.execute(ctx, job)
err = service.complete(ctx, job, result)
```

Production does not use either package. It uses `internal/fifo`, SQS, and the database-free worker through `cmd/monitor-engine`.

#### What is complex

A beginner sees two valid-looking answers to each fundamental question:

- How are checks scheduled?
- Does the worker read PostgreSQL?
- Are jobs leased in PostgreSQL or SQS?
- Where is the HTTP request executed?
- Which integration tests describe production?

Only one answer is correct for the current design.

#### Simpler design

Keep only:

```text
fifo.Scheduler -> transactional outbox -> SQS -> modular worker
```

Remove:

- `internal/scheduler`
- `internal/checker`
- Their SQLC query files and generated output
- Tests that exclusively validate the obsolete path
- Compatibility wording in the scheduler/checker documentation

This would eliminate roughly 1,100 implementation/query/generated lines, plus a substantial legacy test suite.

#### Gain

- One canonical monitoring path.
- Much easier onboarding.
- Fewer packages, SQL queries, database states, and test fixtures.
- Stronger enforcement of the database-free worker rule.

#### Loss and risk

- The old non-SQS fallback disappears.
- Some useful destination and concurrency coverage might exist only in the old tests and should first be migrated to `checkengine`, `fifo`, or modular-worker tests.

**Risk: Medium**, mostly test migration rather than production behavior.

---

### 2. Runtime feature detection makes API composition unclear

**Classification: Should simplify**

Relevant files:

- `internal/httpapi/router.go`
- `internal/httpapi/monitors.go`
- `internal/httpapi/ownership.go`
- `internal/httpapi/tenant_management.go`
- `internal/monitor/service.go`
- `internal/ownership/service.go`
- `internal/fifo/consumer.go`
- `cmd/api/main.go`

The router registers entire API sections only when particular dependencies happen to be non-nil:

```go
if options.AuthService != nil { ... }
if options.Authenticator != nil && options.MonitorService != nil { ... }
```

It also discovers functionality using runtime type assertions:

```go
if management, ok := options.OwnershipService.(OwnershipManagementService); ok {
    registerTenantManagementRoutes(...)
}
```

Monitor lifecycle routes work the same way:

```go
if lifecycle, ok := service.(monitorLifecycleService); ok {
    // update, delete, pause, resume, test
}
```

Services can also be incompletely constructed:

- Three monitor constructors.
- A variadic optional ownership email sender.
- Optional quarantine configuration in the result consumer.

Production supplies the complete implementations, so these optional modes mainly support older tests.

#### What is complex

Reading the router does not tell the reader which endpoints exist. That depends on the concrete runtime type passed to it.

Missing functionality does not necessarily fail compilation or startup. It may silently result in missing routes.

#### Simpler design

- Define the complete interfaces required by the production API.
- Validate all required dependencies once when creating the router.
- Register production routes unconditionally.
- Use one monitor constructor with an explicit configuration struct.
- Require the ownership sender explicitly; tests can pass a deliberate fake or `DisabledSender`.
- Keep route-specific test builders if small tests need reduced dependency sets.

#### Gain

- The compiler detects missing functionality.
- One production-like construction path.
- No silent route disappearance.
- Easier understanding of what the API provides.

#### Loss and risk

- Tests need fuller fakes or route-specific setup helpers.
- Care is needed not to turn one large interface into an unnecessarily broad mock.

**Risk: Medium**, because route availability is public API behavior.

---

### 3. Database access has two competing organizational styles

**Classification: Should simplify**

The specification deliberately chose SQLC. The current implementation sometimes uses SQLC and raw SQL in the same operation. For example, monitor creation first calls a generated query, then executes more SQL inline:

```go
created, err := queries.CreateMonitor(...)

_, err = tx.Exec(ctx, `UPDATE monitors SET ...`)
_, err = tx.Exec(ctx, `INSERT INTO monitor_schedule_periods ...`)
```

The notification worker similarly contains its complete leasing state machine as embedded strings in Go:

```go
WITH candidate AS (...)
UPDATE notification_outbox
SET state='leased', ...
FOR UPDATE SKIP LOCKED
```

Large amounts of inline SQL also exist in:

- `internal/fifo`
- `internal/reliability`
- `internal/ownership`
- `internal/backendapi`
- `internal/incident`
- `internal/operations`
- `internal/workerpool`

#### What is complex

A reader must search both `db/queries/*.sql` and arbitrary Go service files to understand the database behavior of one feature.

It also produces long `Scan(...)` argument lists mixed with transaction and business logic.

#### Simpler design

Move stable, named queries into SQLC files grouped by use case:

```text
db/queries/
  auth.sql
  ownership.sql
  monitors.sql
  monitor_lifecycle.sql
  dashboard.sql
  fifo.sql
  reliability.sql
  incidents.sql
  notifications.sql
  operations.sql
```

Keep transaction boundaries and state-transition order in Go.

Do **not** add generic repository interfaces or an ORM. That would replace visible SQL with more indirection and make the code harder for a beginner.

#### Gain

- One predictable place to find SQL.
- Named and typed queries.
- Shorter services.
- Fewer manual scan lists.
- Better alignment with `DESIGN_SPECIFICATION.md`.

#### Loss and risk

- Generated code becomes larger.
- Some dynamic maintenance queries may be clearer as raw SQL.
- Locking, tenant predicates, and `RowsAffected` semantics can be accidentally changed during conversion.

**Risk: Medium-high**. Migrate one subsystem at a time.

---

### 4. Several files carry too many responsibilities

**Classification: Should simplify**

The largest examples are:

- `internal/monitor/service.go` — 796 lines
- `internal/auth/service.go` — 680 lines
- `internal/reliability/service.go` — 649 lines
- `internal/ownership/management.go` — 568 lines
- `internal/backendapi/service.go` — 432 lines
- `internal/fifo/consumer.go` — result validation, persistence, reliability, incidents, rollup invalidation, and DLQ work

The monitor service also repeats almost identical conversions for create, list, and get rows.

#### What is complex

A package is not necessarily too large, but these particular files require a reader to keep many unrelated use cases in mind simultaneously.

#### Simpler design

Split files while keeping the same packages and public behavior:

```text
internal/monitor/
  types.go
  create_update.go
  read.go
  lifecycle.go
  headers.go

internal/auth/
  accounts.go
  verification.go
  password_reset.go
  sessions.go

internal/reliability/
  report.go
  evaluation.go
  rollups.go
  retention.go
```

This does not add architectural layers. In Go, all files in the same directory still form one package.

Where possible, have SQLC return a consistent monitor row shape instead of maintaining three nearly identical mapper functions.

#### Gain

- Smaller reading units.
- Easier review and testing.
- Clearer locations for particular behavior.

#### Loss and risk

- The raw number of files may increase slightly.
- Careless mapper consolidation could change null/time handling.

**Risk: Low** for file-only moves; **Medium** for mapper/query consolidation.

---

### 5. SMTP transport is implemented twice

**Classification: Should simplify**

Relevant files:

- `internal/auth/verification.go`
- `internal/notification/smtp.go`

Authentication implements its own connection, deadline, STARTTLS, authentication, sender, recipient, data, and quit sequence.

Notifications implement essentially the same SMTP sequence. Mailbox and loopback-host validation are also duplicated.

#### Simpler design

Create one small technical package:

```text
internal/platform/mail/
  smtp.go
```

It should own:

- SMTP connection and deadlines.
- STARTTLS.
- OCI SMTP authentication.
- Envelope sender/recipient handling.
- Basic address and header safety.

Keep domain-specific behavior separate:

- Account-action URL and message creation stay in `auth`.
- Incident notification formatting and provider error classification stay in `notification`.

#### Gain

- One SMTP security implementation to audit.
- Fixes apply to both email paths.
- Less duplicate networking code.

#### Loss and risk

- Authentication and notification require different error semantics.
- The shared transport must not expose credentials in errors.

**Risk: Medium**, because both account access and alert delivery depend on it.

---

### 6. Process configuration and startup wiring are inconsistent

**Classification: Should simplify**

Relevant files:

- `internal/platform/config/config.go`
- `cmd/worker/main.go`
- `cmd/monitor-engine/main.go`
- `cmd/notification-worker/main.go`
- `cmd/queue-gateway/main.go`

The API has a validated configuration type. Other commands manually read environment variables, decode keys, build TLS, configure AWS, and panic for missing values:

```go
func required(name string) string {
    value := strings.TrimSpace(os.Getenv(name))
    if value == "" {
        panic(name + " is required")
    }
    return value
}
```

#### Simpler design

Keep command-specific configuration, but standardize its shape:

```go
cfg, err := config.LoadWorker()
deps, err := buildWorker(ctx, cfg)
err = runWorker(ctx, deps)
```

Use separate files in `internal/platform/config` rather than one huge universal configuration type:

```text
config/
  api.go
  monitor_engine.go
  worker.go
  gateway.go
  notification.go
  files.go
```

The worker and gateway configuration must remain free from PostgreSQL-related dependencies.

#### Gain

- Predictable startup flow.
- Errors instead of panics.
- Easier environment-variable testing.
- Less repeated secret-file and TLS parsing.

#### Loss and risk

- More configuration structs and tests.
- Deployment variable names must remain exactly compatible.

**Risk: Medium**, especially for Coolify configuration.

---

### 7. Some package boundaries and operator tools need curation, not immediate deletion

**Classification: Revisit later**

Two package names/boundaries provide relatively little value:

- `internal/backendapi` is only the API's dashboard/read service; the name is vague.
- `internal/gatewayconfig` is used only by the queue gateway and could live in `internal/queuegateway/config.go`.

The command tree also contains operator commands not currently built into the production image, including queue recovery, mTLS CA creation, artifact signing, and maintenance.

These are not automatically dead:

- Controlled queue recovery is required by the specification.
- mTLS is required for customer-VPC workers.
- Artifact signing supports worker releases.
- Backup guarantees are deferred, but maintenance reporting is already anticipated.

#### Recommendation

- Consider folding `gatewayconfig` into `queuegateway`.
- Rename `backendapi` to `dashboard` or move its read service beside the HTTP API.
- Document which commands are runtime, deployment-time, offline security tools, or future-only.
- Do not combine all operator tools into one binary until their secret and privilege requirements are compared.

**Risk: Low** for package moves and **high** for careless command consolidation.

---

### 8. Compatibility behavior needs explicit expiry conditions

**Classification: Revisit later**

Examples:

- Legacy `wt_local_` access tokens remain valid in `internal/auth/session.go`.
- Monitor reads fall back to the latest observation when no ordered reliability state exists in `internal/monitor/service.go`.
- Old PostgreSQL worker lease columns remain on `check_jobs`.
- The gateway contains an optional bearer-token identity adapter used by tests, while production requires mTLS.

These paths may have been justified during migration, but none has a clear removal date.

#### Simpler design

Record an explicit condition for each compatibility path:

- Remove legacy token acceptance after every possible old access token has expired.
- Backfill or initialize reliability state, then remove the latest-result fallback.
- After removing the old checker, add a new forward migration that drops its lease columns.
- Replace token-based gateway parity tests with mTLS identity tests, then remove or clearly isolate the token adapter.

Do not rewrite old migrations. Use a new forward migration for deployed databases.

**Risk: Medium-high**, because premature removal can invalidate sessions or produce incorrect monitor states.

## Complexity that should remain

| Area | Classification | Why |
|---|---|---|
| `destination` safety | Keep as-is | URL validation, DNS rebinding defence, redirect checks, and actual dialled-IP enforcement prevent SSRF and cloud metadata access. |
| `envelope` cryptography | Keep as-is | Canonical CBOR, signatures, X25519, HKDF, AES-GCM, bounded decoding, and schema compatibility are protocol/security requirements. |
| Transactional job/outbox | Keep as-is | PostgreSQL and SQS cannot share a transaction; the outbox prevents silently lost jobs. |
| `workqueue.Transport` | Keep as-is | It has two real implementations: direct SQS and HTTPS gateway. This is an earned interface, not a speculative abstraction. |
| Modular worker state machine | Keep as-is | Result publication must succeed before job acknowledgement; redelivery must not unnecessarily repeat completed checks. |
| SQLite worker journal | Keep as-is | It provides same-worker replay and durable result publication without giving the worker PostgreSQL access. |
| FIFO publisher/consumer/DLQ logic | Keep as-is | Leases, deduplication, ambiguous publication, conflict quarantine, and redrive are required for at-least-once queues. |
| Reliability calculation | Keep behavior; split file | Missing checks must be unknown, results can arrive out of order, and late results must repair reporting without rewriting old incidents. |
| Incident and notification outboxes | Keep as-is | They make incident transitions and email delivery durable and idempotent. |
| Authorization and tenant-qualified queries | Keep as-is | These prevent cross-organization data leaks. |
| HTTP request/response DTOs | Keep as-is | They stop database representations from becoming the public API contract. |
| SQLC generated files | Keep as-is | They are generated implementation detail, not code a beginner should normally read. |
| Separate API, worker, and gateway commands | Keep as-is | These enforce database and credential boundaries. Merging them for folder-count reduction would weaken security. |
| Small external-boundary interfaces | Keep as-is | Interfaces around SQS, HTTP, DNS, SMTP, and PostgreSQL make failure and concurrency behavior testable. |

## Current important folder structure

```text
cmd/
  api/
  monitor-engine/
  worker/
  notification-worker/
  queue-gateway/
  migrate/
  healthcheck/
  artifact-sign/
  deployment-keys/
  mtls-ca/
  maintenance/
  queue-admin/
  queue-recovery/
  worker-pool/

internal/
  httpapi/
  auth/
  authorization/
  ownership/
  monitor/
  backendapi/
  realtime/

  fifo/
  reliability/
  incident/
  notification/

  checkengine/
  modworker/
  workqueue/
  workerjournal/

  destination/
  envelope/
  secureheaders/
  quarantine/
  queuegateway/
  gatewayconfig/

  operations/
  workerpool/
  artifact/
  deploymentkeys/
  deploymentmanifest/
  mtlspki/

  platform/
    config/
    database/
      migrator/
      sqlc/
    httpserver/

db/
  migrations/
  queries/
```

## Proposed simpler folder structure

This proposal is intentionally conservative. A large package merger would make the code less safe and harder to test.

```text
cmd/
  # Runtime
  api/
  monitor-engine/
  worker/
  notification-worker/
  queue-gateway/

  # Support
  migrate/
  healthcheck/

  # Operator tools — remain separate, but clearly documented as a group
  artifact-sign/
  deployment-keys/
  mtls-ca/
  maintenance/
  queue-admin/
  queue-recovery/
  worker-pool/

internal/
  httpapi/               # optionally absorb the current backendapi read service
  auth/
  authorization/
  ownership/
  monitor/
  realtime/

  fifo/                  # the only scheduler/control-plane queue path
  reliability/
  incident/
  notification/

  checkengine/
  worker/                # clearer name for current modworker
  workqueue/
  workerjournal/

  destination/
  envelope/
  secureheaders/
  quarantine/
  queuegateway/          # absorb gatewayconfig/config.go

  operations/
  workerpool/
  artifact/
  deploymentkeys/
  deploymentmanifest/
  mtlspki/

  platform/
    config/              # command-specific config files
    database/{migrator,sqlc}/
    httpserver/
    mail/                # shared SMTP transport

db/
  migrations/
  queries/
```

The important reductions are the removal of `scheduler` and `checker`, consolidation of gateway configuration, and removal of partial/compatibility modes—not merging every domain into one enormous package.

## Classification summary

### Keep as-is

- Core SQS FIFO/outbox architecture.
- Database-free worker boundary.
- Worker transport interface and two adapters.
- Cryptographic envelope.
- Destination/SSRF protection.
- Worker journal and replay rules.
- Reliability, incident, and notification state-machine behavior.
- Tenant isolation and authorization.
- Separate runtime commands.
- SQLC and generated files.
- Most small external-boundary interfaces.

### Should simplify

- Conditional route registration and runtime type assertions.
- Partial service constructors.
- Raw SQL scattered through services.
- Large multi-purpose service files.
- Duplicate monitor row mappers.
- Duplicate SMTP transport.
- Repeated command configuration and startup code.
- Ambiguous names such as `backendapi` and `modworker`.

### Can remove — completed in Task 1

- `internal/scheduler`.
- `internal/checker`.
- Their SQLC query files and generated output.
- Tests and documentation that exclusively describe those obsolete paths.

### Revisit later

- Old `check_jobs` lease columns.
- Legacy access-token prefix.
- Monitor-state compatibility fallback.
- Gateway bearer-token test adapter.
- Migration squashing.
- Grouping or consolidation of operator commands.
- Phase 4-oriented maintenance/tooling that is not currently deployed.

## Ordered refactoring plan

- [x] **Task 1 — Remove the obsolete PostgreSQL scheduler/checker path**

  **Goal:** Establish one canonical check execution flow.

  **Files affected:** `internal/scheduler`, `internal/checker`, `db/queries/scheduler.sql`, `db/queries/checker.sql`, generated SQLC files, scheduler/checker integration tests, and docs.

  **Changes:** Remove obsolete source and queries; migrate any still-useful coverage to `fifo`, `checkengine`, or modular-worker tests; regenerate SQLC. Do not drop database columns yet.

  **Risk:** Medium. Useful edge-case tests could accidentally disappear.

  **Verification:** SQLC generation, unit/race tests, PostgreSQL tests, SQS scheduler/worker/result-consumer vertical slice, and destination-security tests.

  **Completion:** Finished on 2026-09-14. All specified verification passed. No schema migration was changed.

- [x] **Task 2 — Make API and service composition explicit**

  **Goal:** Ensure the complete API is determined at compile/startup time.

  **Files affected:** `internal/httpapi/router.go`, monitor/ownership route files, `internal/monitor/service.go`, `internal/ownership/service.go`, `internal/fifo/consumer.go`, `cmd/api/main.go`, and router/service tests.

  **Changes:** Remove lifecycle type assertions, require complete dependencies, validate router options, and replace partial constructors with explicit configuration.

  **Risk:** Medium. Test fixtures and route availability are affected.

  **Verification:** OpenAPI route comparison, router tests, authorization tests, monitor lifecycle tests, and startup configuration tests.

  **Completion:** Finished on 2026-09-14. The API now rejects incomplete composition before opening its listener, and every customer route is registered unconditionally.

  **Implementation report:**

  - `httpapi.Options` now contains the concrete production services. `NewRouter` validates every required dependency, returns an error for incomplete startup configuration, and registers the entire API without optional route branches.
  - The monitor API boundary now includes CRUD and lifecycle operations in one interface. The runtime lifecycle type assertion and its silently missing routes were removed.
  - The monitor service now has one validated constructor with an explicit encryption/signing configuration. The database-only, headers-only, and queue-enabled constructor variants were removed.
  - The ownership service now requires its account-action sender explicitly and validates it during construction. The variadic optional-sender mode and its later runtime failure were removed.
  - The FIFO result consumer now requires its quarantine sealer explicitly. Valid conflicting results are always encrypted and quarantined; the optional-quarantine mode was removed.
  - The DLQ reconciler now owns its database dependency directly instead of creating a partially configured result consumer, and it validates all of its dependencies when constructed.
  - Focused HTTP unit tests use a test-only route assembler, while PostgreSQL integration tests build the same complete service graph as production. This keeps partial fixtures out of runtime code.
  - A route contract test compares every registered `/api/v1` method/path with `api/customer-v1.openapi.yaml`, and constructor tests cover missing startup dependencies.

  **Verification completed:** Exact OpenAPI route comparison; focused router/service tests; complete PostgreSQL integration script; `go test -race ./... -count=1`; `go vet ./...`; `go build ./...`; and `git diff --check` all passed. No database schema or customer API contract changed.

- [x] **Task 3 — Split large files without changing behavior**

  **Goal:** Make each package readable one use case at a time.

  **Files affected:** Monitor, auth, ownership, reliability, backend read-model, and FIFO consumer packages.

  **Changes:** Move functions into focused files within the same package. Make this a file-movement task first; consolidate mappers only after behavior is unchanged.

  **Risk:** Low for moves; medium for mapper cleanup.

  **Verification:** No public API changes, full unit tests, `go vet`, race tests, and API contract tests.

  **Completion:** Finished on 2026-09-15. The largest affected production file is now under 300 lines; before this task, `internal/monitor/service.go` was about 800 lines.

  **Implementation report:**

  - Monitor code is separated into construction/creation, reads and row mapping, lifecycle/manual dispatch, and encrypted-header helpers.
  - Authentication code is separated into account signup/login, access-token authentication, email verification, password reset, session rotation/cleanup, and token-family persistence.
  - Ownership code is separated into shared types/construction, organization management, default hierarchy creation, invitations/membership, projects, environments, member management, and shared authorization/audit helpers.
  - Reliability code is separated into reporting, hourly/daily rollups, ordered state evaluation, and repair/retention maintenance.
  - Backend read-model code is separated into shared types/authorization, checks and reports, dashboard reads, incidents, and pagination helpers.
  - FIFO deadline sweeping now lives with maintenance operations, and result-DLQ recording lives with DLQ handling. The central result-consumption transaction remains intact because splitting that transaction would be a behavioral refactor rather than a file move.
  - No package boundaries, public names, function signatures, SQL statements, transaction boundaries, state-transition order, or customer API routes changed.
  - Monitor row mappers were moved together into `read.go` but intentionally not consolidated. That higher-risk cleanup is better paired with the SQLC work in Task 4.

  **Verification completed:** An AST/declaration audit confirmed all 174 declarations from the original large files were moved without changes; affected-package tests; exact OpenAPI route comparison through the HTTP API tests; complete PostgreSQL integration script; `go test -race ./... -count=1`; `go vet ./...`; `go build ./...`; and `git diff --check` all passed.

- [x] **Task 4 — Move API/domain SQL into SQLC**

  **Goal:** Put customer-facing database queries in one predictable location.

  **Files affected:** `internal/monitor`, `ownership`, `backendapi`, `incident`, `notification`, `realtime`, `db/queries`, and generated SQLC.

  **Changes:** Convert stable inline queries into named SQLC queries while leaving transaction orchestration in Go.

  **Risk:** High. Tenant predicates, row locks, and not-found behavior must remain exact.

  **Verification:** Fresh PostgreSQL integration database, tenant-isolation tests, RBAC tests, monitor CRUD/lifecycle tests, and incident/notification tests.

  **Completion:** Finished on 2026-09-15. All stable SQL statements were removed from the six Task 4 Go packages and are now named, typed SQLC queries.

  **Implementation report:**

  - Added focused query groups for ownership management, backend reads, incidents, notifications, and realtime events; extended the existing monitor query group for lifecycle and manual-dispatch operations.
  - Moved 69 stable statements into SQLC across authorization, tenant CRUD, monitor CRUD/lifecycle, checks and dashboards, incident transitions, notification leasing/retries, and realtime polling.
  - Regenerated the SQLC implementation and shared `Querier` interface. The generated files are intentionally larger, but customer-facing SQL now has one predictable source location under `db/queries`.
  - Kept transaction begin/commit/rollback decisions in Go. Authorization ordering, tenant predicates, `FOR UPDATE`, `FOR SHARE`, advisory locking, `SKIP LOCKED`, conflict handling, and `RowsAffected` checks remain explicit at the service level.
  - Kept queue and reliability-engine SQL out of this task because their concurrency-sensitive state machines are the separate scope of Task 5.
  - Replaced manual `Rows` and `Scan` loops with typed SQLC results and small conversions for nullable PostgreSQL values. No repository layer, ORM, generic data abstraction, schema migration, route, or public API was added.
  - Preserved report semantics while giving SQLC predictable types: latency is calculated from typed count/sum results, the earliest matching invalidation is selected directly, and the incident threshold start is selected from the same bounded newest-result set.

  **Verification completed:** Reproducible SQLC generation; zero inline SQL statements in the six scoped packages; focused package tests; fresh PostgreSQL integration tests covering tenant isolation, RBAC, monitor CRUD/lifecycle, incidents, realtime events, and notification retry/lease concurrency; `go test -race ./... -count=1`; `go vet ./...`; `go build ./...`; `go mod verify`; `go mod tidy -diff`; and `git diff --check` all passed.

- [ ] **Task 5 — Move monitoring-engine SQL into SQLC**

  **Goal:** Make queue and reliability persistence easier to inspect without changing state machines.

  **Files affected:** `internal/fifo`, `reliability`, `operations`, `workerpool`, the queue-recovery command, `db/queries`, and generated SQLC.

  **Changes:** Name scheduler, publisher, consumer, rollup, retention, DLQ, and operational queries; retain explicit Go transactions and ordering.

  **Risk:** High because this includes concurrency- and idempotency-sensitive SQL.

  **Verification:** Race tests, concurrent scheduler/publisher tests, duplicate-result tests, DLQ/redrive tests, late-result correction tests, and rollup/retention tests.

- [ ] **Task 6 — Consolidate SMTP transport**

  **Goal:** Maintain one secure SMTP implementation.

  **Files affected:** `internal/auth/verification.go`, `internal/notification/smtp.go`, new `internal/platform/mail`, related tests, and API configuration.

  **Changes:** Share transport/TLS/authentication/mailbox safety; retain domain-specific messages and error mapping.

  **Risk:** Medium. Authentication delivery and alerts can both be affected.

  **Verification:** Local SMTP capture tests, OCI configuration validation, STARTTLS/authentication failure tests, header-injection tests, and log/credential-redaction tests.

- [ ] **Task 7 — Standardize configuration and command startup**

  **Goal:** Make each `main` read as load config -> build dependencies -> run.

  **Files affected:** Worker, monitor-engine, notification-worker, and queue-gateway commands; `internal/platform/config`; container/configuration tests.

  **Changes:** Add typed command-specific loaders, replace panics with errors, centralize key-file/TLS/environment parsing and repeated lifecycle helpers, and preserve worker/gateway database-free boundaries.

  **Risk:** Medium. Deployment environment compatibility is sensitive.

  **Verification:** Environment-variable matrix tests, Docker builds, Coolify Compose validation, readiness/liveness tests, and graceful-shutdown tests.

- [ ] **Task 8 — Retire time-bounded compatibility and legacy schema**

  **Goal:** Remove transitional behavior only after its operational need has ended.

  **Files affected:** `internal/auth/session.go`, monitor state reading, gateway test authentication, `check_jobs` schema, a new forward migration, docs, and tests.

  **Changes:** Remove legacy token parsing after expiry, backfill reliability state before removing fallback reads, replace bearer gateway tests with mTLS tests, and drop old checker lease columns through a new migration.

  **Risk:** High if done prematurely.

  **Verification:** Fresh and upgraded database migrations, active-session expiry audit, reliability-state backfill tests, mTLS gateway tests, and a full deployment smoke test.

## Priority recommendation

The largest immediate improvement would come from:

1. ~~Removing the old scheduler/checker.~~ Completed in Task 1.
2. Removing partial runtime composition.
3. Splitting the largest files without behavioral changes.
4. Standardizing SQL location.
5. Consolidating SMTP and command configuration.
6. Removing compatibility branches only after explicit expiry/backfill checks.

## Final assessment

WatchTrace is **not fundamentally over-engineered relative to its design specification**. It is a security- and reliability-sensitive distributed backend, so substantial complexity is legitimate.

It is, however, carrying obsolete and transitional code that makes the real architecture much harder for a Go beginner to see. Removing that second story—and making configuration and SQL organization consistent—would deliver more clarity than collapsing the necessary queue, security, or worker boundaries.
