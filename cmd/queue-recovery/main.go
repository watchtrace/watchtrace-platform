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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
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
	var kind, jobID, resultID, poolID, status string
	var encrypted []byte
	var count int
	var expires time.Time
	err = db.QueryRow(ctx, `SELECT queue_kind,COALESCE(job_id::text,''),COALESCE(result_id::text,''),COALESCE(worker_pool_id,''),status,encrypted_payload,redrive_count,expires_at FROM monitoring_quarantine WHERE id=$1::uuid`, id).Scan(&kind, &jobID, &resultID, &poolID, &status, &encrypted, &count, &expires)
	if err != nil {
		return err
	}
	if status != "quarantined" || count >= 3 || time.Now().UTC().After(expires) {
		return errors.New("quarantine record is not recoverable")
	}
	if kind == "job" {
		return errors.New("expired or dead jobs are never executed by redrive")
	}
	if resultID == "" || jobID == "" {
		return errors.New("result identity is unavailable")
	}
	plain, err := sealer.Open(encrypted, []byte("result:"+resultID))
	if err != nil {
		return err
	}
	result, err := envelope.PeekResult(plain)
	if err != nil || result.ResultID != resultID || result.JobID != jobID || result.WorkerPoolID != poolID {
		return envelope.ErrInvalid
	}
	var public []byte
	var state string
	err = db.QueryRow(ctx, `SELECT COALESCE((SELECT public_material FROM worker_pool_credentials WHERE worker_pool_id=j.worker_pool_id AND purpose='result_signing' AND key_id=$2 AND status IN('active','retired') ORDER BY activates_at DESC LIMIT 1),wp.result_public_key),j.state FROM check_jobs j JOIN worker_pools wp ON wp.id=j.worker_pool_id WHERE j.id=$1::uuid AND j.worker_pool_id=$3`, jobID, result.ResultKeyID, poolID).Scan(&public, &state)
	if err != nil {
		return err
	}
	if _, err = envelope.VerifyResult(plain, ed25519.PublicKey(public)); err != nil {
		return err
	}
	if state == "completed" { /* idempotent replay remains safe */
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
	tag, err := tx.Exec(ctx, `UPDATE monitoring_quarantine SET status='redriven',redrive_count=redrive_count+1,approver=$2,redrive_reason=$3,reviewed_at=CURRENT_TIMESTAMP WHERE id=$1::uuid AND status='quarantined' AND redrive_count<3`, id, approver, reason)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("quarantine state changed")
	}
	_, err = tx.Exec(ctx, `INSERT INTO worker_pool_audit_events(worker_pool_id,action,actor,reason,safe_details) VALUES($1,'redrive',$2,$3,jsonb_build_object('quarantine_id',$4::text,'count',1))`, poolID, approver, reason, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('redrive',$1::uuid,$2,'controlled result redrive')`, jobID, poolID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
