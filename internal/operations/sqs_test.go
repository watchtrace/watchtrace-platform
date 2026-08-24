package operations

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type queueHealthFake struct {
	fail string
}

func (f queueHealthFake) GetQueueAttributes(_ context.Context, input *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if input.QueueUrl != nil && *input.QueueUrl == f.fail {
		return nil, errors.New("queue unavailable")
	}
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{"ApproximateNumberOfMessages": "7", "ApproximateNumberOfMessagesNotVisible": "2", "ApproximateNumberOfMessagesDelayed": "1"}}, nil
}

func TestReadTransportHealthIsBoundedAndReportsAllQueues(t *testing.T) {
	health, err := ReadTransportHealth(context.Background(), queueHealthFake{}, QueueURLs{Jobs: "jobs", Results: "results", JobDLQ: "job-dlq", ResultDLQ: "result-dlq"})
	if err != nil {
		t.Fatal(err)
	}
	if !health.Reachable || health.Jobs.Available != 7 || health.Results.InFlight != 2 || health.JobDLQ.Delayed != 1 || !health.ResultDLQ.Configured {
		t.Fatalf("unexpected health: %+v", health)
	}
}

func TestReadTransportHealthDoesNotHidePartialFailure(t *testing.T) {
	health, err := ReadTransportHealth(context.Background(), queueHealthFake{fail: "results"}, QueueURLs{Jobs: "jobs", Results: "results"})
	if err == nil || health.Reachable || health.Jobs.Available != 7 || !health.Results.Configured {
		t.Fatalf("health=%+v err=%v", health, err)
	}
}
