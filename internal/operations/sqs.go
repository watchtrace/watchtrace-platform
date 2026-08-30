package operations

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type SQSClient interface {
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

type QueueURLs struct {
	Jobs, Results, JobDLQ, ResultDLQ string
}

func (q QueueURLs) Validate() error {
	queues := []struct {
		name string
		url  string
		dlq  bool
	}{
		{"jobs", q.Jobs, false},
		{"results", q.Results, false},
		{"job DLQ", q.JobDLQ, true},
		{"result DLQ", q.ResultDLQ, true},
	}
	seen := make(map[string]string, len(queues))
	for _, queue := range queues {
		if queue.url == "" {
			continue
		}
		if previous, exists := seen[queue.url]; exists {
			return fmt.Errorf("%s and %s must use distinct SQS queue URLs", previous, queue.name)
		}
		seen[queue.url] = queue.name
		parsed, err := url.Parse(queue.url)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s SQS queue URL is invalid", queue.name)
		}
		isDLQ := strings.HasSuffix(path.Base(strings.TrimSuffix(parsed.Path, "/")), "-dlq.fifo")
		if isDLQ != queue.dlq {
			return fmt.Errorf("%s SQS queue URL does not identify the expected queue type", queue.name)
		}
	}
	return nil
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
