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
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

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
	consumer *ResultConsumer
	source   DLQSource
	sealer   *quarantine.Sealer
}

func NewDLQReconciler(db DB, source DLQSource, sealer *quarantine.Sealer) *DLQReconciler {
	return &DLQReconciler{consumer: NewResultConsumer(db, nil), source: source, sealer: sealer}
}

func (r *DLQReconciler) ReconcileNext(ctx context.Context) (bool, error) {
	if r.source == nil || r.sealer == nil {
		return false, errors.New("invalid DLQ reconciler")
	}
	for _, kind := range []string{"job", "result"} {
		delivery, err := r.source.PullDLQ(ctx, kind, time.Second)
		if errors.Is(err, workqueue.ErrNoMessage) {
			continue
		}
		if err != nil {
			return false, err
		}
		if kind == "job" {
			if err = r.consumer.ReconcileJobDLQ(ctx, delivery.Attributes.JobID, delivery.Attributes.WorkerPoolID); err != nil {
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
		tx, err := r.consumer.db.Begin(ctx)
		if err != nil {
			return true, err
		}
		defer tx.Rollback(context.Background())
		if peekErr != nil {
			_, err = tx.Exec(ctx, `INSERT INTO monitoring_quarantine(queue_kind,safe_reason,encrypted_payload) VALUES('result','invalid result DLQ envelope',$1)`, sealed)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO monitoring_quarantine(queue_kind,job_id,result_id,worker_pool_id,snapshot_hash,safe_reason,encrypted_payload) VALUES('result',$1::uuid,$2::uuid,$3,decode($4,'hex'),'result DLQ recovery',$5) ON CONFLICT DO NOTHING`, result.JobID, result.ResultID, result.WorkerPoolID, result.SnapshotHash, sealed)
			if err == nil {
				_, err = tx.Exec(ctx, `INSERT INTO monitoring_operational_events(event_type,job_id,worker_pool_id,safe_details) VALUES('result_dlq',$1::uuid,$2,'recoverable result quarantined')`, result.JobID, result.WorkerPoolID)
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
	out, err := s.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(url), MaxNumberOfMessages: 1, WaitTimeSeconds: seconds, MessageAttributeNames: []string{"All"}, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
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
