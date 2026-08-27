#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
image_name="watchtrace-worker:test-$$"
container_name="watchtrace-worker-inspect-$$"

cleanup() {
    docker rm --force "$container_name" >/dev/null 2>&1 || true
    docker image rm --force "$image_name" >/dev/null 2>&1 || true
}

trap cleanup EXIT HUP INT TERM

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker is required." >&2
    exit 1
fi

cd "$repository_root"

docker build --check --file Dockerfile.worker .
docker build --file Dockerfile.worker --tag "$image_name" .

configured_user=$(docker image inspect --format '{{.Config.User}}' "$image_name")
if [ "$configured_user" != "65532:65532" ]; then
    echo "Worker image user was '$configured_user', expected '65532:65532'." >&2
    exit 1
fi

configured_entrypoint=$(docker image inspect --format '{{json .Config.Entrypoint}}' "$image_name")
if [ "$configured_entrypoint" != '["/watchtrace-worker"]' ]; then
    echo "Worker image entrypoint was '$configured_entrypoint'." >&2
    exit 1
fi

docker create --name "$container_name" "$image_name" >/dev/null
for binary in watchtrace-worker watchtrace-healthcheck
do
    if ! docker export "$container_name" | tar -tf - | grep -qx "$binary"; then
        echo "Worker image is missing /$binary." >&2
        exit 1
    fi
done
