package fifo

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"strconv"
	"time"
)

type SQSSender struct {
	Client interface {
		SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	}
}

func (s SQSSender) Send(ctx context.Context, in SendInput) (string, error) {
	out, err := s.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(in.QueueURL), MessageBody: aws.String(string(in.Body)), MessageDeduplicationId: aws.String(in.DeduplicationID), MessageGroupId: aws.String(in.GroupID), MessageAttributes: map[string]types.MessageAttributeValue{"schema_version": {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(in.Attributes.SchemaVersion))}, "job_id": {DataType: aws.String("String"), StringValue: aws.String(in.Attributes.JobID)}, "worker_pool_id": {DataType: aws.String("String"), StringValue: aws.String(in.Attributes.WorkerPoolID)}, "snapshot_hash": {DataType: aws.String("String"), StringValue: aws.String(in.Attributes.SnapshotHash)}, "expires_at": {DataType: aws.String("String"), StringValue: aws.String(in.Attributes.ExpiresAt.Format(time.RFC3339Nano))}, "platform_key_id": {DataType: aws.String("String"), StringValue: aws.String(in.Attributes.PlatformKeyID)}, "worker_encryption_key_id": {DataType: aws.String("String"), StringValue: aws.String(in.Attributes.WorkerEncryptionKeyID)}}})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.MessageId), nil
}
