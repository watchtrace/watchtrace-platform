#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
environment=dev
region=${AWS_REGION:-ap-south-1}
manifest_path=
runtime_policy_path=
apply=false

usage() {
    echo "usage: $0 [--environment dev|stg|prod] [--region REGION] [--manifest PATH] [--runtime-policy PATH] [--apply]" >&2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --environment)
            [ "$#" -ge 2 ] || { usage; exit 2; }
            environment=$2
            shift 2
            ;;
        --region)
            [ "$#" -ge 2 ] || { usage; exit 2; }
            region=$2
            shift 2
            ;;
        --manifest)
            [ "$#" -ge 2 ] || { usage; exit 2; }
            manifest_path=$2
            shift 2
            ;;
        --runtime-policy)
            [ "$#" -ge 2 ] || { usage; exit 2; }
            runtime_policy_path=$2
            shift 2
            ;;
        --apply)
            apply=true
            shift
            ;;
        *)
            usage
            exit 2
            ;;
    esac
done

case "$environment" in
    dev|stg|prod) ;;
    *) echo "environment must be dev, stg, or prod" >&2; exit 2 ;;
esac
[ -n "$region" ] || { echo "AWS Region must not be empty" >&2; exit 2; }

if [ -z "$manifest_path" ]; then
    manifest_path="$repository_root/deploy/aws/phase1-sqs.$environment.manifest.json"
fi
if [ -z "$runtime_policy_path" ]; then
    runtime_policy_path=${manifest_path%.json}.runtime-policy.json
fi
[ "$manifest_path" != "$runtime_policy_path" ] || {
    echo "manifest and runtime policy paths must be different" >&2
    exit 2
}

prefix="watchtrace-$environment"
jobs_name="$prefix-check-jobs-hosted.fifo"
results_name="$prefix-check-results.fifo"
jobs_dlq_name="$prefix-check-jobs-hosted-dlq.fifo"
results_dlq_name="$prefix-check-results-dlq.fifo"

printf '%s\n' "Region: $region" "Environment: $environment" "Queues:"
printf '  %s\n' "$jobs_name" "$results_name" "$jobs_dlq_name" "$results_dlq_name"
printf 'Manifest: %s\n' "$manifest_path"
printf 'Runtime policy: %s\n' "$runtime_policy_path"

if [ "$apply" != true ]; then
    echo "Plan only; rerun with --apply to create or reconcile these queues."
    exit 0
fi

for command_name in aws jq; do
    command -v "$command_name" >/dev/null 2>&1 || {
        echo "$command_name is required" >&2
        exit 1
    }
done

endpoint=${WATCHTRACE_SQS_ENDPOINT:-}
if [ -n "$endpoint" ] && [ "${WATCHTRACE_ALLOW_LOCALSTACK:-}" != "1" ]; then
    echo "WATCHTRACE_SQS_ENDPOINT is forbidden unless WATCHTRACE_ALLOW_LOCALSTACK=1" >&2
    exit 1
fi

aws_cli() {
    if [ -n "$endpoint" ]; then
        aws --endpoint-url "$endpoint" --region "$region" "$@"
    else
        aws --region "$region" "$@"
    fi
}

if [ -z "$endpoint" ]; then
    aws_cli sts get-caller-identity >/dev/null
fi

temporary_directory=$(mktemp -d)
created_urls="$temporary_directory/created-queues"
: >"$created_urls"
completed=false
manifest_temporary=
runtime_policy_temporary=

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ] && [ "$completed" != true ] && [ -s "$created_urls" ]; then
        echo "Provisioning failed; deleting queues created by this run." >&2
        awk '{ lines[NR]=$0 } END { for (line=NR; line>=1; line--) print lines[line] }' "$created_urls" |
            while IFS= read -r queue_url; do
                aws_cli sqs delete-queue --queue-url "$queue_url" >/dev/null 2>&1 || true
            done
    fi
    if [ -n "$manifest_temporary" ] && [ -f "$manifest_temporary" ]; then
        rm -f "$manifest_temporary"
    fi
    if [ -n "$runtime_policy_temporary" ] && [ -f "$runtime_policy_temporary" ]; then
        rm -f "$runtime_policy_temporary"
    fi
    rm -r "$temporary_directory"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

queue_url() {
    aws_cli sqs get-queue-url --queue-name "$1" --query QueueUrl --output text 2>/dev/null || true
}

ensure_queue() {
    queue_name=$1
    attributes_file=$2
    existing_url=$(queue_url "$queue_name")
    if [ -z "$existing_url" ] || [ "$existing_url" = "None" ]; then
        existing_url=$(aws_cli sqs create-queue \
            --queue-name "$queue_name" \
            --attributes "file://$attributes_file" \
            --tags "Application=WatchTrace,Environment=$environment,Phase=1,ManagedBy=watchtrace-phase1-manual" \
            --query QueueUrl --output text)
        printf '%s\n' "$existing_url" >>"$created_urls"
        echo "Created $queue_name" >&2
    else
        mutable_attributes="$temporary_directory/$queue_name.mutable.json"
        jq 'del(.FifoQueue)' "$attributes_file" >"$mutable_attributes"
        aws_cli sqs set-queue-attributes \
            --queue-url "$existing_url" \
            --attributes "file://$mutable_attributes" >/dev/null
        echo "Reconciled $queue_name" >&2
    fi
    aws_cli sqs tag-queue \
        --queue-url "$existing_url" \
        --tags "Application=WatchTrace,Environment=$environment,Phase=1,ManagedBy=watchtrace-phase1-manual" >/dev/null
    printf '%s\n' "$existing_url"
}

queue_arn() {
    aws_cli sqs get-queue-attributes \
        --queue-url "$1" \
        --attribute-names QueueArn \
        --query 'Attributes.QueueArn' --output text
}

set_queue_policy() {
    queue_url_value=$1
    queue_arn_value=$2
    policy_file="$temporary_directory/policy-$(printf '%s' "$queue_arn_value" | tr ':/' '__').json"
    attribute_file="$policy_file.attributes"
    if [ -n "$endpoint" ]; then
        # LocalStack is deliberately HTTP-only in the isolated integration test.
        jq -cn --arg arn "$queue_arn_value" '{
          Version:"2012-10-17",
          Statement:[{
            Sid:"LocalManifestVerification",
            Effect:"Allow",
            Principal:"*",
            Action:"sqs:*",
            Resource:$arn
          }]
        }' >"$policy_file"
    else
        jq -cn --arg arn "$queue_arn_value" '{
          Version:"2012-10-17",
          Statement:[{
            Sid:"DenyInsecureTransport",
            Effect:"Deny",
            Principal:"*",
            Action:"sqs:*",
            Resource:$arn,
            Condition:{Bool:{"aws:SecureTransport":"false"}}
          }]
        }' >"$policy_file"
    fi
    jq -n --rawfile policy "$policy_file" '{Policy:($policy | fromjson | tojson)}' >"$attribute_file"
    aws_cli sqs set-queue-attributes --queue-url "$queue_url_value" \
        --attributes "file://$attribute_file" >/dev/null
}

hash_standard_input() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
    else
        shasum -a 256 | awk '{print $1}'
    fi
}

policy_fingerprint() {
    policy=$(aws_cli sqs get-queue-attributes \
        --queue-url "$1" --attribute-names Policy \
        --query 'Attributes.Policy' --output text)
    [ -n "$policy" ] && [ "$policy" != "None" ] || {
        echo "queue policy was not returned for $1" >&2
        exit 1
    }
    printf '%s' "$policy" | jq -S -c -j . | hash_standard_input
}

jq -n '{
  FifoQueue:"true",
  ContentBasedDeduplication:"false",
  VisibilityTimeout:"0",
  MessageRetentionPeriod:"1209600",
  ReceiveMessageWaitTimeSeconds:"0",
  SqsManagedSseEnabled:"true"
}' >"$temporary_directory/dlq-attributes.json"

jobs_dlq_url=$(ensure_queue "$jobs_dlq_name" "$temporary_directory/dlq-attributes.json")
results_dlq_url=$(ensure_queue "$results_dlq_name" "$temporary_directory/dlq-attributes.json")
jobs_dlq_arn=$(queue_arn "$jobs_dlq_url")
results_dlq_arn=$(queue_arn "$results_dlq_url")

jq -n --arg arn "$jobs_dlq_arn" '{
  FifoQueue:"true",
  ContentBasedDeduplication:"false",
  VisibilityTimeout:"90",
  MessageRetentionPeriod:"345600",
  ReceiveMessageWaitTimeSeconds:"20",
  SqsManagedSseEnabled:"true",
  RedrivePolicy:({deadLetterTargetArn:$arn,maxReceiveCount:"5"}|tojson)
}' >"$temporary_directory/jobs-attributes.json"
jq -n --arg arn "$results_dlq_arn" '{
  FifoQueue:"true",
  ContentBasedDeduplication:"false",
  VisibilityTimeout:"60",
  MessageRetentionPeriod:"345600",
  ReceiveMessageWaitTimeSeconds:"20",
  SqsManagedSseEnabled:"true",
  RedrivePolicy:({deadLetterTargetArn:$arn,maxReceiveCount:"10"}|tojson)
}' >"$temporary_directory/results-attributes.json"

jobs_url=$(ensure_queue "$jobs_name" "$temporary_directory/jobs-attributes.json")
results_url=$(ensure_queue "$results_name" "$temporary_directory/results-attributes.json")
jobs_arn=$(queue_arn "$jobs_url")
results_arn=$(queue_arn "$results_url")

jq -n --arg arn "$jobs_arn" '{
  RedriveAllowPolicy:({redrivePermission:"byQueue",sourceQueueArns:[$arn]}|tojson)
}' >"$temporary_directory/jobs-dlq-allow.json"
jq -n --arg arn "$results_arn" '{
  RedriveAllowPolicy:({redrivePermission:"byQueue",sourceQueueArns:[$arn]}|tojson)
}' >"$temporary_directory/results-dlq-allow.json"
aws_cli sqs set-queue-attributes --queue-url "$jobs_dlq_url" \
    --attributes "file://$temporary_directory/jobs-dlq-allow.json" >/dev/null
aws_cli sqs set-queue-attributes --queue-url "$results_dlq_url" \
    --attributes "file://$temporary_directory/results-dlq-allow.json" >/dev/null

set_queue_policy "$jobs_url" "$jobs_arn"
set_queue_policy "$results_url" "$results_arn"
set_queue_policy "$jobs_dlq_url" "$jobs_dlq_arn"
set_queue_policy "$results_dlq_url" "$results_dlq_arn"

jobs_policy=$(policy_fingerprint "$jobs_url")
results_policy=$(policy_fingerprint "$results_url")
jobs_dlq_policy=$(policy_fingerprint "$jobs_dlq_url")
results_dlq_policy=$(policy_fingerprint "$results_dlq_url")
zero_fingerprint=0000000000000000000000000000000000000000000000000000000000000000

manifest_temporary="$manifest_path.tmp.$$"
mkdir -p "$(dirname -- "$manifest_path")"
jq -n \
    --arg environment "$environment" \
    --arg region "$region" \
    --arg jobs_name "$jobs_name" --arg jobs_url "$jobs_url" --arg jobs_arn "$jobs_arn" --arg jobs_policy "$jobs_policy" \
    --arg results_name "$results_name" --arg results_url "$results_url" --arg results_arn "$results_arn" --arg results_policy "$results_policy" \
    --arg jobs_dlq_name "$jobs_dlq_name" --arg jobs_dlq_url "$jobs_dlq_url" --arg jobs_dlq_arn "$jobs_dlq_arn" --arg jobs_dlq_policy "$jobs_dlq_policy" \
    --arg results_dlq_name "$results_dlq_name" --arg results_dlq_url "$results_dlq_url" --arg results_dlq_arn "$results_dlq_arn" --arg results_dlq_policy "$results_dlq_policy" \
    --arg zero "$zero_fingerprint" \
    '{
      version:1,
      environment:$environment,
      aws_region:$region,
      queues:{
        jobs:{name:$jobs_name,url:$jobs_url,arn:$jobs_arn,visibility_timeout_seconds:90,message_retention_seconds:345600,receive_wait_time_seconds:20,max_receive_count:5,dead_letter_queue_arn:$jobs_dlq_arn,sse:"SSE-SQS",content_based_deduplication:false,policy_fingerprint_sha256:$jobs_policy},
        results:{name:$results_name,url:$results_url,arn:$results_arn,visibility_timeout_seconds:60,message_retention_seconds:345600,receive_wait_time_seconds:20,max_receive_count:10,dead_letter_queue_arn:$results_dlq_arn,sse:"SSE-SQS",content_based_deduplication:false,policy_fingerprint_sha256:$results_policy},
        jobs_dlq:{name:$jobs_dlq_name,url:$jobs_dlq_url,arn:$jobs_dlq_arn,visibility_timeout_seconds:0,message_retention_seconds:1209600,receive_wait_time_seconds:0,sse:"SSE-SQS",content_based_deduplication:false,policy_fingerprint_sha256:$jobs_dlq_policy,redrive_allow_source_arns:[$jobs_arn]},
        results_dlq:{name:$results_dlq_name,url:$results_dlq_url,arn:$results_dlq_arn,visibility_timeout_seconds:0,message_retention_seconds:1209600,receive_wait_time_seconds:0,sse:"SSE-SQS",content_based_deduplication:false,policy_fingerprint_sha256:$results_dlq_policy,redrive_allow_source_arns:[$results_arn]}
      },
      roles:{
        job_publisher:{name:"phase4-deferred-job-publisher",policy_fingerprint_sha256:$zero,trust_fingerprint_sha256:$zero},
        hosted_worker:{name:"phase4-deferred-hosted-worker",policy_fingerprint_sha256:$zero,trust_fingerprint_sha256:$zero},
        queue_gateway:{name:"phase4-deferred-queue-gateway",policy_fingerprint_sha256:$zero,trust_fingerprint_sha256:$zero},
        result_consumer:{name:"phase4-deferred-result-consumer",policy_fingerprint_sha256:$zero,trust_fingerprint_sha256:$zero},
        dlq_reconciler:{name:"phase4-deferred-dlq-reconciler",policy_fingerprint_sha256:$zero,trust_fingerprint_sha256:$zero},
        infrastructure_operator:{name:"phase4-deferred-infrastructure-operator",policy_fingerprint_sha256:$zero,trust_fingerprint_sha256:$zero}
      },
      gateway_mappings:{},
      tags:{Application:"WatchTrace",Environment:$environment,Phase:"1",ManagedBy:"watchtrace-phase1-manual"}
    }' >"$manifest_temporary"
chmod 0644 "$manifest_temporary"
mv "$manifest_temporary" "$manifest_path"

runtime_policy_temporary="$runtime_policy_path.tmp.$$"
mkdir -p "$(dirname -- "$runtime_policy_path")"
jq -n \
    --arg jobs "$jobs_arn" \
    --arg results "$results_arn" \
    --arg jobs_dlq "$jobs_dlq_arn" \
    --arg results_dlq "$results_dlq_arn" \
    '{
      Version:"2012-10-17",
      Statement:[
        {
          Sid:"ReadQueueHealth",
          Effect:"Allow",
          Action:["sqs:GetQueueAttributes","sqs:GetQueueUrl"],
          Resource:[$jobs,$results,$jobs_dlq,$results_dlq]
        },
        {
          Sid:"PublishHostedJobs",
          Effect:"Allow",
          Action:"sqs:SendMessage",
          Resource:$jobs
        },
        {
          Sid:"ExecuteHostedJobs",
          Effect:"Allow",
          Action:["sqs:ReceiveMessage","sqs:ChangeMessageVisibility","sqs:DeleteMessage"],
          Resource:$jobs
        },
        {
          Sid:"PublishAndConsumeResults",
          Effect:"Allow",
          Action:["sqs:SendMessage","sqs:ReceiveMessage","sqs:ChangeMessageVisibility","sqs:DeleteMessage"],
          Resource:$results
        }
      ]
    }' >"$runtime_policy_temporary"
chmod 0644 "$runtime_policy_temporary"
mv "$runtime_policy_temporary" "$runtime_policy_path"

completed=true
echo "Provisioned and recorded the controlled Phase 1 SQS queues."
echo "Verify with: go run ./cmd/queue-admin -scope sqs $manifest_path"
echo "Attach the queue-scoped runtime policy to the non-production identity used by Coolify: $runtime_policy_path"
echo "Copy the four queue URLs from the manifest into the matching Coolify variables."
