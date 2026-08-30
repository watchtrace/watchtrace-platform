#!/bin/sh
set -eu
repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
temporary_directory=$(mktemp -d)
trap 'rm -rf "$temporary_directory"' EXIT HUP INT TERM
export PATH="$repository_root/tests/fixtures/coolify:$PATH"
export MOCK_CURL_STATE="$temporary_directory/state"
export COOLIFY_API_URL=https://coolify.example.test
export COOLIFY_TOKEN=test-token
export COOLIFY_DEPLOY_POLL_SECONDS=1
export COOLIFY_DEPLOY_TIMEOUT_SECONDS=2
mkdir "$MOCK_CURL_STATE"
printf '%s\n' 'ghcr.io/watchtrace/watchtrace-platform:main' > "$MOCK_CURL_STATE/control"
printf '%s\n' 'ghcr.io/watchtrace/watchtrace-worker:main' > "$MOCK_CURL_STATE/worker"
export GITHUB_OUTPUT="$temporary_directory/output"

references=$($repository_root/scripts/coolify-deploy.sh inspect-compose backend-app)
printf '%s' "$references" | grep -F 'ghcr.io/watchtrace/watchtrace-platform:main' >/dev/null
printf '%s' "$references" | grep -F 'ghcr.io/watchtrace/watchtrace-worker:main' >/dev/null

control_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
worker_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
$repository_root/scripts/coolify-deploy.sh deploy-compose backend-app \
  ghcr.io/watchtrace/watchtrace-platform "$control_digest" \
  ghcr.io/watchtrace/watchtrace-worker "$worker_digest"
grep -F 'previous_control_reference=ghcr.io/watchtrace/watchtrace-platform:main' "$GITHUB_OUTPUT" >/dev/null
grep -F 'control_reference=ghcr.io/watchtrace/watchtrace-platform@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$GITHUB_OUTPUT" >/dev/null
grep -F 'worker_reference=ghcr.io/watchtrace/watchtrace-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "$GITHUB_OUTPUT" >/dev/null
grep -F 'deployment_uuid=deployment-backend-app' "$GITHUB_OUTPUT" >/dev/null

if COOLIFY_API_URL=http://coolify.example.test $repository_root/scripts/coolify-deploy.sh inspect-compose backend-app >/dev/null 2>&1; then
  echo "The helper accepted an insecure Coolify API URL." >&2
  exit 1
fi
echo "Coolify backend Compose deployment helper tests passed."
