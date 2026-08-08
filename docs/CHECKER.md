# Initial HTTP Check Worker

P1-107 adds the first durable worker path. A worker processes at most one job
per `RunNext` call; callers decide the bounded worker concurrency.

## Claim and lease

The worker claims the oldest scheduled `pending` job before manual work with a
single PostgreSQL statement using `FOR UPDATE SKIP LOCKED`. Claiming changes the
job to `running`, records a bounded worker ID, increments its attempt count,
creates a random lease token, and assigns a 60-second lease. The claim statement
finishes before any network request begins. The lease is safely longer than the
maximum Phase 1 monitor timeout of ten seconds.

P1-107 claims pending jobs only. Expired-lease recovery, internal retry delay,
dead jobs, and stale monitor-version cancellation remain part of P1-307.

## Request execution

- The current monitor method, URL, timeout, and expected status range are read
  while the job is claimed.
- Requests use the guarded destination client, do not use environment proxies,
  and do not follow redirects until the complete redirect policy is added.
- The request carries the fixed `WatchTrace-Phase1/1.0` User-Agent.
- At most 64 KiB of a response body is streamed to `io.Discard`, then the body
  is closed. No response-body field exists in `health_checks`, and body content
  is never included in a stored error.
- Expected status responses are successful. DNS, connection, TLS, timeout,
  protocol, response-read, unsafe-target, and unexpected-status outcomes use
  fixed safe error categories and complete as final target results.
- Cancellation of the parent worker context is an internal interruption. It
  does not write a target result; the durable running job remains for later
  lease recovery.

## Result transaction and delivery guarantee

After the request finishes, one PostgreSQL transaction locks the job, verifies
the current lease token, inserts the `health_checks` row, and marks the job
`completed`. The job ID is the result primary key, so repeated execution can
send a rare duplicate GET or HEAD request but cannot store another final row.
A stale lease token cannot commit a result over a newer worker. Any failure to
mark the job complete rolls back the result insertion.

Stored results contain tenant IDs, monitor and job IDs, job type, scheduled,
start, and completion timestamps, success, optional HTTP status, a fixed error
category, and total duration in microseconds. They do not contain the target
URL, response headers, or response body.
