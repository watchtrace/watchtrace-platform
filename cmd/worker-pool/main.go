package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/workerpool"
)

type publicBundle struct {
	PoolID              string `json:"pool_id"`
	EncryptionKeyID     string `json:"encryption_key_id"`
	ResultKeyID         string `json:"result_key_id"`
	EncryptionPublicKey string `json:"encryption_public_key"`
	ResultPublicKey     string `json:"result_public_key"`
}

func main() {
	mode := flag.String("mode", "generate", "generate, register, bootstrap-hosted, activate, drain, revoke, fail, reconcile, delete")
	pool := flag.String("pool", "", "worker pool ID")
	bundle := flag.String("bundle", "worker-pool-public.json", "public bundle path")
	prefix := flag.String("private-prefix", "worker-pool", "private key prefix")
	poolMode := flag.String("pool-mode", "customer_vpc", "hosted or customer_vpc")
	queueURL := flag.String("queue-url", "", "job FIFO URL")
	queueARN := flag.String("queue-arn", "", "job FIFO ARN")
	dlqURL := flag.String("dlq-url", "", "job DLQ URL")
	dlqARN := flag.String("dlq-arn", "", "job DLQ ARN")
	mtlsFingerprint := flag.String("mtls-fingerprint", "", "issued client certificate SHA-256 fingerprint")
	mtlsNotAfter := flag.String("mtls-not-after", "", "issued client certificate expiry (RFC3339)")
	manifest := flag.String("manifest", "", "verified deployment manifest")
	gatewayMapped := flag.Bool("gateway-mapped", false, "signed gateway mapping was verified")
	actor := flag.String("actor", "", "operator identity")
	reason := flag.String("reason", "", "audit reason")
	confirmation := flag.String("confirm", "", "exact deletion confirmation")
	sourceQueueEmpty := flag.Bool("source-queue-empty", false, "operator verified the source FIFO is empty")
	dlqEmpty := flag.Bool("dlq-empty", false, "operator verified the FIFO DLQ is empty")
	gatewayRemoved := flag.Bool("gateway-removed", false, "signed gateway mapping no longer contains the pool")
	flag.Parse()
	var err error
	if *mode == "generate" {
		err = generate(*pool, *bundle, *prefix)
	} else {
		err = operate(context.Background(), *mode, *pool, *poolMode, *queueURL, *queueARN, *dlqURL, *dlqARN, *mtlsFingerprint, *mtlsNotAfter, *bundle, *manifest, *gatewayMapped, *actor, *reason, *confirmation, workerpool.DeletionReadiness{SourceQueueEmpty: *sourceQueueEmpty, DeadLetterQueueEmpty: *dlqEmpty, GatewayMappingRemoved: *gatewayRemoved})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker-pool operation failed")
		os.Exit(1)
	}
}

func generate(pool, path, prefix string) error {
	if pool == "" {
		return errors.New("pool required")
	}
	encryption, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	resultPublic, resultPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err = os.WriteFile(prefix+"-encryption.key", []byte(base64.StdEncoding.EncodeToString(encryption.Bytes())), 0600); err != nil {
		return err
	}
	if err = os.WriteFile(prefix+"-result.key", []byte(base64.StdEncoding.EncodeToString(resultPrivate)), 0600); err != nil {
		return err
	}
	b := publicBundle{PoolID: pool, EncryptionKeyID: "enc-v1", ResultKeyID: "result-v1", EncryptionPublicKey: base64.StdEncoding.EncodeToString(encryption.PublicKey().Bytes()), ResultPublicKey: base64.StdEncoding.EncodeToString(resultPublic)}
	data, _ := json.MarshalIndent(b, "", "  ")
	return os.WriteFile(path, data, 0644)
}

func operate(ctx context.Context, mode, pool, poolMode, queueURL, queueARN, dlqURL, dlqARN, mtlsFingerprint, mtlsExpiry, bundlePath, manifestPath string, gatewayMapped bool, actor, reason, confirmation string, deletionReadiness workerpool.DeletionReadiness) error {
	if pool == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("pool, actor, and reason required")
	}
	url := strings.TrimSpace(os.Getenv("WATCHTRACE_DATABASE_URL"))
	if url == "" {
		return errors.New("database URL required")
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer db.Close()
	service := workerpool.New(db)
	switch mode {
	case "register":
		registration, e := registrationFromBundle(pool, poolMode, queueURL, queueARN, dlqURL, dlqARN, mtlsFingerprint, mtlsExpiry, bundlePath)
		if e != nil {
			return e
		}
		return service.Register(ctx, registration, actor, reason)
	case "bootstrap-hosted":
		queueURL = setting(queueURL, "WATCHTRACE_SQS_HOSTED_JOB_QUEUE_URL")
		dlqURL = setting(dlqURL, "WATCHTRACE_SQS_HOSTED_JOB_DLQ_URL")
		queueARN, region, account, e := sqsQueueARN(queueURL)
		if e != nil {
			return e
		}
		if configuredRegion := setting("", "AWS_REGION"); configuredRegion == "" || configuredRegion != region {
			return errors.New("AWS region does not match hosted job queue")
		}
		dlqARN, dlqRegion, dlqAccount, e := sqsQueueARN(dlqURL)
		if e != nil || dlqRegion != region || dlqAccount != account {
			return errors.New("hosted job queue and DLQ identities do not match")
		}
		for _, name := range []string{"WATCHTRACE_SQS_RESULT_QUEUE_URL", "WATCHTRACE_SQS_RESULT_DLQ_URL"} {
			_, resultRegion, resultAccount, resultErr := sqsQueueARN(setting("", name))
			if resultErr != nil || resultRegion != region || resultAccount != account {
				return errors.New("SQS queue identities do not share one AWS region and account")
			}
		}
		registration, e := registrationFromBundle("hosted", "hosted", queueURL, queueARN, dlqURL, dlqARN, "", "", bundlePath)
		if e != nil {
			return e
		}
		return service.BootstrapHosted(ctx, registration, actor, reason)
	case "activate", "reconcile":
		digest, e := manifestDigest(manifestPath)
		if e != nil {
			return e
		}
		if mode == "activate" {
			return service.Activate(ctx, pool, digest, gatewayMapped, actor, reason)
		}
		return service.Reconcile(ctx, pool, digest, gatewayMapped, actor, reason)
	case "drain":
		return service.Transition(ctx, pool, "draining", actor, reason)
	case "revoke":
		return service.Transition(ctx, pool, "revoked", actor, reason)
	case "fail":
		return service.Transition(ctx, pool, "failed", actor, reason)
	case "delete":
		return service.Delete(ctx, pool, confirmation, actor, reason, deletionReadiness)
	default:
		return errors.New("unknown mode")
	}
}

func registrationFromBundle(pool, poolMode, queueURL, queueARN, dlqURL, dlqARN, mtlsFingerprint, mtlsExpiry, bundlePath string) (workerpool.Registration, error) {
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return workerpool.Registration{}, err
	}
	var b publicBundle
	if json.Unmarshal(data, &b) != nil || b.PoolID != pool {
		return workerpool.Registration{}, errors.New("invalid public bundle")
	}
	enc, err := base64.StdEncoding.Strict().DecodeString(b.EncryptionPublicKey)
	if err != nil {
		return workerpool.Registration{}, errors.New("invalid worker encryption public key")
	}
	result, err := base64.StdEncoding.Strict().DecodeString(b.ResultPublicKey)
	if err != nil {
		return workerpool.Registration{}, errors.New("invalid worker result public key")
	}
	var notAfter time.Time
	if mtlsExpiry != "" {
		notAfter, err = time.Parse(time.RFC3339, mtlsExpiry)
		if err != nil {
			return workerpool.Registration{}, errors.New("invalid mTLS expiry")
		}
	}
	return workerpool.Registration{ID: pool, Mode: poolMode, JobQueueURL: queueURL, JobQueueARN: queueARN, JobDLQURL: dlqURL, JobDLQARN: dlqARN, EncryptionKeyID: b.EncryptionKeyID, ResultKeyID: b.ResultKeyID, EncryptionPublic: enc, ResultPublic: result, SchemaMin: 1, SchemaMax: 2, MTLSFingerprint: mtlsFingerprint, MTLSNotAfter: notAfter}, nil
}

func setting(value, name string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(os.Getenv(name))
}

func sqsQueueARN(raw string) (string, string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("invalid Amazon SQS queue URL")
	}
	host := strings.Split(parsed.Hostname(), ".")
	if len(host) != 4 || host[0] != "sqs" || host[1] == "" || host[2] != "amazonaws" || host[3] != "com" {
		return "", "", "", errors.New("invalid Amazon SQS queue host")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || len(parts[0]) != 12 || path.Base(parsed.Path) != parts[1] || !strings.HasSuffix(parts[1], ".fifo") {
		return "", "", "", errors.New("invalid Amazon SQS FIFO queue identity")
	}
	for _, digit := range parts[0] {
		if digit < '0' || digit > '9' {
			return "", "", "", errors.New("invalid Amazon SQS account ID")
		}
	}
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", host[1], parts[0], parts[1]), host[1], parts[0], nil
}

func manifestDigest(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}
