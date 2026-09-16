package fifo

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

const dlqVisibilityTimeoutSeconds int32 = 120

type DLQDelivery struct {
	Kind       string
	Body       []byte
	Attributes envelope.Attributes
	Receipt    string
}
type DLQSource interface {
	PullDLQ(context.Context, string, time.Duration) (DLQDelivery, error)
	AcknowledgeDLQ(context.Context, DLQDelivery) error
}

type DLQReconciler struct {
	db     DB
	source DLQSource
	sealer *quarantine.Sealer
}

func NewDLQReconciler(db DB, source DLQSource, sealer *quarantine.Sealer) (*DLQReconciler, error) {
	if db == nil || source == nil || sealer == nil {
		return nil, errors.New("fifo: DLQ database, source, and quarantine sealer are required")
	}
	return &DLQReconciler{db: db, source: source, sealer: sealer}, nil
}

func (r *DLQReconciler) ReconcileNext(ctx context.Context) (bool, error) {
	for _, kind := range []string{"job", "result"} {
		delivery, err := r.source.PullDLQ(ctx, kind, time.Second)
		if errors.Is(err, workqueue.ErrNoMessage) {
			continue
		}
		if err != nil {
			return false, err
		}
		if kind == "job" {
			if err = reconcileJobDLQ(ctx, r.db, delivery.Attributes.JobID, delivery.Attributes.WorkerPoolID); err != nil {
				return true, err
			}
			return true, r.source.AcknowledgeDLQ(ctx, delivery)
		}
		result, peekErr := envelope.PeekResult(delivery.Body)
		aad := "result:invalid"
		if peekErr == nil {
			aad = "result:" + result.ResultID
		}
		sealed, err := r.sealer.Seal(delivery.Body, []byte(aad))
		if err != nil {
			return true, err
		}
		tx, err := r.db.Begin(ctx)
		if err != nil {
			return true, err
		}
		defer tx.Rollback(context.Background())
		queries := database.New(tx)
		if peekErr != nil {
			err = queries.InsertInvalidResultQuarantine(ctx, sealed)
		} else {
			err = queries.InsertResultDLQQuarantine(ctx, database.InsertResultDLQQuarantineParams{JobID: result.JobID, ResultID: result.ResultID, WorkerPoolID: databaseText(result.WorkerPoolID), SnapshotHash: result.SnapshotHash, EncryptedPayload: sealed})
			if err == nil {
				err = queries.InsertMonitoringOperationalEvent(ctx, database.InsertMonitoringOperationalEventParams{EventType: "result_dlq", JobID: result.JobID, WorkerPoolID: result.WorkerPoolID, SafeDetails: "recoverable result quarantined"})
			}
		}
		if err != nil {
			return true, err
		}
		if err = tx.Commit(ctx); err != nil {
			return true, err
		}
		return true, r.source.AcknowledgeDLQ(ctx, delivery)
	}
	return false, nil
}

func reconcileJobDLQ(ctx context.Context, db DB, jobID, poolID string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	queries := database.New(tx)
	rowsAffected, err := queries.MarkCheckJobDead(ctx, database.MarkCheckJobDeadParams{JobID: jobID, WorkerPoolID: poolID})
	if err != nil {
		return err
	}
	if rowsAffected > 0 {
		_ = queries.InsertDeadCoverageGapFromJob(ctx, jobID)
		_ = queries.InsertMonitoringOperationalEvent(ctx, database.InsertMonitoringOperationalEventParams{EventType: "job_dlq", JobID: jobID, WorkerPoolID: poolID, SafeDetails: "job receive limit"})
	}
	return tx.Commit(ctx)
}

type SQSDLQSource struct {
	Client interface {
		ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
		DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	}
	JobDLQURL, ResultDLQURL string
}

func (s *SQSDLQSource) PullDLQ(ctx context.Context, kind string, wait time.Duration) (DLQDelivery, error) {
	url := s.JobDLQURL
	if kind == "result" {
		url = s.ResultDLQURL
	}
	if s.Client == nil || url == "" {
		return DLQDelivery{}, errors.New("invalid DLQ source")
	}
	seconds := int32(wait / time.Second)
	if seconds > 20 {
		seconds = 20
	}
	out, err := s.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(url),
		MaxNumberOfMessages:         1,
		VisibilityTimeout:           dlqVisibilityTimeoutSeconds,
		WaitTimeSeconds:             seconds,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return DLQDelivery{}, err
	}
	if len(out.Messages) == 0 {
		return DLQDelivery{}, workqueue.ErrNoMessage
	}
	m := out.Messages[0]
	attrs, err := workqueue.AttributesFromSQS(m.MessageAttributes)
	if err != nil {
		return DLQDelivery{}, err
	}
	body, err := base64.StdEncoding.DecodeString(aws.ToString(m.Body))
	if err != nil {
		return DLQDelivery{}, envelope.ErrInvalid
	}
	return DLQDelivery{Kind: kind, Body: body, Attributes: attrs, Receipt: aws.ToString(m.ReceiptHandle)}, nil
}
func (s *SQSDLQSource) AcknowledgeDLQ(ctx context.Context, d DLQDelivery) error {
	url := s.JobDLQURL
	if d.Kind == "result" {
		url = s.ResultDLQURL
	}
	_, err := s.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: aws.String(d.Receipt)})
	return err
}

func (c *ResultConsumer) RecordResultDLQ(ctx context.Context, jobID, poolID string) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = database.New(tx).InsertMonitoringOperationalEvent(ctx, database.InsertMonitoringOperationalEventParams{EventType: "result_dlq", JobID: jobID, WorkerPoolID: poolID, SafeDetails: "recoverable result requires redrive"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
