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
paths. Database migrations and queries will be added under `db/` by P1-005;
later service, deployment, and test directories will be added only when their
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

## Verify

```sh
go test ./...
go vet ./...
go build ./...
```

## Contributing and License

See `CONTRIBUTING.md` for the development workflow and repository boundaries.
This repository is proprietary and all rights are reserved; see `LICENSE` for
the license decision.
