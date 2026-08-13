package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/watchtrace/watchtrace-platform/internal/deploymentmanifest"
)

func TestLocalSQSHasProductionFIFOAndRedeliverySemantics(t *testing.T) {
	endpoint := os.Getenv("WATCHTRACE_TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("WATCHTRACE_TEST_SQS_ENDPOINT is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("local", "local", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) })
	prefix := "watchtrace-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	jobDLQ := createFIFOQueue(t, ctx, client, prefix+"-job-dlq.fifo", map[string]string{"MessageRetentionPeriod": "1209600", "SqsManagedSseEnabled": "true"})
	resultDLQ := createFIFOQueue(t, ctx, client, prefix+"-result-dlq.fifo", map[string]string{"MessageRetentionPeriod": "1209600", "SqsManagedSseEnabled": "true"})
	t.Cleanup(func() {
		for _, queueURL := range []string{jobDLQ.url, resultDLQ.url} {
			_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
		}
	})
	jobRedrive, _ := json.Marshal(map[string]any{"deadLetterTargetArn": jobDLQ.arn, "maxReceiveCount": 5})
	resultRedrive, _ := json.Marshal(map[string]any{"deadLetterTargetArn": resultDLQ.arn, "maxReceiveCount": 10})
	jobs := createFIFOQueue(t, ctx, client, prefix+"-jobs.fifo", map[string]string{"ContentBasedDeduplication": "false", "ReceiveMessageWaitTimeSeconds": "20", "VisibilityTimeout": "90", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "RedrivePolicy": string(jobRedrive)})
	results := createFIFOQueue(t, ctx, client, prefix+"-results.fifo", map[string]string{"ContentBasedDeduplication": "false", "ReceiveMessageWaitTimeSeconds": "20", "VisibilityTimeout": "60", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "RedrivePolicy": string(resultRedrive)})
	t.Cleanup(func() {
		for _, queueURL := range []string{jobs.url, results.url} {
			_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
		}
	})
	attributes, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(jobs.url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll}})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false", "ReceiveMessageWaitTimeSeconds": "20", "VisibilityTimeout": "90", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true"} {
		if got := attributes.Attributes[key]; got != want {
			t.Fatalf("job queue %s = %q, want %q", key, got, want)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(jobs.url), MessageBody: aws.String("immutable-body"), MessageDeduplicationId: aws.String("stable-job-id"), MessageGroupId: aws.String("stable-job-id")}); err != nil {
			t.Fatal(err)
		}
	}
	received, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(jobs.url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
	if err != nil || len(received.Messages) != 1 {
		t.Fatalf("deduplicated receives=%d error=%v", len(received.Messages), err)
	}
	if _, err = client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(jobs.url), ReceiptHandle: received.Messages[0].ReceiptHandle, VisibilityTimeout: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	redelivered, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(jobs.url), MaxNumberOfMessages: 1, WaitTimeSeconds: 1, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
	if err != nil || len(redelivered.Messages) != 1 || redelivered.Messages[0].Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)] != "2" {
		t.Fatalf("visibility redelivery=%+v error=%v", redelivered.Messages, err)
	}
	if _, err = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(jobs.url), ReceiptHandle: redelivered.Messages[0].ReceiptHandle}); err != nil {
		t.Fatal(err)
	}
	verifyMovesToDLQ(t, ctx, client, jobs.url, jobDLQ.url, "poison-job", 5)
	verifyMovesToDLQ(t, ctx, client, results.url, resultDLQ.url, "poison-result", 10)
}

func verifyMovesToDLQ(t *testing.T, ctx context.Context, client *sqs.Client, sourceURL, dlqURL, id string, maxReceives int) {
	t.Helper()
	_, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(sourceURL), MessageBody: aws.String(id), MessageDeduplicationId: aws.String(id), MessageGroupId: aws.String(id)})
	if err != nil {
		t.Fatal(err)
	}
	receives := 0
	deadlineSource := time.Now().Add(30 * time.Second)
	for receives <= maxReceives && time.Now().Before(deadlineSource) {
		received, e := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(sourceURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 1})
		if e != nil {
			t.Fatal(e)
		}
		if len(received.Messages) == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		receives++
		_, e = client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(sourceURL), ReceiptHandle: received.Messages[0].ReceiptHandle, VisibilityTimeout: 0})
		if e != nil {
			t.Fatal(e)
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		received, e := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(dlqURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 1})
		if e != nil {
			t.Fatal(e)
		}
		if len(received.Messages) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not move to DLQ", id)
}

func TestLocalSQSDeploymentManifestVerification(t *testing.T) {
	endpoint := os.Getenv("WATCHTRACE_TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("WATCHTRACE_TEST_SQS_ENDPOINT is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("ap-south-1"), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) })
	environment := "verify-" + strconv.FormatInt(time.Now().Unix()%100000000, 10)
	prefix := "watchtrace-" + environment + "-"
	jobDLQ := createFIFOQueue(t, ctx, client, prefix+"jobs-dlq.fifo", map[string]string{"VisibilityTimeout": "0", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "1209600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false"})
	resultDLQ := createFIFOQueue(t, ctx, client, prefix+"results-dlq.fifo", map[string]string{"VisibilityTimeout": "0", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "1209600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false"})
	jobRedrive, _ := json.Marshal(map[string]any{"deadLetterTargetArn": jobDLQ.arn, "maxReceiveCount": 5})
	resultRedrive, _ := json.Marshal(map[string]any{"deadLetterTargetArn": resultDLQ.arn, "maxReceiveCount": 10})
	jobs := createFIFOQueue(t, ctx, client, prefix+"jobs.fifo", map[string]string{"VisibilityTimeout": "90", "ReceiveMessageWaitTimeSeconds": "20", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false", "RedrivePolicy": string(jobRedrive)})
	results := createFIFOQueue(t, ctx, client, prefix+"results.fifo", map[string]string{"VisibilityTimeout": "60", "ReceiveMessageWaitTimeSeconds": "20", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false", "RedrivePolicy": string(resultRedrive)})
	for _, queue := range []queueIdentity{jobs, results, jobDLQ, resultDLQ} {
		q := queue
		t.Cleanup(func() {
			_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(q.url)})
		})
	}
	role := deploymentmanifest.Role{Name: "local-test-role", PolicyFingerprintSHA256: strings.Repeat("0", 64), TrustFingerprintSHA256: strings.Repeat("1", 64)}
	manifest := deploymentmanifest.Manifest{Version: 1, Environment: environment, AWSRegion: "ap-south-1", Queues: map[string]deploymentmanifest.Queue{
		"jobs":        {Name: prefix + "jobs.fifo", URL: jobs.url, ARN: jobs.arn, VisibilityTimeoutSeconds: 90, MessageRetentionSeconds: 345600, ReceiveWaitTimeSeconds: 20, MaxReceiveCount: 5, DeadLetterQueueARN: jobDLQ.arn, SSE: "SSE-SQS"},
		"results":     {Name: prefix + "results.fifo", URL: results.url, ARN: results.arn, VisibilityTimeoutSeconds: 60, MessageRetentionSeconds: 345600, ReceiveWaitTimeSeconds: 20, MaxReceiveCount: 10, DeadLetterQueueARN: resultDLQ.arn, SSE: "SSE-SQS"},
		"jobs_dlq":    {Name: prefix + "jobs-dlq.fifo", URL: jobDLQ.url, ARN: jobDLQ.arn, MessageRetentionSeconds: 1209600, SSE: "SSE-SQS"},
		"results_dlq": {Name: prefix + "results-dlq.fifo", URL: resultDLQ.url, ARN: resultDLQ.arn, MessageRetentionSeconds: 1209600, SSE: "SSE-SQS"}}, Roles: map[string]deploymentmanifest.Role{"job_publisher": role, "hosted_worker": role, "queue_gateway": role, "result_consumer": role, "dlq_reconciler": role, "infrastructure_operator": role}}
	if err = deploymentmanifest.VerifySQS(ctx, client, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSQSFiveMinuteDeduplicationBoundary(t *testing.T) {
	if os.Getenv("WATCHTRACE_RUN_SLOW_SQS_TESTS") != "1" {
		t.Skip("slow SQS boundary test is disabled")
	}
	endpoint := os.Getenv("WATCHTRACE_TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("WATCHTRACE_TEST_SQS_ENDPOINT is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("ap-south-1"), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) })
	queue := createFIFOQueue(t, ctx, client, "watchtrace-dedup-boundary-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".fifo", map[string]string{"ContentBasedDeduplication": "false", "ReceiveMessageWaitTimeSeconds": "0", "VisibilityTimeout": "30", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true"})
	defer client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queue.url)})
	send := func() {
		if _, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queue.url), MessageBody: aws.String("immutable"), MessageDeduplicationId: aws.String("stable-job"), MessageGroupId: aws.String("stable-job")}); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() int {
		out, e := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(queue.url), MaxNumberOfMessages: 1, WaitTimeSeconds: 1})
		if e != nil {
			t.Fatal(e)
		}
		if len(out.Messages) == 1 {
			_, e = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(queue.url), ReceiptHandle: out.Messages[0].ReceiptHandle})
			if e != nil {
				t.Fatal(e)
			}
		}
		return len(out.Messages)
	}
	send()
	if receive() != 1 {
		t.Fatal("initial message missing")
	}
	send()
	if receive() != 0 {
		t.Fatal("in-window duplicate was delivered")
	}
	time.Sleep(5*time.Minute + 5*time.Second)
	send()
	if receive() != 1 {
		t.Fatal("post-window same-ID message was not delivered for downstream idempotency")
	}
}

type queueIdentity struct{ url, arn string }

func createFIFOQueue(t *testing.T, ctx context.Context, client *sqs.Client, name string, attributes map[string]string) queueIdentity {
	t.Helper()
	attributes["FifoQueue"] = "true"
	created, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attributes})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	got, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: created.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	arn := got.Attributes[string(types.QueueAttributeNameQueueArn)]
	if arn == "" {
		t.Fatal(fmt.Errorf("queue %s has no ARN", name))
	}
	return queueIdentity{url: aws.ToString(created.QueueUrl), arn: arn}
}
