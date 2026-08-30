#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
container_name="watchtrace-sqs-provision-$$"
temporary_directory=$(mktemp -d)
manifest_path="$temporary_directory/manifest.json"
runtime_policy_path="$temporary_directory/runtime-policy.json"

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    docker rm -f "$container_name" >/dev/null 2>&1 || true
    rm -r "$temporary_directory"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

for command_name in aws curl docker go jq; do
    command -v "$command_name" >/dev/null 2>&1 || {
        echo "$command_name is required" >&2
        exit 1
    }
done

if "$repository_root/scripts/provision-phase1-sqs.sh" \
    --manifest "$manifest_path" \
    --runtime-policy "$manifest_path" >/dev/null 2>&1; then
    echo "The provisioner must reject overlapping output paths" >&2
    exit 1
fi

if "$repository_root/scripts/provision-phase1-sqs.sh" \
    --environment dev >/dev/null 2>&1; then
    echo "The provisioner must reject an environment other than prod" >&2
    exit 1
fi

docker run --detach --name "$container_name" \
    --env SERVICES=sqs \
    --env DEFAULT_REGION=ap-south-1 \
    --publish 127.0.0.1::4566 \
    localstack/localstack:4.8.1 >/dev/null

published_address=$(docker port "$container_name" 4566/tcp)
endpoint="http://$published_address"
attempt=0
until curl --fail --silent "$endpoint/_localstack/health" | jq -e '.services.sqs == "available" or .services.sqs == "running"' >/dev/null; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 60 ]; then
        echo "LocalStack SQS did not become ready" >&2
        docker logs "$container_name" >&2
        exit 1
    fi
    sleep 1
done

run_with_localstack() {
    AWS_ACCESS_KEY_ID=test \
    AWS_SECRET_ACCESS_KEY=test \
    AWS_REGION=ap-south-1 \
    WATCHTRACE_ALLOW_LOCALSTACK=1 \
    WATCHTRACE_SQS_ENDPOINT="$endpoint" \
        "$@"
}

verify_manifest() {
    if run_with_localstack go run ./cmd/queue-admin -scope sqs "$manifest_path"; then
        return 0
    fi
    echo "SQS manifest verification failed; actual LocalStack policies follow." >&2
    jq -r '.queues[].url' "$manifest_path" | while IFS= read -r queue_url; do
        run_with_localstack aws --endpoint-url "$endpoint" sqs get-queue-attributes \
            --queue-url "$queue_url" --attribute-names Policy --output json >&2
    done
    return 1
}

cd "$repository_root"
run_with_localstack ./scripts/provision-phase1-sqs.sh \
    --environment prod \
    --region ap-south-1 \
    --manifest "$manifest_path" \
    --runtime-policy "$runtime_policy_path" \
    --apply
verify_manifest

# A second run proves that the reviewed manual operation is safely reconcilable.
run_with_localstack ./scripts/provision-phase1-sqs.sh \
    --environment prod \
    --region ap-south-1 \
    --manifest "$manifest_path" \
    --runtime-policy "$runtime_policy_path" \
    --apply
verify_manifest

test "$(run_with_localstack aws --endpoint-url "$endpoint" sqs list-queues --query 'length(QueueUrls)' --output text)" = "4"
jq -e '
  .version == 1 and
  .environment == "prod" and
  .aws_region == "ap-south-1" and
  (.queues | length) == 4 and
  ([.queues[].sse] | all(. == "SSE-SQS"))
' "$manifest_path" >/dev/null
jq -e '
  .Version == "2012-10-17" and
  ([.Statement[].Action] | tostring | contains("sqs:SendMessage")) and
  ([.Statement[].Action] | tostring | contains("sqs:ReceiveMessage")) and
  ([.Statement[].Action] | tostring | contains("sqs:DeleteMessage")) and
  ([.Statement[].Action] | tostring | contains("sqs:ChangeMessageVisibility")) and
  ([.Statement[].Action] | tostring | contains("sqs:GetQueueAttributes")) and
  ([.Statement[] | select(.Sid == "ReconcileDeadLetterQueues")] | length) == 1 and
  (.Statement[] | select(.Sid == "ReconcileDeadLetterQueues") |
    (.Action | index("sqs:ReceiveMessage")) != null and
    (.Action | index("sqs:ChangeMessageVisibility")) != null and
    (.Action | index("sqs:DeleteMessage")) != null and
    (.Resource | length) == 2 and
    ([.Resource[] | endswith("-dlq.fifo")] | all)) and
  (tostring | contains("sqs:*") | not)
' "$runtime_policy_path" >/dev/null

echo "Phase 1 SQS provision/reconcile verification passed"
