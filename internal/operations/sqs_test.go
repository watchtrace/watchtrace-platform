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

func TestQueueURLsValidateDistinctQueueRoles(t *testing.T) {
	valid := QueueURLs{
		Jobs:      "https://sqs.eu-north-1.amazonaws.com/123/watchtrace-prod-check-jobs-hosted.fifo",
		Results:   "https://sqs.eu-north-1.amazonaws.com/123/watchtrace-prod-check-results.fifo",
		JobDLQ:    "https://sqs.eu-north-1.amazonaws.com/123/watchtrace-prod-check-jobs-hosted-dlq.fifo",
		ResultDLQ: "https://sqs.eu-north-1.amazonaws.com/123/watchtrace-prod-check-results-dlq.fifo",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	duplicate := valid
	duplicate.ResultDLQ = duplicate.Results
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate queue URL was accepted")
	}
	swapped := valid
	swapped.Results, swapped.ResultDLQ = swapped.ResultDLQ, swapped.Results
	if err := swapped.Validate(); err == nil {
		t.Fatal("swapped source and DLQ URLs were accepted")
	}
}
