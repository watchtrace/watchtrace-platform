package fifo

import (
	"context"
	"encoding/base64"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

const resultVisibilityTimeoutSeconds int32 = 120

type ResultSQS struct {
	Client interface {
		ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
		DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	}
	QueueURL string
}

func (s ResultSQS) PullResult(ctx context.Context, wait time.Duration) (ResultDelivery, error) {
	out, err := s.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(s.QueueURL),
		MaxNumberOfMessages:         1,
		VisibilityTimeout:           resultVisibilityTimeoutSeconds,
		WaitTimeSeconds:             int32(wait / time.Second),
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return ResultDelivery{}, err
	}
	if len(out.Messages) == 0 {
		return ResultDelivery{}, workqueue.ErrNoMessage
	}
	m := out.Messages[0]
	attributes, err := workqueue.AttributesFromSQS(m.MessageAttributes)
	if err != nil || attributes.ResultID == "" {
		return ResultDelivery{}, envelope.ErrInvalid
	}
	body, err := base64.StdEncoding.DecodeString(aws.ToString(m.Body))
	if err != nil {
		return ResultDelivery{}, envelope.ErrInvalid
	}
	count, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	return ResultDelivery{Body: body, Attributes: attributes, Receipt: aws.ToString(m.ReceiptHandle), ReceiveCount: count}, nil
}
func (s ResultSQS) AcknowledgeResult(ctx context.Context, d ResultDelivery) error {
	_, err := s.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(s.QueueURL), ReceiptHandle: aws.String(d.Receipt)})
	return err
}
