package fifo

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type visibilitySQS struct {
	receiveInput *sqs.ReceiveMessageInput
	message      types.Message
}

func (f *visibilitySQS) ReceiveMessage(_ context.Context, input *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.receiveInput = input
	return &sqs.ReceiveMessageOutput{Messages: []types.Message{f.message}}, nil
}

func (*visibilitySQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return &sqs.DeleteMessageOutput{}, nil
}

func TestSQSReceivesRequestProcessingVisibilityLease(t *testing.T) {
	attributes := map[string]types.MessageAttributeValue{
		"schema_version": {DataType: aws.String("Number"), StringValue: aws.String("1")},
		"job_id":         {DataType: aws.String("String"), StringValue: aws.String("job")},
		"worker_pool_id": {DataType: aws.String("String"), StringValue: aws.String("hosted")},
		"snapshot_hash":  {DataType: aws.String("String"), StringValue: aws.String("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		"result_id":      {DataType: aws.String("String"), StringValue: aws.String("result")},
		"result_key_id":  {DataType: aws.String("String"), StringValue: aws.String("result-v1")},
	}
	client := &visibilitySQS{message: types.Message{
		Body:              aws.String(base64.StdEncoding.EncodeToString([]byte("payload"))),
		ReceiptHandle:     aws.String("receipt"),
		MessageAttributes: attributes,
	}}
	if _, err := (ResultSQS{Client: client, QueueURL: "results"}).PullResult(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if client.receiveInput.VisibilityTimeout != resultVisibilityTimeoutSeconds {
		t.Fatalf("result visibility timeout=%d", client.receiveInput.VisibilityTimeout)
	}
	if _, err := (&SQSDLQSource{Client: client, JobDLQURL: "job-dlq", ResultDLQURL: "result-dlq"}).PullDLQ(context.Background(), "result", time.Second); err != nil {
		t.Fatal(err)
	}
	if client.receiveInput.VisibilityTimeout != dlqVisibilityTimeoutSeconds {
		t.Fatalf("DLQ visibility timeout=%d", client.receiveInput.VisibilityTimeout)
	}
}
