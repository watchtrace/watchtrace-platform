// Package workerpool owns the operator-only worker-pool lifecycle.
package workerpool

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Registration struct {
	ID, Mode, JobQueueURL, JobQueueARN, JobDLQURL, JobDLQARN string
	EncryptionKeyID, ResultKeyID                             string
	EncryptionPublic, ResultPublic                           []byte
	SchemaMin, SchemaMax                                     int
	MTLSFingerprint                                          string
	MTLSNotAfter                                             time.Time
}

type Service struct{ db DB }

func New(db DB) *Service { return &Service{db: db} }

func (s *Service) Register(ctx context.Context, r Registration, actor, reason string) error {
	if s.db == nil || r.ID == "" || (r.Mode != "hosted" && r.Mode != "customer_vpc") || r.JobQueueURL == "" || r.JobQueueARN == "" || r.JobDLQURL == "" || r.JobDLQARN == "" || len(r.EncryptionPublic) != 32 || len(r.ResultPublic) != 32 || r.EncryptionKeyID == "" || r.ResultKeyID == "" || r.SchemaMin < 1 || r.SchemaMax > 2 || r.SchemaMin > r.SchemaMax || actor == "" || reason == "" || (r.Mode == "customer_vpc" && (r.MTLSFingerprint == "" || r.MTLSNotAfter.IsZero() || r.MTLSNotAfter.After(time.Now().UTC().Add(31*24*time.Hour)))) {
		return errors.New("invalid worker-pool registration")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `INSERT INTO worker_pools(id,mode,enabled,lifecycle_state,schema_min,schema_max,encryption_key_id,encryption_public_key,result_key_id,result_public_key,job_queue_url,job_queue_arn,job_dlq_url,job_dlq_arn)
VALUES($1,$2,false,'provisioning',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.ID, r.Mode, r.SchemaMin, r.SchemaMax, r.EncryptionKeyID, r.EncryptionPublic, r.ResultKeyID, r.ResultPublic, r.JobQueueURL, r.JobQueueARN, r.JobDLQURL, r.JobDLQARN)
	if err != nil {
		return fmt.Errorf("register pool: %w", err)
	}
	for _, credential := range []struct {
		purpose, keyID string
		public         []byte
	}{{"job_encryption", r.EncryptionKeyID, r.EncryptionPublic}, {"result_signing", r.ResultKeyID, r.ResultPublic}} {
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(credential.public))
		if _, err = tx.Exec(ctx, `INSERT INTO worker_pool_credentials(worker_pool_id,purpose,key_id,public_material,fingerprint,status,activates_at) VALUES($1,$2,$3,$4,$5,'pending',CURRENT_TIMESTAMP)`, r.ID, credential.purpose, credential.keyID, credential.public, fingerprint); err != nil {
			return err
		}
	}
	if r.Mode == "customer_vpc" {
		if _, err = tx.Exec(ctx, `INSERT INTO worker_pool_credentials(worker_pool_id,purpose,key_id,fingerprint,status,activates_at,not_after) VALUES($1,'mtls_certificate',$2,$3,'pending',CURRENT_TIMESTAMP,$4)`, r.ID, "mtls-"+r.EncryptionKeyID, r.MTLSFingerprint, r.MTLSNotAfter); err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, r.ID, "register", actor, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Activate(ctx context.Context, id string, manifestDigest []byte, gatewayMapped bool, actor, reason string) error {
	if len(manifestDigest) != 32 || !gatewayMapped {
		return errors.New("pool dependencies are not verified")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var ready bool
	err = tx.QueryRow(ctx, `SELECT lifecycle_state='provisioning' AND job_queue_url IS NOT NULL AND job_queue_arn IS NOT NULL AND job_dlq_url IS NOT NULL AND job_dlq_arn IS NOT NULL AND encryption_public_key IS NOT NULL AND result_public_key IS NOT NULL AND (SELECT count(*)=CASE WHEN worker_pools.mode='customer_vpc' THEN 3 ELSE 2 END FROM worker_pool_credentials c WHERE c.worker_pool_id=worker_pools.id AND c.status='pending' AND (c.purpose<>'mtls_certificate' OR c.not_after>CURRENT_TIMESTAMP+INTERVAL '10 days')) FROM worker_pools WHERE id=$1 FOR UPDATE`, id).Scan(&ready)
	if err != nil || !ready {
		if err == nil {
			err = errors.New("pool is incomplete")
		}
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE worker_pools SET enabled=true,lifecycle_state='active',manifest_digest=$2,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, id, manifestDigest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE worker_pool_credentials SET status='active' WHERE worker_pool_id=$1 AND status='pending'`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, id, "activate", actor, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Transition(ctx context.Context, id, target, actor, reason string) error {
	actions := map[string]string{"draining": "drain", "revoked": "revoke", "failed": "fail", "deleting": "delete_start"}
	action, ok := actions[target]
	if !ok {
		return errors.New("invalid lifecycle target")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var current string
	if err = tx.QueryRow(ctx, `SELECT lifecycle_state FROM worker_pools WHERE id=$1 FOR UPDATE`, id).Scan(&current); err != nil {
		return err
	}
	allowed := map[string]map[string]bool{"active": {"draining": true, "revoked": true, "failed": true}, "provisioning": {"failed": true, "revoked": true}, "draining": {"revoked": true, "deleting": true}, "revoked": {"deleting": true}, "failed": {"deleting": true}}[current][target]
	if !allowed {
		return errors.New("invalid lifecycle transition")
	}
	_, err = tx.Exec(ctx, `UPDATE worker_pools SET lifecycle_state=$2,enabled=false,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, id, target)
	if err != nil {
		return err
	}
	if target == "revoked" {
		_, err = tx.Exec(ctx, `UPDATE worker_pool_credentials SET status='revoked',revoked_at=CURRENT_TIMESTAMP WHERE worker_pool_id=$1 AND status IN('pending','active','retired')`, id)
		if err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, id, action, actor, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Reconcile(ctx context.Context, id string, expectedDigest []byte, gatewayMapped bool, actor, reason string) error {
	if len(expectedDigest) != 32 {
		return errors.New("invalid manifest digest")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var current []byte
	if err = tx.QueryRow(ctx, `SELECT manifest_digest FROM worker_pools WHERE id=$1 FOR UPDATE`, id).Scan(&current); err != nil {
		return err
	}
	drift := !gatewayMapped || string(current) != string(expectedDigest)
	if drift {
		if _, err = tx.Exec(ctx, `UPDATE worker_pools SET enabled=false,lifecycle_state='failed',updated_at=CURRENT_TIMESTAMP WHERE id=$1`, id); err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, id, "reconcile", actor, reason); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,worker_pool_id,safe_details) VALUES('pool_drift',$1,$2)`, id, map[bool]string{true: "pool manifest drift", false: "pool manifest verified"}[drift]); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if drift {
		return errors.New("worker-pool drift detected")
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id, confirmation, actor, reason string) error {
	if confirmation != "delete:"+id {
		return errors.New("exact deletion confirmation required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var state string
	var jobs int
	if err = tx.QueryRow(ctx, `SELECT lifecycle_state,(SELECT count(*) FROM check_jobs WHERE worker_pool_id=$1 AND state IN('pending','pending_publish','published','running')) FROM worker_pools WHERE id=$1 FOR UPDATE`, id).Scan(&state, &jobs); err != nil {
		return err
	}
	if state != "deleting" || jobs != 0 {
		return errors.New("pool is not drained and deleting")
	}
	if err = audit(ctx, tx, id, "delete_complete", actor, reason); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM worker_pools WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func audit(ctx context.Context, tx pgx.Tx, id, action, actor, reason string) error {
	if len(actor) == 0 || len(actor) > 128 || len(reason) == 0 || len(reason) > 240 {
		return errors.New("invalid audit identity or reason")
	}
	_, err := tx.Exec(ctx, `INSERT INTO worker_pool_audit_events(worker_pool_id,action,actor,reason) VALUES($1,$2,$3,$4)`, id, action, actor, reason)
	return err
}
