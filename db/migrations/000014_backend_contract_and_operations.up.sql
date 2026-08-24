CREATE TABLE api_refresh_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    environment_id uuid,
    event_type text NOT NULL CHECK (event_type IN (
      'organization.changed','project.changed','environment.changed','membership.changed',
      'monitor.changed','check.accepted','incident.changed','notification.changed')),
    resource_type text NOT NULL CHECK (resource_type IN (
      'organization','project','environment','membership','monitor','check','incident','notification')),
    resource_id uuid NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (organization_id, environment_id)
      REFERENCES environments (organization_id, id) ON DELETE CASCADE
);
CREATE INDEX api_refresh_events_tenant_replay_idx
    ON api_refresh_events (organization_id, id);
CREATE INDEX api_refresh_events_environment_replay_idx
    ON api_refresh_events (environment_id, id) WHERE environment_id IS NOT NULL;

CREATE TABLE audit_logs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid REFERENCES organizations(id) ON DELETE CASCADE,
    actor_user_id uuid REFERENCES users(id),
    action text NOT NULL CHECK (octet_length(action) BETWEEN 1 AND 80),
    resource_type text NOT NULL CHECK (octet_length(resource_type) BETWEEN 1 AND 40),
    resource_id uuid,
    safe_metadata jsonb NOT NULL DEFAULT '{}'::jsonb
      CHECK (jsonb_typeof(safe_metadata)='object' AND octet_length(safe_metadata::text)<=4096),
    occurred_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX audit_logs_tenant_timeline_idx
    ON audit_logs (organization_id, occurred_at DESC, id DESC);

CREATE TABLE maintenance_status (
    task_name text PRIMARY KEY CHECK (task_name IN (
      'session_cleanup','queue_maintenance','rollup_retention','notification_cleanup','backup')),
    last_started_at timestamptz,
    last_success_at timestamptz,
    last_failure_at timestamptz,
    last_safe_error text CHECK (last_safe_error IS NULL OR octet_length(last_safe_error)<=160),
    rows_affected bigint NOT NULL DEFAULT 0 CHECK (rows_affected>=0),
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO maintenance_status(task_name) VALUES
  ('session_cleanup'),('queue_maintenance'),('rollup_retention'),('notification_cleanup'),('backup');

CREATE INDEX notification_outbox_pending_age_idx
    ON notification_outbox (created_at, delivery_id)
    WHERE state IN ('pending','leased');

