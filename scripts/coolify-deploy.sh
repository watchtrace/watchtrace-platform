#!/bin/sh
set -eu

usage() {
  cat >&2 <<'EOF'
Usage:
  coolify-deploy.sh inspect-compose COMPOSE_UUID
  coolify-deploy.sh deploy-compose COMPOSE_UUID CONTROL_IMAGE sha256:DIGEST WORKER_IMAGE sha256:DIGEST

Required environment variables:
  COOLIFY_API_URL   Trusted HTTPS Coolify origin, with or without /api/v1
  COOLIFY_TOKEN     Coolify API token with read, write, and deploy permissions
EOF
  exit 2
}

fail() { echo "Coolify deployment error: $*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || fail "curl is required."
command -v jq >/dev/null 2>&1 || fail "jq is required."
: "${COOLIFY_API_URL:?Set COOLIFY_API_URL}"
: "${COOLIFY_TOKEN:?Set COOLIFY_TOKEN}"
case "$COOLIFY_API_URL" in https://*) ;; *) fail "COOLIFY_API_URL must use HTTPS." ;; esac

api_base=${COOLIFY_API_URL%/}
case "$api_base" in */api/v1) ;; *) api_base="$api_base/api/v1" ;; esac
timeout_seconds=${COOLIFY_DEPLOY_TIMEOUT_SECONDS:-1200}
poll_seconds=${COOLIFY_DEPLOY_POLL_SECONDS:-10}
case "$timeout_seconds:$poll_seconds" in *[!0-9:]*|:*|*:) fail "Timeout values must be positive integers." ;; esac
[ "$timeout_seconds" -gt 0 ] && [ "$poll_seconds" -gt 0 ] || fail "Timeout values must be greater than zero."

api_request() {
  method=$1; path=$2; body=${3-}
  if [ -n "$body" ]; then
    curl --fail-with-body --silent --show-error --proto '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 60 --request "$method" \
      --header "Authorization: Bearer $COOLIFY_TOKEN" \
      --header 'Content-Type: application/json' --data "$body" "$api_base$path"
  else
    curl --fail-with-body --silent --show-error --proto '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 60 --request "$method" \
      --header "Authorization: Bearer $COOLIFY_TOKEN" "$api_base$path"
  fi
}

validate_uuid() { case "$1" in ''|*[!A-Za-z0-9_-]*) fail "Invalid Coolify resource UUID." ;; esac; }
validate_image() {
  case "$1" in ghcr.io/*/*) ;; *) fail "Expected a fully qualified ghcr.io/OWNER/IMAGE name." ;; esac
  case "$1" in *@*|*:*) fail "IMAGE_NAME must not contain a tag or digest." ;; esac
}
immutable_reference() {
  image=$1; digest=$2
  case "$digest" in sha256:*) hex=${digest#sha256:} ;; *) fail "Image digest must start with sha256:." ;; esac
  [ "${#hex}" -eq 64 ] || fail "Image digest must contain exactly 64 hexadecimal characters."
  case "$hex" in *[!0-9a-f]*) fail "Image digest must use lowercase hexadecimal characters." ;; esac
  printf '%s@sha256:%s' "$image" "$hex"
}
write_output() { if [ -n "${GITHUB_OUTPUT:-}" ]; then printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT"; fi; }
summary() { if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then printf '%s\n' "$1" >> "$GITHUB_STEP_SUMMARY"; fi; }

get_application() {
  application_json=$(api_request GET "/applications/$1") || fail "Could not read Coolify application $1."
  [ "$(printf '%s' "$application_json" | jq -r '.uuid // empty')" = "$1" ] || fail "Coolify returned a different application."
}
get_compose_images() {
  env_json=$(api_request GET "/applications/$1/envs") || fail "Could not read Compose image variables for $1."
  control_reference=$(printf '%s' "$env_json" | jq -r '[.[] | select(.key == "WATCHTRACE_CONTROL_IMAGE" and .is_preview != true)][0].value // empty')
  worker_reference=$(printf '%s' "$env_json" | jq -r '[.[] | select(.key == "WATCHTRACE_WORKER_IMAGE" and .is_preview != true)][0].value // empty')
  [ -n "$control_reference" ] || fail "WATCHTRACE_CONTROL_IMAGE is missing or unreadable. Keep this image variable non-secret."
  [ -n "$worker_reference" ] || fail "WATCHTRACE_WORKER_IMAGE is missing or unreadable. Keep this image variable non-secret."
}
update_image_variables() {
  uuid=$1; control=$2; worker=$3
  body=$(jq -cn --arg control "$control" --arg worker "$worker" \
    '{data:[
      {key:"WATCHTRACE_CONTROL_IMAGE",value:$control,is_preview:false,is_literal:true},
      {key:"WATCHTRACE_WORKER_IMAGE",value:$worker,is_preview:false,is_literal:true}
    ]}')
  api_request PATCH "/applications/$uuid/envs/bulk" "$body" >/dev/null || fail "Could not update backend image variables for $uuid."
}
trigger_and_wait() {
  uuid=$1
  body=$(jq -cn --arg uuid "$uuid" '{uuid:$uuid,force:false}')
  response=$(api_request POST /deploy "$body") || fail "Coolify rejected deployment of $uuid."
  deployment_uuid=$(printf '%s' "$response" | jq -r --arg uuid "$uuid" '.deployments[]? | select(.resource_uuid == $uuid) | .deployment_uuid' | head -n 1)
  [ -n "$deployment_uuid" ] || fail "Coolify did not return a deployment UUID for $uuid."
  started_at=$(date +%s)
  while :; do
    deployment_json=$(api_request GET "/deployments/$deployment_uuid") || fail "Could not read deployment $deployment_uuid."
    status=$(printf '%s' "$deployment_json" | jq -r '.status // empty')
    case "$status" in
      finished|success|successful) break ;;
      queued|pending|in_progress|running) ;;
      failed|cancelled|canceled|cancelled-by-user) fail "Deployment $deployment_uuid ended with status '$status'." ;;
      '') fail "Deployment $deployment_uuid returned no status." ;;
      *) fail "Deployment $deployment_uuid returned unknown status '$status'." ;;
    esac
    now=$(date +%s)
    [ $((now - started_at)) -lt "$timeout_seconds" ] || fail "Deployment $deployment_uuid timed out."
    sleep "$poll_seconds"
  done
}

[ "$#" -ge 1 ] || usage
operation=$1; shift
case "$operation" in
  inspect-compose)
    [ "$#" -eq 1 ] || usage
    uuid=$1; validate_uuid "$uuid"; get_application "$uuid"; get_compose_images "$uuid"
    write_output control_reference "$control_reference"
    write_output worker_reference "$worker_reference"
    printf '%s\n%s\n' "$control_reference" "$worker_reference"
    ;;
  deploy-compose)
    [ "$#" -eq 5 ] || usage
    uuid=$1; control_image=$2; control_digest=$3; worker_image=$4; worker_digest=$5
    validate_uuid "$uuid"; validate_image "$control_image"; validate_image "$worker_image"
    desired_control=$(immutable_reference "$control_image" "$control_digest")
    desired_worker=$(immutable_reference "$worker_image" "$worker_digest")
    get_application "$uuid"; get_compose_images "$uuid"
    previous_control=$control_reference; previous_worker=$worker_reference
    update_image_variables "$uuid" "$desired_control" "$desired_worker"
    trigger_and_wait "$uuid"
    get_compose_images "$uuid"
    [ "$control_reference" = "$desired_control" ] || fail "Coolify saved '$control_reference' instead of '$desired_control'."
    [ "$worker_reference" = "$desired_worker" ] || fail "Coolify saved '$worker_reference' instead of '$desired_worker'."
    write_output previous_control_reference "$previous_control"
    write_output previous_worker_reference "$previous_worker"
    write_output control_reference "$desired_control"
    write_output worker_reference "$desired_worker"
    write_output deployment_uuid "$deployment_uuid"
    summary "- Deployed backend Compose resource \`$uuid\` as deployment \`$deployment_uuid\`."
    ;;
  *) usage ;;
esac
