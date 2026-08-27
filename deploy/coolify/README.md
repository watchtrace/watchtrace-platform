# Private Coolify deployment

This directory defines the owner-only Phase 1–3 deployment. It is disposable,
admits no external clients, and makes no backup, restore, availability, RPO, or
RTO promise. Internet-facing security controls still apply. P4-701 must be
completed before any customer, production, or public-beta use.

## What this stack runs

- PostgreSQL 18 with a named volume;
- a one-shot database migration;
- the API, monitoring engine, hosted database-free worker, and notification
  worker from the control and worker images;
- the React/Nginx frontend as the only public Coolify service.

The Nginx container forwards `/api/v1/*` to the private `api:8080` service. It
disables proxy buffering for the same-origin event stream. Do not assign a
Coolify domain or public port to PostgreSQL, the API, or either worker.

The named PostgreSQL and worker-journal volumes survive normal container
replacement. They are not backups and may be discarded during Phases 1–3.

## Before creating the Coolify resource

1. Publish ARM64-compatible control, worker, and frontend images. Use immutable
   release tags or `image@sha256:...` values for an actual deployment. The
   `:main` examples are only convenient while the publishing pipeline is being
   introduced. GHCR packages are private by default. On the Oracle host, log in
   to `ghcr.io` as the same operating-system user Coolify uses, using a dedicated
   classic GitHub token with only `read:packages`; never put a package-write
   token on the server. Confirm all three candidate digest references can be
   pulled before creating the stack.

   ```sh
   printf '%s' "$WATCHTRACE_GHCR_READ_TOKEN" | \
     docker login ghcr.io \
       --username "$WATCHTRACE_GHCR_USER" \
       --password-stdin
   unset WATCHTRACE_GHCR_READ_TOKEN
   docker pull ghcr.io/watchtrace/watchtrace-platform@sha256:<control-digest>
   docker pull ghcr.io/watchtrace/watchtrace-worker@sha256:<worker-digest>
   docker pull ghcr.io/watchtrace/watchtrace-console@sha256:<frontend-digest>
   ```
2. Create and verify the four FIFO queues with
   `scripts/provision-phase1-sqs.sh` by following `docs/AWS_SQS_RUNBOOK.md`.
   Review and commit the generated non-secret environment manifest containing
   the real Region, URLs, ARNs, attributes, tags, redrive policies, and policy
   fingerprints.
3. Create an OCI Email Delivery approved sender and SMTP credential. Never use
   the OCI account password as the SMTP password.
4. Point the public DNS record at the Oracle VM and allow inbound TCP 80 and 443
   in both the OCI security list/NSG and the VM firewall. Keep PostgreSQL and
   application ports closed.

## Generate deployment keys

Generate every environment's keys on a trusted machine. The following example
uses a temporary directory so the private material is not written into this
repository:

```sh
key_directory=$(mktemp -d)
chmod 700 "$key_directory"
go run ./cmd/artifact-sign -mode generate \
  -key "$key_directory/platform-signing.key" \
  -public-out "$key_directory/platform-signing.pub"
go run ./cmd/worker-pool -mode generate -pool hosted \
  -bundle "$key_directory/hosted-public.json" \
  -private-prefix "$key_directory/hosted"
openssl rand -base64 32 >"$key_directory/monitor-header.key"
openssl rand -base64 32 >"$key_directory/quarantine.key"
chmod 600 "$key_directory"/*.key
```

Map the values into Coolify variables as follows:

| Coolify variable | Generated value |
| --- | --- |
| `WATCHTRACE_PLATFORM_SIGNING_PRIVATE_KEY` | `platform-signing.key` |
| `WATCHTRACE_PLATFORM_SIGNING_PUBLIC_KEY` | `platform-signing.pub` |
| `WATCHTRACE_MONITOR_HEADER_KEY` | `monitor-header.key` |
| `WATCHTRACE_QUARANTINE_KEY` | `quarantine.key` |
| `WATCHTRACE_WORKER_ENCRYPTION_PRIVATE_KEY` | `hosted-encryption.key` |
| `WATCHTRACE_WORKER_RESULT_PRIVATE_KEY` | `hosted-result.key` |

The public keys in `hosted-public.json` belong in the hosted worker-pool
registration. Keep the private files in an approved secret store after copying
their values to Coolify; do not commit them, paste them into issue trackers, or
place them in the AWS manifest.

## Create the stack in Coolify

1. In Coolify, create a Docker Compose resource from this repository and choose
   `deploy/coolify/compose.yml` as the Compose file.
2. Copy every name from `.env.example` into the resource's environment-variable
   editor and replace every placeholder. Mark credentials and private keys as
   secrets. `POSTGRES_PASSWORD` must be URL-safe and must exactly match the
   password embedded in `WATCHTRACE_DATABASE_URL`.
3. Set `WATCHTRACE_PUBLIC_URL` to the final HTTPS origin without a trailing
   slash. Set the OCI SMTP endpoint for the VM's OCI Region.
4. Assign the public domain only to the `frontend` service on container port
   `8080`. Let Coolify provision and renew HTTPS.
5. Deploy. The migration must finish successfully before the API and dependent
   services start. Do not work around an unhealthy service by removing its
   health check.

Coolify stores the resource variables; this repository stores names and
placeholders only. For this private Phase 1 deployment, the AWS access key must
belong to the queue-scoped non-production runtime identity described in the SQS
runbook. Never store the provisioning operator's credentials in Coolify.
Separate temporary workload roles remain a Phase 4 requirement.

## First-deployment checks

Run these checks before creating personal test data:

```sh
./scripts/verify-private-deployment.sh https://watchtrace.example.com
```

Then confirm in Coolify that `postgres`, `api`, `monitor-engine`,
`hosted-worker`, `notification-worker`, and `frontend` are healthy and that
`migrate` exited successfully. Exercise registration, sign-in, email action,
one hosted monitor, SSE refresh, and a controlled container restart. Verify that
the PostgreSQL and worker-journal named volumes remain attached.

The deployment is not considered P1-703 complete merely because the containers
start. The private deployment gate also requires its ARM64, security, capacity,
operational visibility, network-isolation, drift, and rollback checks.

## Controlled deployment and rollback exercise

The GHCR workflows publish `main` plus an immutable `sha-<full commit>` tag and
record each image digest in the GitHub Actions summary. Keep the last known-good
control, worker, and frontend digest references before every deployment.

1. Replace all three image variables with the candidate
   `ghcr.io/watchtrace/<image>@sha256:<digest>` references, then deploy in
   Coolify. Never mix candidate and previous release digests.
2. Require `migrate` to exit successfully and every long-running service to be
   healthy. Run `scripts/verify-private-deployment.sh`, then exercise sign-in,
   one hosted monitor, SSE refresh, email, and a controlled service restart.
3. For the rollback test, replace all three image variables with the recorded
   last known-good digests and deploy again. Re-run the same checks and confirm
   the PostgreSQL and worker-journal volumes remain attached.
4. To finish the exercise on the candidate release, restore the three candidate
   digest variables, deploy once more, and repeat the checks. Record candidate
   and previous digests, timestamps, migration outcome, health result, and the
   operator in the private deployment log.

A backward-incompatible database migration would make an image-only rollback
unsafe. Phase 1 migrations must therefore remain backward compatible; stop the
exercise and rebuild the disposable private environment if that condition is
not met. Do not mark the deployment/rollback gate until both directions have
actually succeeded on the Oracle ARM64 instance.
