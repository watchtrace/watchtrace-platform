# Phase 1 SQS provisioning and verification

Phase 1.2 uses a reviewed manual AWS procedure and a versioned, non-secret deployment manifest to validate FIFO queue behavior with one operator-provided non-production IAM identity. Separate workload-role creation, trust-policy verification, production security exercises, Terraform, numeric CloudWatch alarms, SNS topics, and infrastructure email alerts are deferred to Phase 4.

The owner-only Oracle deployment has one deployment environment, named `prod`.
That label determines resource names and configuration selection; it does not
mean the Phase 1 system has passed the Phase 4 customer-production gates. The
provisioning script rejects `dev` and `stg` so that a second queue set cannot be
created accidentally.

Use one explicit Region for the `prod` deployment (`ap-south-1` is the reference Region). Never place access keys, session tokens, receipt handles, private keys, certificate private material, or decrypted monitor data in the manifest.

## Create or reconcile the private Phase 1 queues

Authenticate the AWS CLI with the explicitly controlled non-production identity,
confirm the account before changing anything, and preview the names and output
path:

```sh
aws sts get-caller-identity
./scripts/provision-phase1-sqs.sh --environment prod --region ap-south-1
```

Apply the reviewed plan. This creates DLQs before source queues, rolls back only
queues newly created by a failed run, safely reconciles an existing queue set,
applies the documented tags and TLS-only queue policy, and writes the non-secret
environment manifest:

```sh
./scripts/provision-phase1-sqs.sh \
  --environment prod \
  --region ap-south-1 \
  --manifest deploy/aws/phase1-sqs.prod.manifest.json \
  --runtime-policy deploy/aws/phase1-sqs.prod.runtime-policy.json \
  --apply
go run ./cmd/queue-admin \
  -scope sqs \
  deploy/aws/phase1-sqs.prod.manifest.json
```

Review and commit the generated non-secret manifest and runtime IAM policy after
verification. Attach the generated queue-scoped policy to a dedicated
non-production runtime identity; put only that identity's access key in Coolify.
Do not put the provisioning operator's credentials in Coolify. Copy the
manifest's four queue URLs into the matching Coolify variables. Do not set
`WATCHTRACE_SQS_ENDPOINT` for AWS; endpoint overrides are accepted only by the
isolated LocalStack test when `WATCHTRACE_ALLOW_LOCALSTACK=1` is also set.

## Local validation

Docker Compose starts LocalStack on `http://127.0.0.1:4566`. Set `AWS_ACCESS_KEY_ID=test`, `AWS_SECRET_ACCESS_KEY=test`, `AWS_REGION=ap-south-1`, and `WATCHTRACE_SQS_ENDPOINT=http://host.docker.internal:4566` only for LocalStack. These values are not production credentials.

Create the two DLQs first and then the source queues. Every queue is FIFO, uses explicit deduplication, and enables SQS-managed server-side encryption. Configure:

SQS message bodies are UTF-8 text. WatchTrace therefore base64-encodes the sealed binary job envelope and signed binary result envelope at the SQS adapter boundary. The durable outbox stores the exact base64 job body that is retried; workers, result consumers, and DLQ reconciliation decode it before cryptographic validation. Message attributes remain bounded, non-secret routing metadata.

| Queue | Retention | Long poll | Visibility | Redrive |
| --- | ---: | ---: | ---: | ---: |
| `watchtrace-{environment}-check-jobs-hosted.fifo` | 4 days | 20 seconds | 90 seconds | job DLQ after 5 receives |
| `watchtrace-{environment}-check-results.fifo` | 4 days | 20 seconds | 120 seconds | result DLQ after 10 receives |
| `watchtrace-{environment}-check-jobs-hosted-dlq.fifo` | 14 days | 0 | 120 seconds | none |
| `watchtrace-{environment}-check-results-dlq.fifo` | 14 days | 0 | 120 seconds | none |

Restrict each DLQ's `RedriveAllowPolicy` to its one source queue. For Phase 1.2, record the returned URL and ARN, exact attributes, redrive policy, redrive-allow sources, tags, and queue-policy fingerprint. The controlled AWS FIFO/DLQ integration test verifies those behaviors with the configured non-production profile. The manifest's role/trust/policy fields and the IAM verifier are retained for the Phase 4 production-role rollout; they are not a P1-306 completion requirement.

During Phase 4 production-role rollout, run the full queue-and-IAM verifier with the AWS SDK default credential chain:

```text
go run ./cmd/queue-admin path/to/environment-manifest.json
```

For LocalStack only, additionally set `WATCHTRACE_SQS_ENDPOINT`. Controlled Phase 1.2 Amazon SQS validation uses one non-production profile through the default chain. Production must not set an endpoint override and, beginning in Phase 4, uses separate temporary workload-role credentials.

## Phase 4 least-privilege roles

- job publisher: `SendMessage` and the minimum queue-attribute read on assigned job queues;
- hosted worker: receive/change visibility/delete on its job queue and send on the result queue;
- queue gateway: the equivalent queue-scoped operations for mapped pools only, with no database credentials or network route;
- result consumer: receive/change visibility/delete and attributes on the result queue;
- DLQ reconciler: receive/change visibility/delete and attributes on the two DLQs only;
- infrastructure operator: reviewed queue, policy, tags, encryption, and controlled redrive administration.

Customer-VPC workers never receive an AWS role or direct SQS credentials. They use the outbound HTTPS/mTLS gateway.

## Activation and recovery

Do not activate a worker pool until the manifest verifier, PostgreSQL pool reconciliation, signed gateway mapping, public-key history, and certificate checks all pass. A partial operation stays `provisioning` or moves to `failed`; it must never be silently enabled.

Deletion requires `draining`, empty source/DLQ queues, no non-terminal ledger jobs, expired/revoked credentials, removal from the signed gateway mapping, a recorded reason and approver, and exact interactive confirmation. DLQ messages are inspected and redriven only through the bounded recovery command; never bulk-redrive blindly in the AWS console.
