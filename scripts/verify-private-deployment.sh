#!/bin/sh

set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 https://watchtrace.example.com" >&2
    exit 2
fi

public_url=${1%/}
case "$public_url" in
    https://*) ;;
    *) echo "the public deployment URL must use HTTPS" >&2; exit 2 ;;
esac

for command_name in curl jq openssl; do
    command -v "$command_name" >/dev/null 2>&1 || {
        echo "$command_name is required" >&2
        exit 1
    }
done

authority=${public_url#https://}
authority=${authority%%/*}
case "$authority" in
    *:*|*@*|"") echo "use an HTTPS hostname without credentials or a custom port" >&2; exit 2 ;;
esac

temporary_directory=$(mktemp -d)
trap 'rm -r "$temporary_directory"' EXIT HUP INT TERM

curl_https() {
    curl --fail --silent --show-error --location \
        --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 30 "$@"
}

curl_https --dump-header "$temporary_directory/health.headers" \
    --output "$temporary_directory/health.body" "$public_url/health"
test "$(tr -d '\r\n' <"$temporary_directory/health.body")" = "ok"

curl_https --output "$temporary_directory/index.html" "$public_url/"
grep -F '<div id="root"></div>' "$temporary_directory/index.html" >/dev/null
grep -F '<title>WatchTrace</title>' "$temporary_directory/index.html" >/dev/null

auth_status=$(curl --silent --show-error \
    --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 30 \
    --dump-header "$temporary_directory/auth.headers" \
    --output "$temporary_directory/auth.body" \
    --write-out '%{http_code}' "$public_url/api/v1/auth/me")
test "$auth_status" = "401"
jq -e '.error.code == "invalid_session"' "$temporary_directory/auth.body" >/dev/null
grep -i '^www-authenticate:[[:space:]]*Bearer' "$temporary_directory/auth.headers" >/dev/null
grep -i '^cache-control:[[:space:]]*no-store' "$temporary_directory/auth.headers" >/dev/null

redirect_headers="$temporary_directory/redirect.headers"
redirect_status=$(curl --silent --show-error --connect-timeout 10 --max-time 30 \
    --dump-header "$redirect_headers" --output /dev/null --write-out '%{http_code}' \
    "http://$authority/health")
case "$redirect_status" in
    301|302|307|308) ;;
    *) echo "HTTP did not redirect to HTTPS (status $redirect_status)" >&2; exit 1 ;;
esac
grep -i "^location:[[:space:]]*https://$authority" "$redirect_headers" >/dev/null

openssl s_client -connect "$authority:443" -servername "$authority" </dev/null 2>/dev/null |
    openssl x509 -noout -checkend 604800 >/dev/null

echo "Private deployment HTTPS, React, API proxy, and certificate checks passed."
