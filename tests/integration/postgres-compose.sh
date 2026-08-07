#!/bin/sh

set -eu

project_name="watchtrace-p1-003-$$"
repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
compose_file="$repository_root/docker-compose.yml"
environment_file=$(mktemp "${TMPDIR:-/tmp}/watchtrace-p1-003-postgres.XXXXXX")

compose() {
    env WATCHTRACE_POSTGRES_ENV_FILE="$environment_file" \
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

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker with the Compose v2 plugin is required." >&2
    exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
    echo "Docker with the Compose v2 plugin is required." >&2
    exit 1
fi

cat >"$environment_file" <<'EOF'
POSTGRES_DB=watchtrace_test
POSTGRES_USER=watchtrace_test
POSTGRES_PASSWORD=p1-003-local-test-password
EOF

compose config --quiet
compose up --detach --wait postgres
compose exec --no-TTY postgres psql \
    --username watchtrace_test \
    --dbname watchtrace_test \
    --set ON_ERROR_STOP=1 \
    --command "CREATE TABLE p1_003_persistence_probe (id integer PRIMARY KEY); INSERT INTO p1_003_persistence_probe VALUES (1);"

# Recreate the container without deleting its named volume.
compose down
compose up --detach --wait postgres

row_count=$(compose exec --no-TTY postgres psql \
    --username watchtrace_test \
    --dbname watchtrace_test \
    --tuples-only \
    --no-align \
    --command "SELECT count(*) FROM p1_003_persistence_probe WHERE id = 1;")

if [ "$(printf '%s' "$row_count" | tr -d '[:space:]')" != "1" ]; then
    echo "PostgreSQL data did not persist after container recreation." >&2
    exit 1
fi
