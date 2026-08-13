# Modular check worker

The Phase 1.2 worker has no PostgreSQL dependency or database configuration. It
accepts work through direct Amazon SQS FIFO or the outbound HTTPS gateway,
verifies and decrypts the immutable job, executes the shared bounded check
engine, journals the signed result in SQLite, publishes the result, and only
then acknowledges the job.

Generate pool keys locally with `go run ./cmd/worker-pool -mode generate
-pool customer-a`. Protect the two generated private-key files with mode 0600,
an encrypted host volume, and the customer's backup policy. Only the generated
public JSON bundle is registered by an operator. Rotate by registering new key
IDs, deploying workers that can decrypt/verify the transition set, then revoke
the old pool key after all old jobs and journaled results have expired.

Configure the current IDs with `WATCHTRACE_PLATFORM_SIGNING_KEY_ID`,
`WATCHTRACE_WORKER_ENCRYPTION_KEY_ID`, and `WATCHTRACE_RESULT_KEY_ID`. The
worker rejects an envelope when its key IDs are absent from trusted local
configuration. During rotation, `WATCHTRACE_WORKER_KEYRING` can point to a
strict JSON file containing `worker_encryption` and `platform_signing` maps
from key ID to protected key-file path, plus a `revoked` ID array. Keep current
and previous keys for at least 21 days. An emergency-revoked ID is rejected
immediately, without the normal overlap.

HTTPS mode is the default for customer-VPC workers and requires a client
certificate/key plus the gateway CA. Direct SQS mode requires temporary AWS
credentials limited to the pool's job FIFO and the shared result FIFO. Neither
mode accepts inbound customer traffic. Private CIDRs come only from the local
`WATCHTRACE_PRIVATE_CIDRS` configuration; job messages cannot widen them.

Mount `/var/lib/watchtrace-worker` as a restricted persistent volume. Losing it
does not lose central results, but removes same-worker replay protection and can
cause a duplicate GET or HEAD. Journal terminal entries are retained for seven
days. The image runs as non-root and builds for `linux/amd64` and `linux/arm64`.

The readiness endpoint fails when the configured measured clock offset exceeds
five seconds or while a journaled result cannot be published. Hosts must use UTC and an active NTP service. Shutdown stops new
pulls and gives current requests the orchestrator grace period; SQS visibility
recovers work that does not finish.

Build the complete release set with `scripts/build-worker-release.ps1`. It
produces Linux AMD64/ARM64 binaries, a multi-architecture OCI archive, SHA-256
checksums, a CycloneDX SBOM, and Ed25519 signature JSON for each artifact. The
signing private key is mounted read-only and is not included in an image or
artifact. Verify a downloaded file with:

```text
go run ./cmd/artifact-sign -mode verify -file ARTIFACT -key artifact-signing.pub -signature ARTIFACT.sig.json
```

The container runs as the distroless `nonroot` user. Customer deployments mount
only the outbound mTLS identity, trusted keyring, and persistent journal; they
must not configure AWS credentials. Hosted direct-SQS workers use temporary
workload-role credentials through the AWS SDK default credential chain.
