package integration_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHundredTimeoutQueueAndLedgerBoundary(t *testing.T) {
	endpoint, databaseURL := os.Getenv("WATCHTRACE_TEST_SQS_ENDPOINT"), os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if endpoint == "" || databaseURL == "" {
		t.Skip("local SQS and PostgreSQL are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("ap-south-1"), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) })
	queue := createFIFOQueue(t, ctx, client, fmt.Sprintf("watchtrace-load-%d.fifo", time.Now().UnixNano()), map[string]string{"VisibilityTimeout": "90", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false"})
	defer client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queue.url)})
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			id := fmt.Sprintf("timeout-%03d", i)
			if _, e := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queue.url), MessageBody: aws.String("bounded-timeout"), MessageDeduplicationId: aws.String(id), MessageGroupId: aws.String(id)}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if time.Since(start) > 20*time.Second {
		t.Fatal("queue did not approach planned 20 checks/second boundary")
	}
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(queue.url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Attributes["ApproximateNumberOfMessages"] != "100" {
		t.Fatalf("queue depth=%s", attrs.Attributes["ApproximateNumberOfMessages"])
	}
	var nonterminal int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM check_jobs WHERE state IN('pending','pending_publish','published','running')`).Scan(&nonterminal); err != nil {
		t.Fatal(err)
	}
	if nonterminal > 1000 {
		t.Fatalf("ledger exceeded admission bound: %d", nonterminal)
	}
}
