package operations

import (
	"context"
	"errors"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type SQSClient interface {
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

type QueueURLs struct {
	Jobs, Results, JobDLQ, ResultDLQ string
}

type QueueState struct {
	Configured bool  `json:"configured"`
	Available  int64 `json:"available"`
	InFlight   int64 `json:"in_flight"`
	Delayed    int64 `json:"delayed"`
}

type TransportHealth struct {
	Reachable bool       `json:"reachable"`
	Jobs      QueueState `json:"jobs"`
	Results   QueueState `json:"results"`
	JobDLQ    QueueState `json:"job_dlq"`
	ResultDLQ QueueState `json:"result_dlq"`
}

func ReadTransportHealth(ctx context.Context, client SQSClient, urls QueueURLs) (TransportHealth, error) {
	health := TransportHealth{}
	if client == nil {
		return health, nil
	}
	var errs []error
	for _, item := range []struct {
		url   string
		state *QueueState
	}{{urls.Jobs, &health.Jobs}, {urls.Results, &health.Results}, {urls.JobDLQ, &health.JobDLQ}, {urls.ResultDLQ, &health.ResultDLQ}} {
		if item.url == "" {
			continue
		}
		item.state.Configured = true
		output, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: &item.url, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible, types.QueueAttributeNameApproximateNumberOfMessagesDelayed}})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		item.state.Available = parseCount(output.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
		item.state.InFlight = parseCount(output.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
		item.state.Delayed = parseCount(output.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesDelayed)])
	}
	health.Reachable = len(errs) == 0
	return health, errors.Join(errs...)
}

func parseCount(value string) int64 {
	count, err := strconv.ParseInt(value, 10, 64)
	if err != nil || count < 0 {
		return 0
	}
	return count
}
