# Private `prod` Coolify deployment

This directory defines the owner-only Phase 1–3 deployment. It is disposable,
admits no external clients, and makes no backup, restore, availability, RPO, or
RTO promise. Internet-facing security controls still apply. P4-701 must be
completed before any customer, production, or public-beta use.

There is one deployment environment and its resource label is `prod`. This is
the configuration selected by the application and used in AWS queue names. The
label does not override the private-use restriction above or claim that the
Phase 4 customer-production gates have passed.

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

1. Publish ARM64-compatible control, worker, and frontend images. The automated
   deployment follows the movable `:main` tags, while every workflow also
   records an immutable `image@sha256:...` reference for audit and rollback.
   Use an immutable control-image digest for the one-time key-generation
   command below. GHCR packages are private by default. On the Oracle host, log in
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
4. Until an owned domain is available, use
   `watchtrace-129-159-236-232.sslip.io`; sslip.io resolves that hostname to the
   Oracle VM at `129.159.236.232` without a DNS account. Allow inbound TCP 80
   and 443 in both the OCI security list/NSG and the VM firewall. Keep
   PostgreSQL and application ports closed. Replace the temporary hostname with
   an owned domain before external-client use.

## Generate deployment keys on Oracle

Generate the `prod` keys once, directly on Oracle, with
`scripts/bootstrap-deployment-keys.sh`. The script runs the generator from the
reviewed control image without network access. No private key passes through a
developer computer, GitHub, GHCR metadata, Coolify variables, or terminal
output.

The script stages and verifies the complete set before one atomic rename makes
it active under `/data/watchtrace/keyset`. If an active keyset already exists,
the same command verifies it and changes nothing; it never rotates or
overwrites keys.

### 1. Publish and select the control image

Push this change and wait for `Backend CI` to succeed. In the workflow summary,
copy the control image reference that looks like:

```text
ghcr.io/watchtrace/watchtrace-platform@sha256:<64 hexadecimal characters>
```

The digest is an immutable fingerprint. The ordinary `:main` tag moves after
new builds, while a digest always identifies the exact reviewed generator.

### 2. Remove the failed empty-file attempt

The previous Coolify configuration created empty files in
`/data/watchtrace/secrets`. They are not valid keys and the new Compose file no
longer uses that path. Preserve them temporarily for diagnosis instead of
deleting them:

```sh
ssh root@129.159.236.232
test ! -e /data/watchtrace/secrets.failed-empty-files || {
  echo "STOP: the diagnostic destination already exists"
  exit 1
}
mv -T /data/watchtrace/secrets /data/watchtrace/secrets.failed-empty-files
exit
```

If the destination already exists, stop and inspect it rather than choosing a
different destructive command.

### 3. Run the one-time bootstrap from the repository checkout

On the developer computer, from the `watchtrace-platform` repository, replace
the example with the complete digest reference and run:

```sh
ssh root@129.159.236.232 \
  'sh -s -- ghcr.io/watchtrace/watchtrace-platform@sha256:<control-digest>' \
  < scripts/bootstrap-deployment-keys.sh
```

SSH encrypts the script while sending it to the server. `sh -s --` tells the
server shell to read that script from standard input and supplies the image as
its first argument. The script itself then:

1. validates and pulls the immutable image;
2. creates a root-only staging directory;
3. generates keys as unprivileged UID `65532` with networking disabled;
4. cryptographically verifies all key relationships and the public bundle;
5. sets restrictive ownership and permissions;
6. atomically renames the complete staging tree to
   `/data/watchtrace/keyset`; and
7. verifies the installed files through the same read-only mounts deployment
   will use.

The six runtime key files end with owner `65532:65532` and mode `400`. The public worker
registration bundle is
`/data/watchtrace/keyset/public/hosted-public.json`. Never use `cat` on a
private file in logs, screenshots, issues, or chat.

### 4. Remove obsolete Coolify key values

Delete these old variables from the Coolify resource:

- `WATCHTRACE_PLATFORM_SIGNING_PRIVATE_KEY`
- `WATCHTRACE_PLATFORM_SIGNING_PUBLIC_KEY`
- `WATCHTRACE_MONITOR_HEADER_KEY`
- `WATCHTRACE_QUARANTINE_KEY`
- `WATCHTRACE_WORKER_ENCRYPTION_PRIVATE_KEY`
- `WATCHTRACE_WORKER_RESULT_PRIVATE_KEY`

They are raw key-value configuration from the earlier approach. Keeping them
would give the application two competing sources: an environment value and a
mounted file. Keep the key IDs and version variables because those are labels,
not secret contents.

### 5. Let every deployment verify, never generate

The Compose service `deployment-key-check` mounts all seven installed files
read-only, has no network, and runs only the verifier. Migrations, the API,
monitor engine, and hosted worker wait for it to complete successfully. A
missing, empty, corrupt, mismatched, or unreadable key therefore stops the
deployment before those services start. Every bind mount also sets
`create_host_path: false`, so Docker reports a missing file instead of silently
creating another empty directory at that path.

Generation is intentionally not part of continuous deployment. Generating on
every push would silently rotate the application's identity and could make
stored encrypted data or in-flight signed messages unusable. Continuous
deployment changes application containers; this stable server keyset remains
until an explicit, separately designed rotation procedure is implemented.

## Create the stack in Coolify

1. In Coolify, create the `WatchTrace` project with one environment named
   `prod`. In that environment, create a Docker Compose resource from this
   repository and choose `deploy/coolify/compose.yml` as the Compose file.
2. Copy every name from `.env.example` into the resource's environment-variable
   editor and replace every placeholder. Mark credentials as secrets.
   `POSTGRES_PASSWORD` must be URL-safe and must exactly match the password
   embedded in `WATCHTRACE_DATABASE_URL`. Keep all three deployment image
   variables on their `:main` references so an authenticated deployment webhook
   always pulls the images published by the successful workflow.
3. Delete all six obsolete key-value variables from Coolify before deploying:
   `WATCHTRACE_PLATFORM_SIGNING_PRIVATE_KEY`,
   `WATCHTRACE_PLATFORM_SIGNING_PUBLIC_KEY`,
   `WATCHTRACE_MONITOR_HEADER_KEY`, `WATCHTRACE_QUARANTINE_KEY`,
   `WATCHTRACE_WORKER_ENCRYPTION_PRIVATE_KEY`, and
   `WATCHTRACE_WORKER_RESULT_PRIVATE_KEY`. The new key set is different from
   any earlier local set, and leaving the old values available would create
   ambiguous or stale configuration.
4. Set `WATCHTRACE_PUBLIC_URL` to
   `https://watchtrace-129-159-236-232.sslip.io` without a trailing slash. Set
   the OCI SMTP endpoint for the VM's OCI Region.
5. Assign the public domain only to the `frontend` service on container port
   `8080`. Let Coolify provision and renew HTTPS.
6. Confirm `/data/watchtrace/keyset` passes the bootstrap verifier, then reload the
   latest Compose configuration from Git. Do not enable Coolify's immediate
   Git-source Auto Deploy: it could run before GitHub Actions has tested and
   published the new images. The authenticated workflow webhook below is the
   only normal deployment trigger.

Coolify stores the resource variables; this repository stores names and
placeholders only. For this private Phase 1 deployment, the AWS access key must
belong to the queue-scoped non-production runtime identity described in the SQS
runbook. Never store the provisioning operator's credentials in Coolify.
Separate temporary workload roles remain a Phase 4 requirement.

## Enable continuous deployment

Both repositories publish `:main` only after their tests pass. Their final
workflow job then calls the same authenticated Coolify deploy webhook. The
backend deploy job waits for both the control and worker image matrix entries;
the frontend deploy job waits for the frontend image. A pull request, a failed
test, a failed image build, or a non-`main` push cannot deploy.

Keep the repository variable `COOLIFY_DEPLOY_ENABLED` absent or set to `false`
until the first-deployment prerequisites are complete. This avoids deploying an
unconfigured database, queue, SMTP credential, or host key directory merely
because the workflow change was pushed.

1. In self-hosted Coolify, open **Settings > Configuration > Advanced**, enable
   API access, and save. The GitHub workflows send an authenticated `POST` to
   the deployment API after image publication succeeds.
2. Open **Keys & Tokens > API Tokens**, create a token named
   `github-actions-prod`, grant only its deploy permission, choose an expiry,
   and copy it once. The token authorizes a deployment request; it is not a
   GitHub or GHCR credential.
3. Open the WatchTrace Compose resource, select **Configuration > Webhooks**,
   and copy **Deploy Webhook (auth required)**. The URL identifies the resource;
   the API token proves that the caller may deploy it. The URL must use the
   public HTTPS Coolify hostname so a GitHub-hosted runner can reach it.
4. In both `watchtrace-platform` and `watchtrace-console`, open **Settings >
   Secrets and variables > Actions**. Add repository secrets
   `COOLIFY_DEPLOY_WEBHOOK` and `COOLIFY_TOKEN`. GitHub masks and supplies these
   only to the workflow; never commit either value.
5. Finish the SQS, SMTP, Coolify variable, GHCR login, `/data/watchtrace/keyset`,
   domain, and firewall setup. Then add the repository variable
   `COOLIFY_DEPLOY_ENABLED=true` in both repositories. A variable is used for
   this non-secret on/off switch; secrets are used for the webhook and token.
6. Trigger the first deployment with an intentional push to `main`. GitHub
   Actions must finish verification and image publication before its `Trigger
   prod deployment in Coolify` job can run. Thereafter every successful
   `main`-branch push in either repository automatically requests deployment.

The webhook returning success means Coolify accepted the request; inspect the
Coolify deployment status and run the live verifier below to establish that the
new containers became healthy. An automatic post-deployment status poll is not
part of Phase 1 yet.

## First-deployment checks

Run these checks before creating personal test data:

```sh
./scripts/verify-private-deployment.sh https://watchtrace-129-159-236-232.sslip.io
```

Then confirm in Coolify that `postgres`, `api`, `monitor-engine`,
`hosted-worker`, `notification-worker`, and `frontend` are healthy and that
`migrate` exited successfully. Exercise registration, sign-in, email action,
one hosted monitor, SSE refresh, and a controlled container restart. Verify that
the PostgreSQL and worker-journal named volumes remain attached.

The deployment is not considered P1-703 complete merely because the containers
start. The private deployment gate also requires its ARM64, security, capacity,
operational visibility, network-isolation, drift, and rollback checks.

## Automatic deployment and controlled rollback

The GHCR workflows publish `main` plus an immutable `sha-<full commit>` tag and
record each image digest in the GitHub Actions summary. Keep the last known-good
control, worker, and frontend digest references before every deployment.

1. Normal deployment leaves all three Coolify variables on `:main`. The
   successful repository workflow publishes the changed image and calls the
   webhook; Coolify's `pull_policy: always` fetches the current tags.
2. Require `migrate` to exit successfully and every long-running service to be
   healthy. Run `scripts/verify-private-deployment.sh`, then exercise sign-in,
   one hosted monitor, SSE refresh, email, and a controlled service restart.
3. For a controlled rollback, first set `COOLIFY_DEPLOY_ENABLED=false` in both
   repositories. Replace all three image variables with the recorded last
   known-good `ghcr.io/watchtrace/<image>@sha256:<digest>` references and deploy
   from Coolify. Re-run the checks and confirm both named volumes remain
   attached. Disabling automation prevents a concurrent push from immediately
   replacing the rollback.
4. To resume automatic deployment, restore all three `:main` variables, deploy
   and verify once, then set `COOLIFY_DEPLOY_ENABLED=true` in both repositories.
   Record candidate and previous digests, timestamps, migration outcome, health
   result, and the operator in the private deployment log.

A backward-incompatible database migration would make an image-only rollback
unsafe. Phase 1 migrations must therefore remain backward compatible; stop the
exercise and rebuild the disposable private environment if that condition is
not met. Do not mark the deployment/rollback gate until both directions have
actually succeeded on the Oracle ARM64 instance.
