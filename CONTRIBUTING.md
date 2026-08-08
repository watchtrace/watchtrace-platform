# Contributing

## Repository Scope

This repository is the WatchTrace backend monorepo. It owns Go commands,
backend modules, database assets, backend tests, deployment definitions, and
the versioned API contract. The React and TypeScript application belongs in a
separate frontend repository and integrates through the documented HTTP and
Server-Sent Events contract.

Keep changes within the current implementation-plan task. In particular, do
not add tracing or later-phase infrastructure while working on Phase 1 uptime
monitoring.

## Before Starting

1. Install the tools listed in `README.md`.
2. Read `DESIGN_SPECIFICATION.md`, `RISKS_AND_CAVEATS.md`, and the relevant
   task in `PHASE_1_IMPLEMENTATION_PLAN.md`.
3. Check the task dependencies and existing uncommitted changes.
4. Use the task ID in the branch and pull request where practical, for example
   `p1-002-initialize-go-application`.

## Development Rules

- Prefer focused changes that follow the documented phase order.
- Keep product rules in their owning module and technical helpers under
  `internal/platform`.
- Do not commit credentials, tokens, cookies, private keys, or real `.env`
  files.
- Treat organization boundaries as part of every tenant-owned query and test.
- Add tests appropriate to the behavior and risk of the change.
- Update documentation when commands, configuration, or behavior changes.
- Mark a plan checkbox complete only after its acceptance criteria pass.

## Verification

Format changed Go files and run the repository checks before opening a pull
request:

```sh
go mod download
go mod verify
test -z "$(gofmt -l .)"
go mod tidy -diff
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate -f db/sqlc.yaml
git diff --exit-code -- db internal/platform/database/sqlc
go vet ./...
go test -race ./...
go build ./...
./tests/integration/postgres-database.sh
```

These are the same checks enforced by the backend CI workflow. The final
command requires Docker with Compose and runs the migrations and generated
query against an isolated PostgreSQL container. Frontend checks will be added
in the separate frontend repository when its implementation begins.

## Pull Requests

Describe the task ID, behavior changed, verification performed, and any known
risk or follow-up. Keep generated artifacts, unrelated formatting changes, and
later-phase scaffolding out of the pull request.
