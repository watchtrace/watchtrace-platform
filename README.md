# WatchTrace Platform

Backend monorepo for WatchTrace. The Phase 1 backend is a modular Go
application; the React frontend will live in a separate repository.

This repository owns backend commands and modules, database assets, backend
tests, deployment definitions, and the versioned HTTP and Server-Sent Events
contract. The frontend repository consumes that contract and does not import
backend source code.

## Repository Layout

```text
cmd/                 Application entry points
internal/            Backend modules and shared platform code
deploy/              Local and future deployment definitions
tests/integration/   Cross-component integration tests
docs/                Documentation index and future task-owned documentation
```

The design specification, Phase 1 implementation plan, and risk register stay
at the repository root because the implementation workflow uses their stable
paths. Database migrations, SQLC queries, and SQLC configuration live under
`db/`; generated database code lives under `internal/platform/database/sqlc`.
Later service, deployment, and test directories will be added only when their
owning task begins. See [`docs/README.md`](docs/README.md) for the documentation
index.

## Requirements

- Git
- Go 1.26.5
- Docker Engine with the Docker Compose v2 plugin
- PowerShell 7 only when running the Windows commands or the PostgreSQL
  integration-test script

## First-Time Setup

On macOS or Linux:

```sh
git clone https://github.com/watchtrace/watchtrace-platform.git
cd watchtrace-platform
go mod download
cp deploy/local/postgres.env.example deploy/local/postgres.env
cp .env.example .env
# Replace both password placeholders with the same local-only password.
chmod 600 deploy/local/postgres.env
chmod 600 .env
docker compose up --detach --wait postgres
set -a
. ./.env
set +a
go run ./cmd/migrate up
go test ./...
```

On Windows PowerShell:

```powershell
git clone https://github.com/watchtrace/watchtrace-platform.git
Set-Location watchtrace-platform
go mod download
Copy-Item deploy/local/postgres.env.example deploy/local/postgres.env
Copy-Item .env.example .env
# Replace both password placeholders with the same local-only password.
docker compose up --detach --wait postgres
Get-Content .env | Where-Object { $_ -match '^[A-Za-z_][A-Za-z0-9_]*=' } | ForEach-Object {
    $name, $value = $_ -split '=', 2
    Set-Item -Path "Env:$name" -Value $value
}
go run ./cmd/migrate up
go test ./...
```

## Run the API

The application reads its configuration from the process environment. The
local `.env` file is a convenience template and is not loaded implicitly.

On macOS or Linux, load it into the current shell and run the API:

```sh
set -a
. ./.env
set +a
go run ./cmd/api
```

On Windows PowerShell:

```powershell
Get-Content .env | Where-Object { $_ -match '^[A-Za-z_][A-Za-z0-9_]*=' } | ForEach-Object {
    $name, $value = $_ -split '=', 2
    Set-Item -Path "Env:$name" -Value $value
}
go run ./cmd/api
```

The API listens on `http://localhost:8080`. Verify its health endpoint with:

```sh
curl http://localhost:8080/health
```

Or, in Windows PowerShell:

```powershell
Invoke-RestMethod http://localhost:8080/health
```

Liveness is also available at `/health/live`; readiness is available at
`/health/ready`. Every response includes `X-Request-ID`, and API failures use a
consistent JSON error envelope. See
[`docs/API_CONVENTIONS.md`](docs/API_CONVENTIONS.md) for request ID, validation,
error, health, and safe-logging conventions.

Stop the process with `Ctrl+C`; the server stops accepting new connections and
allows active requests up to 10 seconds to finish.

## Local PostgreSQL

The Compose service runs PostgreSQL 18.4 on `127.0.0.1:5432` and stores its
data in the named `postgres_data` development volume. Local bootstrap values
live in the ignored `deploy/local/postgres.env` file. They are development-only
credentials and must never be reused for production.

Start PostgreSQL and wait for its health check:

```sh
docker compose up --detach --wait postgres
docker compose ps postgres
```

Stop PostgreSQL without deleting development data:

```sh
docker compose down
```

To deliberately reset the local database, delete its named volume:

```sh
docker compose down --volumes
```

The PostgreSQL Compose integration tests use an isolated project and volume,
then verify that data survives container recreation. On macOS or Linux, run:

```sh
./tests/integration/postgres-compose.sh
```

On Windows PowerShell, run:

```powershell
pwsh -NoProfile -File ./tests/integration/postgres-compose.ps1
```

## Database Migrations and SQLC

Database migrations are embedded into the migration command, so these commands
work from any directory after `WATCHTRACE_DATABASE_URL` is exported:

```sh
go run ./cmd/migrate up
go run ./cmd/migrate version
go run ./cmd/migrate down
```

`up` applies every pending migration. `down` rolls back exactly one migration
to reduce the chance of an accidental full rollback. Migration errors and logs
never print the configured database URL.

SQL queries live in `db/queries`, migrations in `db/migrations`, and generated
Go code in `internal/platform/database/sqlc`. Regenerate it with the pinned
SQLC release:

```sh
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate -f db/sqlc.yaml
```

Run the clean-database migration and generated-query integration test on macOS
or Linux with:

```sh
./tests/integration/postgres-database.sh
```

On Windows PowerShell, run:

```powershell
pwsh -NoProfile -File ./tests/integration/postgres-database.ps1
```

## Verify

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

The `Backend CI` GitHub Actions workflow runs these checks for every push and
pull request, and can also be started manually. It selects the toolchain from
`go.mod`, verifies committed SQLC output, and runs the migration and generated
query tests against an isolated PostgreSQL container. The workflow has
read-only repository permissions and does not use production credentials.

## Contributing and License

See `CONTRIBUTING.md` for the development workflow and repository boundaries.
This repository is proprietary and all rights are reserved; see `LICENSE` for
the license decision.
