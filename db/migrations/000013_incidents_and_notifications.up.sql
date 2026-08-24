ALTER TABLE monitor_reliability_states
    DROP CONSTRAINT monitor_reliability_states_consecutive_failures_check,
    DROP CONSTRAINT monitor_reliability_states_consecutive_successes_check,
    ADD CONSTRAINT monitor_reliability_states_consecutive_failures_check
      CHECK (consecutive_failures BETWEEN 0 AND 20),
    ADD CONSTRAINT monitor_reliability_states_consecutive_successes_check
      CHECK (consecutive_successes BETWEEN 0 AND 20);

CREATE TABLE alert_rules (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    rule_key text NOT NULL DEFAULT 'consecutive_failures'
      CHECK (rule_key = 'consecutive_failures'),
    failure_threshold smallint NOT NULL DEFAULT 3
      CHECK (failure_threshold BETWEEN 1 AND 20),
    recovery_threshold smallint NOT NULL DEFAULT 2
      CHECK (recovery_threshold BETWEEN 1 AND 20),
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (monitor_id, rule_key),
    UNIQUE (organization_id, environment_id, id),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
      REFERENCES monitors (organization_id, environment_id, id) ON DELETE CASCADE
);

INSERT INTO alert_rules(organization_id,environment_id,monitor_id)
SELECT organization_id,environment_id,id FROM monitors;

CREATE TABLE incidents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    alert_rule_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
    started_at timestamptz NOT NULL,
    opened_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    acknowledged_at timestamptz,
    acknowledged_by_user_id uuid REFERENCES users (id),
    resolved_at timestamptz,
    resolved_by_user_id uuid REFERENCES users (id),
    resolution_kind text
      CHECK (resolution_kind IN ('automatic_recovery','manual_resolution','late_result_correction')),
    resolution_reason text CHECK (resolution_reason IS NULL OR octet_length(resolution_reason) <= 500),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (organization_id, environment_id, id),
    FOREIGN KEY (organization_id, environment_id, monitor_id)
      REFERENCES monitors (organization_id, environment_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, environment_id, alert_rule_id)
      REFERENCES alert_rules (organization_id, environment_id, id) ON DELETE CASCADE,
    CHECK ((acknowledged_at IS NULL) = (acknowledged_by_user_id IS NULL)),
    CHECK (
      (status = 'open' AND resolved_at IS NULL AND resolved_by_user_id IS NULL
       AND resolution_kind IS NULL AND resolution_reason IS NULL) OR
      (status = 'resolved' AND resolved_at IS NOT NULL AND resolution_kind IS NOT NULL)
    )
);
CREATE UNIQUE INDEX incidents_one_open_rule_idx
    ON incidents (monitor_id, alert_rule_id) WHERE status='open';
CREATE INDEX incidents_environment_timeline_idx
    ON incidents (organization_id, environment_id, opened_at DESC, id DESC);

CREATE TABLE incident_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    incident_id uuid NOT NULL,
    event_key text NOT NULL CHECK (octet_length(event_key) BETWEEN 1 AND 80),
    event_type text NOT NULL
      CHECK (event_type IN ('opened','acknowledged','automatic_recovery','manual_resolution','late_result_correction')),
    actor_user_id uuid REFERENCES users (id),
    source_job_id uuid,
    safe_reason text CHECK (safe_reason IS NULL OR octet_length(safe_reason) <= 500),
    occurred_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (incident_id, event_key),
    UNIQUE (organization_id, incident_id, id),
    FOREIGN KEY (organization_id, environment_id, incident_id)
      REFERENCES incidents (organization_id, environment_id, id) ON DELETE CASCADE
);
CREATE INDEX incident_events_timeline_idx
    ON incident_events (incident_id, occurred_at, id);

CREATE TABLE notification_outbox (
    delivery_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    incident_id uuid NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    incident_event_id uuid NOT NULL REFERENCES incident_events (id) ON DELETE CASCADE,
    recipient_user_id uuid NOT NULL REFERENCES users (id),
    recipient_email text NOT NULL
      CHECK (btrim(recipient_email) <> '' AND octet_length(recipient_email) <= 254
             AND recipient_email !~ '[\r\n]'),
    channel text NOT NULL DEFAULT 'email' CHECK (channel = 'email'),
    transition text NOT NULL CHECK (transition IN ('opened','resolved')),
    state text NOT NULL DEFAULT 'pending'
      CHECK (state IN ('pending','leased','accepted','failed')),
    attempt_count smallint NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 4),
    next_attempt_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    lease_owner text CHECK (lease_owner IS NULL OR octet_length(lease_owner) BETWEEN 1 AND 128),
    lease_token uuid,
    lease_expires_at timestamptz,
    provider_message_id text
      CHECK (provider_message_id IS NULL OR octet_length(provider_message_id) <= 255),
    last_provider_status text
      CHECK (last_provider_status IS NULL OR octet_length(last_provider_status) <= 160),
    accepted_at timestamptz,
    failed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (incident_event_id, recipient_user_id, channel),
    FOREIGN KEY (organization_id, incident_id, incident_event_id)
      REFERENCES incident_events (organization_id, incident_id, id) ON DELETE CASCADE,
    CHECK (
      (state = 'leased' AND lease_owner IS NOT NULL AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL) OR
      (state <> 'leased' AND lease_owner IS NULL AND lease_token IS NULL AND lease_expires_at IS NULL)
    ),
    CHECK ((state = 'accepted') = (accepted_at IS NOT NULL)),
    CHECK ((state = 'failed') = (failed_at IS NOT NULL))
);
CREATE INDEX notification_outbox_claim_idx
    ON notification_outbox (next_attempt_at, created_at, delivery_id)
    WHERE state='pending';
CREATE INDEX notification_outbox_expired_lease_idx
    ON notification_outbox (lease_expires_at, delivery_id)
    WHERE state='leased';

CREATE TABLE notification_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_id uuid NOT NULL REFERENCES notification_outbox (delivery_id) ON DELETE CASCADE,
    attempt_number smallint NOT NULL CHECK (attempt_number BETWEEN 1 AND 4),
    outcome text NOT NULL CHECK (outcome IN ('accepted','retry_scheduled','failed')),
    provider_status text NOT NULL CHECK (octet_length(provider_status) BETWEEN 1 AND 160),
    attempted_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (delivery_id, attempt_number)
);
CREATE INDEX notification_attempts_delivery_idx
    ON notification_attempts (delivery_id, attempt_number);
