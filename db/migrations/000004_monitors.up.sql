CREATE TABLE monitors (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    name text NOT NULL CHECK (btrim(name) <> '' AND octet_length(name) <= 120),
    target_url text NOT NULL CHECK (btrim(target_url) <> '' AND octet_length(target_url) <= 2048),
    method text NOT NULL DEFAULT 'GET' CHECK (method = 'GET'),
    interval_seconds integer NOT NULL DEFAULT 300
        CHECK (interval_seconds IN (60, 120, 300, 600, 1800)),
    timeout_seconds integer NOT NULL DEFAULT 5
        CHECK (timeout_seconds BETWEEN 1 AND 10),
    expected_status_min smallint NOT NULL DEFAULT 200
        CHECK (expected_status_min BETWEEN 100 AND 599),
    expected_status_max smallint NOT NULL DEFAULT 299
        CHECK (expected_status_max BETWEEN 100 AND 599),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (organization_id, id),
    FOREIGN KEY (organization_id, environment_id)
        REFERENCES environments (organization_id, id),
    CHECK (expected_status_min <= expected_status_max)
);

CREATE INDEX monitors_environment_list_idx
    ON monitors (organization_id, environment_id, created_at, id);
