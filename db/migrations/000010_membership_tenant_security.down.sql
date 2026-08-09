ALTER TABLE health_checks DROP CONSTRAINT health_checks_tenant_job_fkey;
ALTER TABLE check_jobs DROP CONSTRAINT check_jobs_tenant_job_id_key;
DROP TABLE org_invitations;
