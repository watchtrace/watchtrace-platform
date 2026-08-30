# WatchTrace `prod` deployment in Coolify

WatchTrace uses one environment named `prod` and two Coolify resources:

1. `watchtrace-backend`: the existing Docker Compose resource containing
   PostgreSQL, deployment-key verification, migration, API, monitor engine,
   notification worker, and hosted worker.
2. `watchtrace-frontend`: one Docker Image application containing only the
   React/Nginx frontend.

This hybrid keeps state, keys, startup ordering, and backend networking in one
version-controlled Compose file while allowing a frontend commit to deploy only
the frontend container.

## Backend Compose resource

The existing `compose.yml` is still the backend definition. Its PostgreSQL and
worker-journal volumes are unchanged, so the split does not copy or rename
stateful volumes. The only structural change is removal of the `frontend`
service.

Keep every variable from `.env.example`. `WATCHTRACE_CONTROL_IMAGE` and
`WATCHTRACE_WORKER_IMAGE` must remain ordinary, non-secret Coolify variables;
GitHub Actions reads their previous values and updates them to immutable image
digests. Credentials such as PostgreSQL, AWS, and SMTP values remain secrets.

Do not expose a domain or host port for PostgreSQL, API, monitor, notification,
or hosted worker. Enable **Connect To Predefined Network** so the separate
frontend can reach the Compose service name `api` on port 8080.

The one-time key bootstrap remains unchanged:

```sh
ssh root@129.159.236.232 \
  'sh -s -- ghcr.io/watchtrace/watchtrace-platform@sha256:<control-digest>' \
  < scripts/bootstrap-deployment-keys.sh
```

Continuous deployment verifies the installed `/data/watchtrace/keyset`; it does
not generate or rotate keys.

## Frontend Docker Image application

Create `watchtrace-frontend` from:

```text
ghcr.io/watchtrace/watchtrace-console@sha256:<frontend-digest>
```

Configure:

- Ports Exposes: `8080`
- Ports Mappings: empty
- Connect To Predefined Network: enabled
- Health check: HTTP GET `/health`, port `8080`, expected `200`
- Interval: 15 seconds
- Timeout: 5 seconds
- Retries: 5
- Start period: 10 seconds
- Stop grace period: 30 seconds
- Consistent Container Names: disabled
- Custom/container name: empty
- Domain: the single public WatchTrace HTTPS hostname
- Runtime environment variables: none
- Persistent storage: none

The frontend Nginx container proxies `/api/v1` and SSE to `http://api:8080`.
After connecting both resources to the predefined network, verify from the
frontend container that the name `api` resolves and reaches the backend API.

## GHCR and Coolify credentials

If GHCR images are private, log the Oracle/Coolify server user into GHCR with a
dedicated GitHub classic personal access token limited to `read:packages`:

```sh
printf '%s' "$WATCHTRACE_GHCR_READ_TOKEN" | \
  docker login ghcr.io --username "$WATCHTRACE_GHCR_USER" --password-stdin
unset WATCHTRACE_GHCR_READ_TOKEN
```

Create a separate Coolify API token named `github-actions-prod` with `read`,
`write`, and `deploy`. Store it as the GitHub `prod` environment secret
`COOLIFY_TOKEN`. The GHCR token downloads images; the Coolify token changes and
deploys resources. Never interchange them.

## One-time frontend split

1. Set `COOLIFY_DEPLOY_ENABLED=false` in both GitHub repositories.
2. Record the current backend and frontend image references.
3. Create and manually verify `watchtrace-frontend` on the same predefined
   network. Initially leave its public domain unassigned.
4. Update the existing backend Compose resource from Git. Confirm the new
   definition removes only the frontend service and retains the same PostgreSQL
   and worker-journal volumes.
5. Stop/remove the old Compose-managed frontend container without deleting any
   volume. Assign its public domain to `watchtrace-frontend` on port 8080.
6. Verify `/health`, React, `/api/v1`, authentication, and SSE.
7. Put the backend Compose UUID into backend GitHub variable
   `COOLIFY_BACKEND_UUID`; put the frontend application UUID into frontend
   variable `COOLIFY_FRONTEND_UUID`.
8. Enable continuous deployment and make one intentional frontend commit.
   Confirm only `watchtrace-frontend` deploys. Then make one intentional backend
   commit and confirm only the backend Compose resource deploys.

No PostgreSQL or worker-journal migration is part of this split.

## Automated deployment

Backend merge:

```text
tests → publish control and worker digests
      → update WATCHTRACE_CONTROL_IMAGE and WATCHTRACE_WORKER_IMAGE
      → deploy watchtrace-backend once
      → poll status and verify saved values
      → live checks and retained release record
```

Frontend merge:

```text
tests → publish frontend digest
      → update only watchtrace-frontend
      → poll status and verify digest
      → live checks and retained release record
```

The mutable `:main` tag is published for convenience but production resources
are updated automatically to immutable digests. There is no manual digest edit
on normal deployments.

## Rollback

Set `COOLIFY_DEPLOY_ENABLED=false` before rollback.

- Frontend failure: roll back only `watchtrace-frontend` using its prior digest.
- Backend failure: restore both previous control and worker references in the
  backend Compose variables and redeploy the backend resource once.
- Never automatically run a database down migration. Confirm schema
  compatibility before rolling an application image backward.
- Keep PostgreSQL and worker-journal volumes attached and never use Docker's
  unused-volume deletion during rollback.

Scheduled backup/restore remains Phase 4 work while this is an owner-only,
disposable project. A one-time VM/volume snapshot before the frontend split is
still sensible protection against an operator mistake.
