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

- [`AUTHENTICATION.md`](AUTHENTICATION.md) defines the minimal signup, login,
  and short-lived local-development bearer session contract.
- [`API_CONVENTIONS.md`](API_CONVENTIONS.md) defines request IDs, error
  envelopes, JSON validation, health endpoints, and safe request logging.
- [`OWNERSHIP_SCHEMA.md`](OWNERSHIP_SCHEMA.md) documents the Phase 1 account
  and tenant-ownership tables and their database-enforced isolation rules.
- [`OWNERSHIP_API.md`](OWNERSHIP_API.md) defines the authenticated, atomic
  organization/project/production-environment creation contract.
- [`MONITORS.md`](MONITORS.md) defines the initial tenant-scoped GET monitor
  create/list API, defaults, bounds, and outbound destination-safety boundary.
- [`SCHEDULER.md`](SCHEDULER.md) defines the initial durable PostgreSQL
  scheduler transaction and queue invariants.
- [`CHECKER.md`](CHECKER.md) defines leased job claiming, guarded HTTP
  execution, idempotent result storage, and the response-body boundary.

Use this directory for documentation introduced by later tasks, including
architecture decision records, operations runbooks, API guides, and deployment
procedures. Add those directories only when their owning task creates content;
do not keep empty scaffolding in Git.
