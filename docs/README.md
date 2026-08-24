# WatchTrace Backend Documentation

The authoritative Phase 1 planning documents remain at the repository root so
the implementation workflow and task prompts can locate them consistently:

- [`DESIGN_SPECIFICATION.md`](../DESIGN_SPECIFICATION.md) defines the product
  behavior and architecture.
- [`PHASE_1_IMPLEMENTATION_PLAN.md`](../PHASE_1_IMPLEMENTATION_PLAN.md) defines
  implementation order, acceptance criteria, and release gates.
- [`RISKS_AND_CAVEATS.md`](../RISKS_AND_CAVEATS.md) records risks and required
  controls.

Current implementation documentation:

- [`AUTHENTICATION.md`](AUTHENTICATION.md) defines signup, email verification, password reset, login,
  and short-lived local-development bearer session contract.
- [`API_CONVENTIONS.md`](API_CONVENTIONS.md) defines request IDs, error
  envelopes, JSON validation, health endpoints, and safe request logging.
- [`OWNERSHIP_SCHEMA.md`](OWNERSHIP_SCHEMA.md) documents the Phase 1 account
  and tenant-ownership tables and their database-enforced isolation rules.
- [`OWNERSHIP_API.md`](OWNERSHIP_API.md) defines the authenticated, atomic
  organization/project/production-environment creation contract.
- [`MONITORS.md`](MONITORS.md) defines the tenant-scoped GET/HEAD lifecycle,
  encrypted custom headers, current-state semantics, and outbound safety.
- [`SCHEDULER.md`](SCHEDULER.md) defines atomic FIFO scheduling and immutable
  publication intent.
- [`RELIABILITY_REPORTING.md`](RELIABILITY_REPORTING.md) defines expected slots,
  coverage, ordered state correction, rollups, and retention.
- [`CHECKER.md`](CHECKER.md) defines bounded database-free execution and
  idempotent result storage.
- [`MODULAR_WORKER.md`](MODULAR_WORKER.md) documents worker deployment and key
  protection.
- [`QUEUE_GATEWAY.md`](QUEUE_GATEWAY.md) documents the stateless mTLS adapter.
- [`AWS_SQS_RUNBOOK.md`](AWS_SQS_RUNBOOK.md) defines manual Phase 1 queue
  provisioning, least-privilege roles, manifest verification, and recovery.
- [`../api/worker-v1.openapi.yaml`](../api/worker-v1.openapi.yaml) is the
  independent current/previous HTTPS worker protocol contract.

Use this directory for documentation introduced by later tasks, including
architecture decision records, operations runbooks, API guides, and deployment
procedures. Add those directories only when their owning task creates content;
do not keep empty scaffolding in Git.
