-- name: ListEnvironmentRefreshEvents :many
SELECT id, event_type, resource_type, resource_id::text AS resource_id, occurred_at
FROM api_refresh_events
WHERE environment_id = sqlc.arg(environment_id)::text::uuid
  AND id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(result_limit);
