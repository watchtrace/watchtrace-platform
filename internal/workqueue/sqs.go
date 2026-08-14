package workqueue

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
)

type SQSAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type DirectSQS struct {
	Client                                    SQSAPI
	JobQueueURL, ResultQueueURL, WorkerPoolID string
}

func (d *DirectSQS) Pull(ctx context.Context, wait time.Duration) (Delivery, error) {
	if d.Client == nil || d.JobQueueURL == "" {
		return Delivery{}, errors.New("invalid SQS transport")
	}
	seconds := int32(wait / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	if seconds > 20 {
		seconds = 20
	}
	out, err := d.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(d.JobQueueURL), MaxNumberOfMessages: 1, WaitTimeSeconds: seconds, MessageAttributeNames: []string{"All"}, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
	if err != nil {
		return Delivery{}, err
	}
	if len(out.Messages) == 0 {
		return Delivery{}, ErrNoMessage
	}
	m := out.Messages[0]
	attrs, err := attributesFromSQS(m.MessageAttributes)
	if err != nil {
		return Delivery{}, err
	}
	if attrs.WorkerPoolID != d.WorkerPoolID {
		return Delivery{}, errors.New("wrong worker pool delivery")
	}
	body, err := base64.StdEncoding.DecodeString(aws.ToString(m.Body))
	if err != nil {
		return Delivery{}, envelope.ErrInvalid
	}
	count, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	return Delivery{Body: body, Attributes: attrs, LeaseToken: aws.ToString(m.ReceiptHandle), ReceiveCount: count}, nil
}
func (d *DirectSQS) Extend(ctx context.Context, msg Delivery, duration time.Duration) error {
	_, err := d.Client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(d.JobQueueURL), ReceiptHandle: aws.String(msg.LeaseToken), VisibilityTimeout: int32(duration / time.Second)})
	return err
}
func (d *DirectSQS) PublishResultAndAcknowledge(ctx context.Context, msg Delivery, result []byte) error {
	parsed, err := envelope.PeekResult(result)
	if err != nil || parsed.JobID != msg.Attributes.JobID || parsed.SnapshotHash != msg.Attributes.SnapshotHash || parsed.WorkerPoolID != msg.Attributes.WorkerPoolID {
		return envelope.ErrInvalid
	}
	attrs := envelope.ResultAttributes(parsed)
	wireBody := base64.StdEncoding.EncodeToString(result)
	if _, err := d.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(d.ResultQueueURL), MessageBody: aws.String(wireBody), MessageDeduplicationId: aws.String(parsed.ResultID), MessageGroupId: aws.String(parsed.JobID), MessageAttributes: attributesToSQS(attrs)}); err != nil {
		return err
	}
	_, err = d.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(d.JobQueueURL), ReceiptHandle: aws.String(msg.LeaseToken)})
	return err
}
func (d *DirectSQS) AcknowledgeExpired(ctx context.Context, msg Delivery, _ []byte) error {
	_, err := d.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(d.JobQueueURL), ReceiptHandle: aws.String(msg.LeaseToken)})
	return err
}
func attributesToSQS(a envelope.Attributes) map[string]types.MessageAttributeValue {
	result := map[string]types.MessageAttributeValue{"schema_version": {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(a.SchemaVersion))}, "job_id": {DataType: aws.String("String"), StringValue: aws.String(a.JobID)}, "worker_pool_id": {DataType: aws.String("String"), StringValue: aws.String(a.WorkerPoolID)}, "snapshot_hash": {DataType: aws.String("String"), StringValue: aws.String(a.SnapshotHash)}}
	if !a.ExpiresAt.IsZero() {
		result["expires_at"] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(a.ExpiresAt.UTC().Format(time.RFC3339Nano))}
	}
	if a.ResultID != "" {
		result["result_id"] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(a.ResultID)}
		result["result_key_id"] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(a.ResultKeyID)}
	}
	if a.PlatformKeyID != "" {
		result["platform_key_id"] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(a.PlatformKeyID)}
		result["worker_encryption_key_id"] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(a.WorkerEncryptionKeyID)}
	}
	return result
}
func attributesFromSQS(m map[string]types.MessageAttributeValue) (envelope.Attributes, error) {
	get := func(k string) string { return aws.ToString(m[k].StringValue) }
	version, err := strconv.Atoi(get("schema_version"))
	if err != nil {
		return envelope.Attributes{}, err
	}
	var expiry time.Time
	if raw := get("expires_at"); raw != "" {
		expiry, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return envelope.Attributes{}, err
		}
	}
	a := envelope.Attributes{SchemaVersion: version, JobID: get("job_id"), WorkerPoolID: get("worker_pool_id"), SnapshotHash: get("snapshot_hash"), ExpiresAt: expiry, ResultID: get("result_id"), ResultKeyID: get("result_key_id"), PlatformKeyID: get("platform_key_id"), WorkerEncryptionKeyID: get("worker_encryption_key_id")}
	if a.JobID == "" || a.WorkerPoolID == "" || len(a.SnapshotHash) != 64 {
		return envelope.Attributes{}, errors.New("invalid message attributes")
	}
	if (a.ResultID == "") != (a.ResultKeyID == "") {
		return envelope.Attributes{}, errors.New("invalid result message attributes")
	}
	if (a.PlatformKeyID == "") != (a.WorkerEncryptionKeyID == "") {
		return envelope.Attributes{}, errors.New("invalid job key attributes")
	}
	return a, nil
}

// AttributesFromSQS validates the bounded non-secret routing attributes.
func AttributesFromSQS(values map[string]types.MessageAttributeValue) (envelope.Attributes, error) {
	return attributesFromSQS(values)
}
