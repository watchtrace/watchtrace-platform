// Package workerpool owns the operator-only worker-pool lifecycle.
package workerpool

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
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

type DeletionReadiness struct {
	SourceQueueEmpty      bool
	DeadLetterQueueEmpty  bool
	GatewayMappingRemoved bool
}

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
	queries := database.New(tx)
	err = queries.RegisterWorkerPool(ctx, database.RegisterWorkerPoolParams{
		ID: r.ID, Mode: r.Mode, SchemaMin: int16(r.SchemaMin), SchemaMax: int16(r.SchemaMax),
		EncryptionKeyID: text(r.EncryptionKeyID), EncryptionPublicKey: r.EncryptionPublic,
		ResultKeyID: text(r.ResultKeyID), ResultPublicKey: r.ResultPublic,
		JobQueueUrl: text(r.JobQueueURL), JobQueueArn: text(r.JobQueueARN),
		JobDlqUrl: text(r.JobDLQURL), JobDlqArn: text(r.JobDLQARN),
	})
	if err != nil {
		return fmt.Errorf("register pool: %w", err)
	}
	for _, credential := range []struct {
		purpose, keyID string
		public         []byte
	}{{"job_encryption", r.EncryptionKeyID, r.EncryptionPublic}, {"result_signing", r.ResultKeyID, r.ResultPublic}} {
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(credential.public))
		if err = queries.InsertWorkerPoolCredential(ctx, database.InsertWorkerPoolCredentialParams{WorkerPoolID: r.ID, Purpose: credential.purpose, KeyID: credential.keyID, PublicMaterial: credential.public, Fingerprint: fingerprint}); err != nil {
			return err
		}
	}
	if r.Mode == "customer_vpc" {
		if err = queries.InsertWorkerPoolMTLSCredential(ctx, database.InsertWorkerPoolMTLSCredentialParams{WorkerPoolID: r.ID, KeyID: "mtls-" + r.EncryptionKeyID, Fingerprint: r.MTLSFingerprint, NotAfter: timestamp(r.MTLSNotAfter)}); err != nil {
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
	queries := database.New(tx)
	ready, err := queries.LockWorkerPoolActivationReadiness(ctx, id)
	if err != nil || !ready.Valid || !ready.Bool {
		if err == nil {
			err = errors.New("pool is incomplete")
		}
		return err
	}
	if err = queries.ActivateWorkerPool(ctx, database.ActivateWorkerPoolParams{ID: id, ManifestDigest: manifestDigest}); err != nil {
		return err
	}
	if err = queries.ActivatePendingWorkerPoolCredentials(ctx, id); err != nil {
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
	queries := database.New(tx)
	current, err := queries.LockWorkerPoolLifecycleState(ctx, id)
	if err != nil {
		return err
	}
	allowed := map[string]map[string]bool{"active": {"draining": true, "revoked": true, "failed": true}, "provisioning": {"failed": true, "revoked": true}, "draining": {"revoked": true, "deleting": true}, "revoked": {"deleting": true}, "failed": {"deleting": true}}[current][target]
	if !allowed {
		return errors.New("invalid lifecycle transition")
	}
	err = queries.UpdateWorkerPoolLifecycle(ctx, database.UpdateWorkerPoolLifecycleParams{ID: id, LifecycleState: target})
	if err != nil {
		return err
	}
	if target == "revoked" || target == "deleting" {
		err = queries.RevokeWorkerPoolCredentials(ctx, id)
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
	queries := database.New(tx)
	current, err := queries.LockWorkerPoolManifestDigest(ctx, id)
	if err != nil {
		return err
	}
	drift := !gatewayMapped || string(current) != string(expectedDigest)
	if drift {
		if err = queries.FailWorkerPool(ctx, id); err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, id, "reconcile", actor, reason); err != nil {
		return err
	}
	if err = queries.InsertWorkerPoolDriftEvent(ctx, database.InsertWorkerPoolDriftEventParams{WorkerPoolID: text(id), SafeDetails: map[bool]string{true: "pool manifest drift", false: "pool manifest verified"}[drift]}); err != nil {
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

func (s *Service) Delete(ctx context.Context, id, confirmation, actor, reason string, readiness DeletionReadiness) error {
	if confirmation != "delete:"+id || !readiness.SourceQueueEmpty || !readiness.DeadLetterQueueEmpty || !readiness.GatewayMappingRemoved {
		return errors.New("exact deletion confirmation required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	readinessState, err := queries.LockWorkerPoolDeletionState(ctx, id)
	if err != nil {
		return err
	}
	if readinessState.LifecycleState != "deleting" || readinessState.Jobs != 0 || readinessState.TrustedCredentials != 0 {
		return errors.New("pool is not drained and deleting")
	}
	if err = audit(ctx, tx, id, "delete_complete", actor, reason); err != nil {
		return err
	}
	if err = queries.DeleteWorkerPool(ctx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func audit(ctx context.Context, tx pgx.Tx, id, action, actor, reason string) error {
	if len(actor) == 0 || len(actor) > 128 || len(reason) == 0 || len(reason) > 240 {
		return errors.New("invalid audit identity or reason")
	}
	return database.New(tx).InsertWorkerPoolAudit(ctx, database.InsertWorkerPoolAuditParams{WorkerPoolID: id, Action: action, Actor: actor, Reason: reason})
}

func text(value string) pgtype.Text { return pgtype.Text{String: value, Valid: true} }

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
