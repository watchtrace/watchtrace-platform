#!/bin/sh

set -eu

usage() {
    echo "Usage: $0 ghcr.io/watchtrace/watchtrace-platform@sha256:<64-hex-digest>" >&2
    exit 2
}

fail() {
    echo "ERROR: $*" >&2
    exit 1
}

[ "$#" -eq 1 ] || usage
[ "$(id -u)" -eq 0 ] || fail "run this bootstrap command as root (or through sudo) on the Oracle host"

control_image=$1
deployment_root=/data/watchtrace
keyset_directory=$deployment_root/keyset

printf '%s\n' "$control_image" | grep -Eq \
    '^ghcr\.io/watchtrace/watchtrace-platform@sha256:[0-9a-f]{64}$' || \
    fail "the control image must be the immutable WatchTrace GHCR digest reference"

for required_command in docker install mktemp mv chown chmod grep find sort; do
    command -v "$required_command" >/dev/null 2>&1 || \
        fail "$required_command is required"
done

verify_installed_keyset() {
    [ -d "$keyset_directory/secrets" ] || \
        fail "$keyset_directory/secrets is missing"
    [ -f "$keyset_directory/public/hosted-public.json" ] || \
        fail "$keyset_directory/public/hosted-public.json is missing"

    docker run --rm \
        --user 65532:65532 \
        --network none \
        --read-only \
        --cap-drop ALL \
        --security-opt no-new-privileges \
        --mount "type=bind,source=$keyset_directory/secrets/platform-signing-private,target=/keys/platform-signing-private,readonly" \
        --mount "type=bind,source=$keyset_directory/secrets/platform-signing-public,target=/keys/platform-signing-public,readonly" \
        --mount "type=bind,source=$keyset_directory/secrets/monitor-header-key,target=/keys/monitor-header-key,readonly" \
        --mount "type=bind,source=$keyset_directory/secrets/quarantine-key,target=/keys/quarantine-key,readonly" \
        --mount "type=bind,source=$keyset_directory/secrets/worker-encryption-private,target=/keys/worker-encryption-private,readonly" \
        --mount "type=bind,source=$keyset_directory/secrets/worker-result-private,target=/keys/worker-result-private,readonly" \
        --mount "type=bind,source=$keyset_directory/public/hosted-public.json,target=/keys/hosted-public.json,readonly" \
        "$control_image" \
        /watchtrace-deployment-keys \
        -mode verify \
        -directory /keys
}

echo "Pulling the reviewed control image by immutable digest..."
docker pull "$control_image"

if [ -e "$keyset_directory" ] || [ -L "$keyset_directory" ]; then
    [ -d "$keyset_directory" ] || fail "$keyset_directory exists but is not a directory"
    echo "An installed keyset already exists. It will be verified, not replaced."
    verify_installed_keyset
    echo "The existing WatchTrace deployment keyset is valid. No files were changed."
    exit 0
fi

install -d -o root -g root -m 0700 "$deployment_root"
staging_directory=$(mktemp -d "$deployment_root/keyset.new.XXXXXX")

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ -n "${staging_directory:-}" ] && [ -d "$staging_directory" ]; then
        case "$staging_directory" in
            "$deployment_root"/keyset.new.*) rm -r -- "$staging_directory" ;;
            *) echo "Refusing to remove unexpected staging path: $staging_directory" >&2 ;;
        esac
    fi
    exit "$status"
}
trap cleanup EXIT HUP INT TERM

generated_directory=$staging_directory/generated
install -d -o 65532 -g 65532 -m 0700 "$generated_directory"

echo "Generating a new keyset inside a network-disabled container..."
docker run --rm \
    --user 65532:65532 \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --mount "type=bind,source=$generated_directory,target=/output" \
    "$control_image" \
    /watchtrace-deployment-keys \
    -mode generate \
    -directory /output

echo "Cryptographically verifying the generated keyset before activation..."
docker run --rm \
    --user 65532:65532 \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --mount "type=bind,source=$generated_directory,target=/keys,readonly" \
    "$control_image" \
    /watchtrace-deployment-keys \
    -mode verify \
    -directory /keys

private_directory=$staging_directory/secrets
public_directory=$staging_directory/public
mv "$generated_directory" "$private_directory"
install -d -o root -g root -m 0755 "$public_directory"
install -o root -g root -m 0444 \
    "$private_directory/hosted-public.json" \
    "$public_directory/hosted-public.json"
rm -f -- "$private_directory/hosted-public.json"

for runtime_key_file in \
    platform-signing-private \
    platform-signing-public \
    monitor-header-key \
    quarantine-key \
    worker-encryption-private \
    worker-result-private
do
    [ -s "$private_directory/$runtime_key_file" ] || \
        fail "generated file $runtime_key_file is missing or empty"
    chown 65532:65532 "$private_directory/$runtime_key_file"
    chmod 0400 "$private_directory/$runtime_key_file"
done

chown root:root "$private_directory" "$staging_directory"
chmod 0700 "$private_directory" "$staging_directory"

# Both the runtime key files and public bundle become visible together because
# this rename stays within the /data/watchtrace filesystem.
mv -T "$staging_directory" "$keyset_directory"
staging_directory=

echo "Verifying the installed files exactly as deployment will mount them..."
verify_installed_keyset

trap - EXIT HUP INT TERM

echo "Installed metadata (contents are deliberately not printed):"
find "$keyset_directory" -type f \
    -printf '%P  owner=%u:%g  mode=%m  bytes=%s\n' | sort
echo "WatchTrace deployment keys were generated, activated, and verified successfully."
