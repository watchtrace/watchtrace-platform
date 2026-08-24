#!/bin/sh

set -eu

project_name="watchtrace-p1-005-$$"
repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
compose_file="$repository_root/docker-compose.yml"
environment_file=$(mktemp "${TMPDIR:-/tmp}/watchtrace-p1-005-postgres.XXXXXX")

compose() {
    env WATCHTRACE_POSTGRES_ENV_FILE="$environment_file" \
        WATCHTRACE_POSTGRES_PORT=0 \
        docker compose \
        --file "$compose_file" \
        --project-name "$project_name" \
        "$@"
}

cleanup() {
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    rm -f -- "$environment_file"
}

trap cleanup EXIT HUP INT TERM

if ! docker compose version >/dev/null 2>&1; then
    echo "Docker with the Compose v2 plugin is required." >&2
    exit 1
fi

cat >"$environment_file" <<'EOF'
POSTGRES_DB=watchtrace_test
POSTGRES_USER=watchtrace_test
POSTGRES_PASSWORD=p1-005-local-test-password
EOF

compose config --quiet
compose up --detach --wait postgres

published_address=$(compose port postgres 5432)
database_port=${published_address##*:}
case "$database_port" in
    *[!0-9]* | "")
        echo "Could not determine the PostgreSQL test port." >&2
        exit 1
        ;;
esac

database_url="postgres://watchtrace_test:p1-005-local-test-password@127.0.0.1:$database_port/watchtrace_test?sslmode=disable"

cd "$repository_root"

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate up
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 14 (clean)" ]; then
    echo "Migration version after up was '$version'." >&2
    exit 1
fi

env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    go test ./tests/integration -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 13 (clean)" ]; then
    echo "Migration version after down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestBackendPhase14SchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 12 (clean)" ]; then
    echo "Migration version after second down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_INCIDENT_NOTIFICATION_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestIncidentNotificationSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 11 (clean)" ]; then
    echo "Migration version after second down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_RELIABILITY_REPORTING_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestReliabilityReportingSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 10 (clean)" ]; then
    echo "Migration version after second down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestReliableEngineSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 9 (clean)" ]; then
    echo "Migration version after second down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestMembershipTenantSecuritySchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 8 (clean)" ]; then
    echo "Migration version after third down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_PASSWORD_RESET_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestPasswordResetSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 7 (clean)" ]; then
    echo "Migration version after fourth down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestEmailVerificationSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 6 (clean)" ]; then
    echo "Migration version after fifth down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestProductionAuthSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate down
version=$(env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate version)
if [ "$version" != "version 5 (clean)" ]; then
    echo "Migration version after sixth down was '$version'." >&2
    exit 1
fi
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT=1 \
    go test ./tests/integration -run '^TestHTTPCheckWorkerSchemaRollback$' -count=1

env WATCHTRACE_DATABASE_URL="$database_url" go run ./cmd/migrate up
env WATCHTRACE_TEST_DATABASE_URL="$database_url" \
    go test ./tests/integration -count=1
