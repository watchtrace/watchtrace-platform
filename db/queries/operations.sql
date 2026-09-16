-- name: GetSchedulerDelaySeconds :one
SELECT COALESCE(EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP-min(next_check_at)))::bigint,0)::bigint
FROM monitors
WHERE deleted_at IS NULL AND paused_at IS NULL AND next_check_at<CURRENT_TIMESTAMP;

-- name: GetResultConsumerDelaySeconds :one
SELECT COALESCE(EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP-max(created_at)))::bigint,0)::bigint
FROM health_checks;

-- name: CountRecentCoverageGaps :one
SELECT count(*)::bigint
FROM monitoring_coverage_gaps
WHERE scheduled_at>CURRENT_TIMESTAMP-INTERVAL '24 hours';

-- name: GetRecentCheckCounts :one
SELECT count(*)::bigint AS completed_checks,
       count(*) FILTER (WHERE NOT succeeded)::bigint AS failed_checks
FROM health_checks
WHERE completed_at>CURRENT_TIMESTAMP-INTERVAL '24 hours';

-- name: CountPendingNotifications :one
SELECT count(*)::bigint
FROM notification_outbox
WHERE state IN ('pending','leased');

-- name: GetOldestPendingNotification :one
SELECT created_at
FROM notification_outbox
WHERE state IN ('pending','leased')
ORDER BY created_at
LIMIT 1;

-- name: ListMaintenanceStatuses :many
SELECT task_name,last_started_at,last_success_at,last_failure_at,last_safe_error,rows_affected
FROM maintenance_status
ORDER BY task_name;

-- name: UpdateMaintenanceStatus :execrows
UPDATE maintenance_status
SET last_started_at=$2,
    last_success_at=COALESCE(sqlc.narg(last_success_at),last_success_at),
    last_failure_at=COALESCE(sqlc.narg(last_failure_at),last_failure_at),
    last_safe_error=sqlc.narg(last_safe_error),
    rows_affected=$3,
    updated_at=CURRENT_TIMESTAMP
WHERE task_name=$1;

-- name: DeleteExpiredUserActionTokens :execrows
DELETE FROM user_action_tokens
WHERE expires_at<$1::timestamptz OR used_at<($1::timestamptz-INTERVAL '7 days');

-- name: DeleteExpiredOrganizationInvitations :execrows
DELETE FROM org_invitations
WHERE expires_at<($1::timestamptz-INTERVAL '7 days') OR accepted_at<($1::timestamptz-INTERVAL '7 days');

-- name: DeleteOldNotificationDeliveries :execrows
DELETE FROM notification_outbox
WHERE state IN ('accepted','failed') AND updated_at<($1::timestamptz-INTERVAL '30 days');

-- name: DeleteOldRefreshEvents :execrows
DELETE FROM api_refresh_events
WHERE occurred_at<($1::timestamptz-INTERVAL '1 day');
