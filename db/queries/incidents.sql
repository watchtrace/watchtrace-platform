-- name: EnsureDefaultAlertRule :exec
INSERT INTO alert_rules (organization_id, environment_id, monitor_id)
SELECT organization_id, environment_id, id
FROM monitors
WHERE id = sqlc.arg(monitor_id)::text::uuid
ON CONFLICT (monitor_id, rule_key) DO NOTHING;

-- name: GetEnabledMonitorAlertRule :one
SELECT alert_rules.id::text AS rule_id,
       alert_rules.organization_id::text AS organization_id,
       alert_rules.environment_id::text AS environment_id,
       alert_rules.failure_threshold,
       monitor_reliability_states.observed_state
FROM alert_rules
JOIN monitor_reliability_states
  ON monitor_reliability_states.monitor_id = alert_rules.monitor_id
WHERE alert_rules.monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND alert_rules.rule_key = 'consecutive_failures'
  AND alert_rules.enabled;

-- name: LockOpenIncident :one
SELECT id::text AS id
FROM incidents
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
  AND alert_rule_id = sqlc.arg(rule_id)::text::uuid
  AND status = 'open'
FOR UPDATE;

-- name: ListRecentIncidentEvaluationTimes :many
SELECT scheduled_at
FROM monitor_result_evaluations
WHERE monitor_id = sqlc.arg(monitor_id)::text::uuid
ORDER BY scheduled_at DESC, job_id DESC
LIMIT sqlc.arg(failure_threshold);

-- name: CreateOpenIncident :one
INSERT INTO incidents (
    organization_id, environment_id, monitor_id, alert_rule_id,
    started_at, opened_at
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(monitor_id)::text::uuid,
    sqlc.arg(rule_id)::text::uuid,
    sqlc.arg(started_at),
    sqlc.arg(opened_at)
)
ON CONFLICT (monitor_id, alert_rule_id) WHERE status = 'open' DO NOTHING
RETURNING id::text AS id;

-- name: ResolveOpenIncident :execrows
UPDATE incidents
SET status = 'resolved',
    resolved_at = sqlc.arg(resolved_at),
    resolved_by_user_id = NULLIF(sqlc.arg(actor_user_id)::text, '')::uuid,
    resolution_kind = sqlc.arg(resolution_kind),
    resolution_reason = NULLIF(btrim(sqlc.arg(resolution_reason)::text), ''),
    updated_at = sqlc.arg(resolved_at)
WHERE id = sqlc.arg(incident_id)::text::uuid
  AND status = 'open';

-- name: GetIncidentTenant :one
SELECT organization_id::text AS organization_id,
       environment_id::text AS environment_id
FROM incidents
WHERE id = sqlc.arg(incident_id)::text::uuid;

-- name: UpsertIncidentEvent :one
INSERT INTO incident_events (
    organization_id, environment_id, incident_id, event_key, event_type,
    actor_user_id, source_job_id, safe_reason, occurred_at
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    sqlc.arg(incident_id)::text::uuid,
    sqlc.arg(event_key),
    sqlc.arg(event_type),
    NULLIF(sqlc.arg(actor_user_id)::text, '')::uuid,
    NULLIF(sqlc.arg(source_job_id)::text, '')::uuid,
    NULLIF(btrim(sqlc.arg(safe_reason)::text), ''),
    sqlc.arg(occurred_at)
)
ON CONFLICT (incident_id, event_key)
DO UPDATE SET event_key = EXCLUDED.event_key
RETURNING id::text AS id;

-- name: InsertIncidentRefreshEvent :exec
INSERT INTO api_refresh_events (
    organization_id, environment_id, event_type, resource_type, resource_id
)
VALUES (
    sqlc.arg(organization_id)::text::uuid,
    sqlc.arg(environment_id)::text::uuid,
    'incident.changed',
    'incident',
    sqlc.arg(incident_id)::text::uuid
);

-- name: AcknowledgeOpenIncident :exec
UPDATE incidents
SET acknowledged_at = sqlc.arg(acknowledged_at),
    acknowledged_by_user_id = sqlc.arg(user_id)::text::uuid,
    updated_at = sqlc.arg(acknowledged_at)
WHERE id = sqlc.arg(incident_id)::text::uuid
  AND status = 'open'
  AND acknowledged_at IS NULL;

-- name: LoadAuthorizedIncident :one
SELECT incidents.id::text AS id,
       incidents.organization_id::text AS organization_id,
       incidents.environment_id::text AS environment_id,
       incidents.monitor_id::text AS monitor_id,
       incidents.status,
       incidents.started_at,
       incidents.opened_at,
       incidents.acknowledged_at,
       incidents.acknowledged_by_user_id,
       incidents.resolved_at,
       incidents.resolved_by_user_id,
       incidents.resolution_kind,
       incidents.resolution_reason,
       org_members.role
FROM incidents
JOIN org_members
  ON org_members.organization_id = incidents.organization_id
 AND org_members.user_id = sqlc.arg(user_id)::text::uuid
WHERE incidents.environment_id = sqlc.arg(environment_id)::text::uuid
  AND incidents.id = sqlc.arg(incident_id)::text::uuid
FOR UPDATE OF incidents;
