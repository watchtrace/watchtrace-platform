-- name: EnqueueIncidentNotifications :execrows
INSERT INTO notification_outbox (
    organization_id, incident_id, incident_event_id, recipient_user_id,
    recipient_email, transition
)
SELECT incident_events.organization_id,
       incident_events.incident_id,
       incident_events.id,
       org_members.user_id,
       lower(btrim(users.email)),
       sqlc.arg(transition)
FROM incident_events
JOIN org_members ON org_members.organization_id = incident_events.organization_id
JOIN users ON users.id = org_members.user_id
WHERE incident_events.id = sqlc.arg(event_id)::text::uuid
  AND users.email_verified_at IS NOT NULL
  AND org_members.incident_notifications_enabled
  AND org_members.role IN ('owner', 'admin', 'member', 'viewer')
ON CONFLICT (incident_event_id, recipient_user_id, channel) DO NOTHING;

-- name: ReclaimExpiredNotificationLeases :execrows
UPDATE notification_outbox
SET state = 'pending',
    next_attempt_at = LEAST(next_attempt_at, sqlc.arg(reclaimed_at)),
    lease_owner = NULL,
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = sqlc.arg(reclaimed_at)
WHERE state = 'leased'
  AND lease_expires_at <= sqlc.arg(reclaimed_at);

-- name: ClaimNotificationDelivery :one
WITH candidate AS (
    SELECT delivery_id
    FROM notification_outbox
    WHERE state = 'pending'
      AND next_attempt_at <= sqlc.arg(claimed_at)
      AND attempt_count < 4
    ORDER BY next_attempt_at, created_at, delivery_id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE notification_outbox AS outbox
SET state = 'leased',
    lease_owner = sqlc.arg(worker_id),
    lease_token = gen_random_uuid(),
    lease_expires_at = sqlc.arg(lease_expires_at),
    updated_at = sqlc.arg(claimed_at)
FROM candidate
WHERE outbox.delivery_id = candidate.delivery_id
RETURNING outbox.delivery_id::text AS delivery_id,
          outbox.incident_id::text AS incident_id,
          outbox.organization_id::text AS organization_id,
          (SELECT environment_id::text FROM incidents WHERE id = outbox.incident_id) AS environment_id,
          outbox.recipient_email,
          outbox.transition,
          outbox.lease_token::text AS lease_token,
          outbox.attempt_count + 1 AS attempt_number;

-- name: AcceptNotificationDelivery :execrows
UPDATE notification_outbox
SET state = 'accepted',
    attempt_count = sqlc.arg(attempt_number),
    provider_message_id = sqlc.arg(provider_message_id),
    last_provider_status = sqlc.arg(provider_status),
    accepted_at = sqlc.arg(attempted_at),
    lease_owner = NULL,
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = sqlc.arg(attempted_at)
WHERE delivery_id = sqlc.arg(delivery_id)::text::uuid
  AND state = 'leased'
  AND lease_token = sqlc.arg(lease_token)::text::uuid;

-- name: CompleteFailedNotificationAttempt :execrows
UPDATE notification_outbox
SET state = sqlc.arg(state),
    attempt_count = sqlc.arg(attempt_number),
    next_attempt_at = sqlc.arg(next_attempt_at),
    last_provider_status = sqlc.arg(provider_status),
    failed_at = CASE
      WHEN sqlc.arg(state)::text = 'failed' THEN sqlc.arg(attempted_at)::timestamptz
      ELSE NULL
    END,
    lease_owner = NULL,
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = sqlc.arg(attempted_at)
WHERE delivery_id = sqlc.arg(delivery_id)::text::uuid
  AND state = 'leased'
  AND lease_token = sqlc.arg(lease_token)::text::uuid;

-- name: InsertNotificationAttempt :exec
INSERT INTO notification_attempts (
    delivery_id, attempt_number, outcome, provider_status, attempted_at
)
VALUES (
    sqlc.arg(delivery_id)::text::uuid,
    sqlc.arg(attempt_number),
    sqlc.arg(outcome),
    sqlc.arg(provider_status),
    sqlc.arg(attempted_at)
);

-- name: InsertNotificationRefreshEvent :exec
INSERT INTO api_refresh_events (
    organization_id, environment_id, event_type, resource_type, resource_id
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    'notification.changed',
    'notification',
    sqlc.arg(delivery_id)::text::uuid
);
