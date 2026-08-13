# Phase 1 SQS provisioning and verification

Phase 1 uses a reviewed manual AWS procedure and a versioned, non-secret deployment manifest. Terraform, numeric CloudWatch alarms, SNS topics, and infrastructure email alerts are deliberately deferred to Phase 4.

Use one explicit Region for an environment (`ap-south-1` is the reference Region). Never place access keys, session tokens, receipt handles, private keys, certificate private material, or decrypted monitor data in the manifest.

## Local validation

Docker Compose starts LocalStack on `http://127.0.0.1:4566`. Set `AWS_ACCESS_KEY_ID=test`, `AWS_SECRET_ACCESS_KEY=test`, `AWS_REGION=ap-south-1`, and `WATCHTRACE_SQS_ENDPOINT=http://host.docker.internal:4566` only for LocalStack. These values are not production credentials.

Create the two DLQs first and then the source queues. Every queue is FIFO, uses explicit deduplication, and enables SQS-managed server-side encryption. Configure:

| Queue | Retention | Long poll | Visibility | Redrive |
| --- | ---: | ---: | ---: | ---: |
| `watchtrace-{environment}-jobs.fifo` | 4 days | 20 seconds | 90 seconds | job DLQ after 5 receives |
| `watchtrace-{environment}-results.fifo` | 4 days | 20 seconds | 60 seconds | result DLQ after 10 receives |
| `watchtrace-{environment}-jobs-dlq.fifo` | 14 days | 0 | 0 | none |
| `watchtrace-{environment}-results-dlq.fifo` | 14 days | 0 | 0 | none |

Record the returned URL and ARN, the exact attributes, redrive policy, tags, and role/trust/policy fingerprints in a copy of `deploy/aws/phase1-sqs.manifest.example.json`. Replace every placeholder fingerprint with the SHA-256 of normalized reviewed JSON. The verifier intentionally rejects missing inventory.

Run the verifier with the AWS SDK default credential chain:

```text
go run ./cmd/queue-admin path/to/environment-manifest.json
```

For LocalStack only, additionally set `WATCHTRACE_SQS_ENDPOINT`. Production must not set that endpoint and should supply temporary workload-role credentials through the default chain.

## Least-privilege roles

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
