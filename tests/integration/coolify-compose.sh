#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
compose_file="$repository_root/deploy/coolify/compose.yml"
rendered_file=$(mktemp)
rendered_json=$(mktemp)
portable_compose=$(mktemp)
trap 'rm -f "$rendered_file" "$rendered_json" "$portable_compose"' EXIT HUP INT TERM

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1 \
    || ! command -v jq >/dev/null 2>&1; then
    echo "Docker Compose v2 and jq are required." >&2
    exit 1
fi

export WATCHTRACE_CONTROL_IMAGE=ghcr.io/example/watchtrace-platform@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export WATCHTRACE_WORKER_IMAGE=ghcr.io/example/watchtrace-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export WATCHTRACE_FRONTEND_IMAGE=ghcr.io/example/watchtrace-console@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
export POSTGRES_PASSWORD=test-only-url-safe-password
export WATCHTRACE_DATABASE_URL=postgres://watchtrace:test-only-url-safe-password@postgres:5432/watchtrace?sslmode=disable
export WATCHTRACE_PUBLIC_URL=https://watchtrace.example.test
export WATCHTRACE_SMTP_ADDRESS=smtp.email.example.test:587
export WATCHTRACE_SMTP_USERNAME=test-smtp-user
export WATCHTRACE_SMTP_PASSWORD=test-smtp-password
export WATCHTRACE_EMAIL_FROM=watchtrace@example.test
export AWS_REGION=ap-south-1
export AWS_ACCESS_KEY_ID=TESTONLYACCESSKEY
export AWS_SECRET_ACCESS_KEY=test-only-secret-key
export WATCHTRACE_SQS_HOSTED_JOB_QUEUE_URL=https://sqs.ap-south-1.amazonaws.com/123456789012/watchtrace-dev-check-jobs-hosted.fifo
export WATCHTRACE_SQS_HOSTED_JOB_DLQ_URL=https://sqs.ap-south-1.amazonaws.com/123456789012/watchtrace-dev-check-jobs-hosted-dlq.fifo
export WATCHTRACE_SQS_RESULT_QUEUE_URL=https://sqs.ap-south-1.amazonaws.com/123456789012/watchtrace-dev-check-results.fifo
export WATCHTRACE_SQS_RESULT_DLQ_URL=https://sqs.ap-south-1.amazonaws.com/123456789012/watchtrace-dev-check-results-dlq.fifo
export WATCHTRACE_PLATFORM_SIGNING_PRIVATE_KEY=TEST_ONLY_PRIVATE_KEY
export WATCHTRACE_PLATFORM_SIGNING_PUBLIC_KEY=TEST_ONLY_PUBLIC_KEY
export WATCHTRACE_MONITOR_HEADER_KEY=TEST_ONLY_HEADER_KEY
export WATCHTRACE_QUARANTINE_KEY=TEST_ONLY_QUARANTINE_KEY
export WATCHTRACE_WORKER_ENCRYPTION_PRIVATE_KEY=TEST_ONLY_ENCRYPTION_KEY
export WATCHTRACE_WORKER_RESULT_PRIVATE_KEY=TEST_ONLY_RESULT_KEY

# exclude_from_hc is a documented Coolify extension for one-shot migration
# services. Remove only that extension before validating the remaining file
# against the standard Docker Compose schema.
if ! grep -Eq '^[[:space:]]+exclude_from_hc:[[:space:]]+true$' "$compose_file"; then
    echo "The one-shot migration must be excluded from Coolify health checks." >&2
    exit 1
fi
sed '/^[[:space:]]*exclude_from_hc:[[:space:]]*true$/d' "$compose_file" >"$portable_compose"

docker compose --file "$portable_compose" config --quiet
docker compose --file "$portable_compose" config >"$rendered_file"
docker compose --file "$portable_compose" config --format json >"$rendered_json"

if grep -Eq '^[[:space:]]+ports:' "$rendered_file"; then
    echo "The private stack must not publish host ports in Compose." >&2
    exit 1
fi

for service in postgres api monitor-engine hosted-worker notification-worker frontend; do
    if ! jq -e --arg service "$service" '.services[$service].healthcheck' \
        "$rendered_json" >/dev/null; then
        echo "$service must define a health check." >&2
        exit 1
    fi
done

if jq -e '.services["hosted-worker"].environment.WATCHTRACE_DATABASE_URL' \
    "$rendered_json" >/dev/null; then
    echo "The hosted worker must not receive a PostgreSQL URL." >&2
    exit 1
fi

echo "Coolify Compose configuration is valid."
