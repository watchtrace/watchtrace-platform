package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
)

func main() {
	id := flag.String("id", "", "quarantine record UUID")
	execute := flag.Bool("execute", false, "perform the reviewed redrive")
	approver := flag.String("approver", "", "operator identity")
	reason := flag.String("reason", "", "reviewed reason")
	flag.Parse()
	if err := run(context.Background(), *id, *execute, *approver, *reason); err != nil {
		fmt.Fprintln(os.Stderr, "queue recovery failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, id string, execute bool, approver, reason string) error {
	if id == "" || approver == "" || reason == "" || len(approver) > 128 || len(reason) > 240 {
		return errors.New("id, approver, and reason are required")
	}
	databaseURL := strings.TrimSpace(os.Getenv("WATCHTRACE_DATABASE_URL"))
	keyPath := strings.TrimSpace(os.Getenv("WATCHTRACE_QUARANTINE_KEY"))
	if databaseURL == "" || keyPath == "" {
		return errors.New("database and quarantine key are required")
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyData)))
	if err != nil {
		return errors.New("invalid quarantine key")
	}
	sealer, err := quarantine.New(key)
	if err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	quarantineID, err := databaseUUID(id)
	if err != nil {
		return err
	}
	queries := database.New(db)
	record, err := queries.GetQuarantineRecord(ctx, quarantineID)
	if err != nil {
		return err
	}
	kind, jobID, resultID, poolID := record.QueueKind, record.JobID, record.ResultID, record.WorkerPoolID
	if record.Status != "quarantined" || record.RedriveCount >= 3 || time.Now().UTC().After(record.ExpiresAt.Time) {
		return errors.New("quarantine record is not recoverable")
	}
	if kind == "job" {
		return errors.New("expired or dead jobs are never executed by redrive")
	}
	if resultID == "" || jobID == "" {
		return errors.New("result identity is unavailable")
	}
	plain, err := sealer.Open(record.EncryptedPayload, []byte("result:"+resultID))
	if err != nil {
		return err
	}
	result, err := envelope.PeekResult(plain)
	if err != nil || result.ResultID != resultID || result.JobID != jobID || result.WorkerPoolID != poolID {
		return envelope.ErrInvalid
	}
	jobUUID, err := databaseUUID(jobID)
	if err != nil {
		return err
	}
	verification, err := queries.GetRecoveryResultVerification(ctx, database.GetRecoveryResultVerificationParams{Column1: jobUUID, KeyID: result.ResultKeyID, WorkerPoolID: poolID})
	if err != nil {
		return err
	}
	if _, err = envelope.VerifyResult(plain, ed25519.PublicKey(verification.PublicKey)); err != nil {
		return err
	}
	if verification.State == "completed" { /* idempotent replay remains safe */
	}
	fmt.Printf("quarantine %s: result %s for job %s is recoverable (dry_run=%t)\n", id, resultID, jobID, !execute)
	if !execute {
		return nil
	}
	queueURL := strings.TrimSpace(os.Getenv("WATCHTRACE_SQS_RESULT_QUEUE_URL"))
	if queueURL == "" {
		return errors.New("result queue URL required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		if endpoint := strings.TrimSpace(os.Getenv("WATCHTRACE_SQS_ENDPOINT")); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	attrs := map[string]types.MessageAttributeValue{"schema_version": {DataType: aws.String("Number"), StringValue: aws.String(fmt.Sprint(result.SchemaVersion))}, "job_id": {DataType: aws.String("String"), StringValue: aws.String(jobID)}, "worker_pool_id": {DataType: aws.String("String"), StringValue: aws.String(poolID)}, "snapshot_hash": {DataType: aws.String("String"), StringValue: aws.String(result.SnapshotHash)}, "result_id": {DataType: aws.String("String"), StringValue: aws.String(resultID)}, "result_key_id": {DataType: aws.String("String"), StringValue: aws.String(result.ResultKeyID)}}
	wireBody := base64.StdEncoding.EncodeToString(plain)
	if _, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queueURL), MessageBody: aws.String(wireBody), MessageDeduplicationId: aws.String(resultID), MessageGroupId: aws.String(jobID), MessageAttributes: attrs}); err != nil {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	txQueries := database.New(tx)
	rowsAffected, err := txQueries.MarkQuarantineRedriven(ctx, database.MarkQuarantineRedrivenParams{Column1: quarantineID, Approver: databaseText(approver), RedriveReason: databaseText(reason)})
	if err != nil || rowsAffected != 1 {
		return errors.New("quarantine state changed")
	}
	if err = txQueries.InsertQueueRecoveryAudit(ctx, database.InsertQueueRecoveryAuditParams{WorkerPoolID: poolID, Actor: approver, Reason: reason, Column4: id}); err != nil {
		return err
	}
	if err = txQueries.InsertQueueRecoveryOperationalEvent(ctx, database.InsertQueueRecoveryOperationalEventParams{Column1: jobUUID, WorkerPoolID: databaseText(poolID)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func databaseUUID(value string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, nil
}

func databaseText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: true}
}
