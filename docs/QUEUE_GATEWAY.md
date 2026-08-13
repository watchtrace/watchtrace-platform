# Stateless HTTPS queue gateway

The queue gateway has Amazon SQS access but no PostgreSQL driver, database URL,
or product-data API. Its Ed25519-signed, expiring configuration snapshot maps an authenticated worker-pool
identity to exactly one job FIFO and the shared result FIFO. Mutual TLS is
mandatory in the shipped command; a short-lived external pool-token validator
can be injected at the package boundary when an approved issuer is available.

Pull responses replace the raw SQS receipt handle with a short-lived AES-GCM
lease token bound to the pool, job ID, snapshot hash, trusted expiry, and
receipt. Result submission verifies the pool signature and publishes to the
result FIFO before deleting the job. The only no-result deletion is a signed
expired acknowledgement accepted after gateway time reaches the trusted job
expiry within the configured tolerance.

Deploy with outbound HTTPS to SQS, no route to PostgreSQL, a read-only trusted
configuration file, a protected lease-key file, and certificates issued by the
operator-controlled worker CA. Receipt handles, lease tokens, keys, decrypted
jobs, and result bodies are never logged.

Each trusted pool entry includes the queue mapping, result public key and ID,
current/previous schema range, capabilities, revoked certificate serials, and
per-pool request/byte/concurrent-pull/result limits. The gateway verifies the
snapshot with `WATCHTRACE_GATEWAY_CONFIG_SIGNING_PUBLIC_KEY` before opening its
listener and rejects expired, altered, or wrong-environment configuration. mTLS
certificates last at most 30 days; renewal starts before ten days remain and a
revoked serial is rejected from the signed snapshot. The gateway requires both
the signature and configured result key ID to match before publishing a result
or expired acknowledgement. `api/worker-v1.openapi.yaml` is the independent
current/previous worker contract.
