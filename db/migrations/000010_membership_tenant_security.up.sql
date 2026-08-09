CREATE TABLE org_invitations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    invited_by_user_id uuid NOT NULL REFERENCES users (id),
    email text NOT NULL CHECK (btrim(email) <> '' AND octet_length(email) <= 254),
    role text NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (expires_at > created_at),
    CHECK (accepted_at IS NULL OR accepted_at >= created_at)
);

CREATE UNIQUE INDEX org_invitations_active_email_idx
    ON org_invitations (organization_id, lower(btrim(email)))
    WHERE accepted_at IS NULL;

ALTER TABLE check_jobs
    ADD CONSTRAINT check_jobs_tenant_job_id_key
        UNIQUE (organization_id, environment_id, monitor_id, id);

ALTER TABLE health_checks
    ADD CONSTRAINT health_checks_tenant_job_fkey
        FOREIGN KEY (organization_id, environment_id, monitor_id, job_id)
        REFERENCES check_jobs (organization_id, environment_id, monitor_id, id);
