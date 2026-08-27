#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
image_name="watchtrace-p1-008:test-$$"
container_name="watchtrace-p1-008-$$"

cleanup() {
    docker rm --force "$container_name" >/dev/null 2>&1 || true
    docker image rm --force "$image_name" >/dev/null 2>&1 || true
}

trap cleanup EXIT HUP INT TERM

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker is required." >&2
    exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required." >&2
    exit 1
fi

cd "$repository_root"

docker build --check .
docker build --tag "$image_name" .

configured_user=$(docker image inspect --format '{{.Config.User}}' "$image_name")
if [ "$configured_user" != "65532:65532" ]; then
    echo "Container image user was '$configured_user', expected '65532:65532'." >&2
    exit 1
fi

configured_command=$(docker image inspect --format '{{json .Config.Cmd}}' "$image_name")
if [ "$configured_command" != '["/watchtrace-api"]' ]; then
    echo "Container image command was '$configured_command'." >&2
    exit 1
fi

docker run --detach \
    --name "$container_name" \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --publish 127.0.0.1::8080 \
    --env 'WATCHTRACE_DATABASE_URL=postgres://watchtrace@postgres/watchtrace?sslmode=disable' \
    "$image_name" >/dev/null

for binary in \
    watchtrace-api \
    watchtrace-migrate \
    watchtrace-monitor-engine \
    watchtrace-notification-worker \
    watchtrace-worker-pool \
    watchtrace-queue-admin \
    watchtrace-healthcheck
do
    if ! docker export "$container_name" | tar -tf - | grep -qx "$binary"; then
        echo "Container image is missing /$binary." >&2
        exit 1
    fi
done

published_address=$(docker port "$container_name" 8080/tcp)
attempt=0
response=""
while [ "$attempt" -lt 30 ]; do
    if response=$(curl --fail --silent --show-error \
        --max-time 2 "http://$published_address/health/live" 2>/dev/null); then
        break
    fi
    attempt=$((attempt + 1))
    sleep 1
done

if [ "$response" != '{"status":"ok"}' ]; then
    echo "Container liveness response was '$response'." >&2
    docker logs "$container_name" >&2
    exit 1
fi

docker exec "$container_name" /watchtrace-healthcheck http://127.0.0.1:8080/health/live

docker stop --time 15 "$container_name" >/dev/null
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$container_name")
if [ "$exit_code" != "0" ]; then
    echo "Container exited with status $exit_code after SIGTERM." >&2
    docker logs "$container_name" >&2
    exit 1
fi
