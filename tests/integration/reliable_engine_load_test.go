package integration_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/checkengine"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/modworker"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
	"github.com/watchtrace/watchtrace-platform/internal/workerjournal"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

func TestHundredTimeoutsAcrossQueueJournalsAndLedger(t *testing.T) {
	endpoint, databaseURL := os.Getenv("WATCHTRACE_TEST_SQS_ENDPOINT"), os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	realAWS := os.Getenv("WATCHTRACE_TEST_AWS_SQS") == "1"
	if databaseURL == "" || (endpoint == "" && !realAWS) {
		t.Skip("PostgreSQL and an SQS test target are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	region := "ap-south-1"
	options := []func(*awsconfig.LoadOptions) error{}
	if realAWS {
		region = strings.TrimSpace(os.Getenv("AWS_REGION"))
		if region == "" || endpoint != "" {
			t.Fatal("real AWS load test requires AWS_REGION and forbids an endpoint override")
		}
	} else {
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	}
	options = append(options, awsconfig.WithRegion(region))
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	environment := strings.TrimSpace(os.Getenv("WATCHTRACE_ENV"))
	if environment == "" {
		environment = "dev"
	}
	if environment != "dev" && environment != "stg" && environment != "prod" {
		t.Fatal("WATCHTRACE_ENV must be dev, stg, or prod")
	}
	prefix := fmt.Sprintf("watchtrace-%s-p1309-load-%d", environment, time.Now().UnixNano())
	jobDLQ := createFIFOQueue(t, ctx, client, prefix+"-jobs-dlq.fifo", map[string]string{"VisibilityTimeout": "0", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "1209600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false"})
	resultDLQ := createFIFOQueue(t, ctx, client, prefix+"-results-dlq.fifo", map[string]string{"VisibilityTimeout": "0", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "1209600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false"})
	jobRedrive := fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":5}`, jobDLQ.arn)
	resultRedrive := fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":10}`, resultDLQ.arn)
	jobs := createFIFOQueue(t, ctx, client, prefix+"-jobs.fifo", map[string]string{"VisibilityTimeout": "90", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false", "RedrivePolicy": jobRedrive})
	results := createFIFOQueue(t, ctx, client, prefix+"-results.fifo", map[string]string{"VisibilityTimeout": "60", "ReceiveMessageWaitTimeSeconds": "0", "MessageRetentionPeriod": "345600", "SqsManagedSseEnabled": "true", "ContentBasedDeduplication": "false", "RedrivePolicy": resultRedrive})
	for _, queue := range []queueIdentity{jobs, results, jobDLQ, resultDLQ} {
		queue := queue
		t.Cleanup(func() {
			_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queue.url)})
		})
	}

	slug := "timeout-pipeline"
	deleteSchedulerTestData(t, ctx, db, []string{slug})
	t.Cleanup(func() { deleteSchedulerTestData(t, context.Background(), db, []string{slug}) })
	organizationID, environmentID := insertSchedulerTenant(t, ctx, db, slug)
	platformPublic, platformPrivate, _ := ed25519.GenerateKey(rand.Reader)
	resultPublic, resultPrivate, _ := ed25519.GenerateKey(rand.Reader)
	workerPrivate, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err = db.Exec(ctx, `UPDATE worker_pools SET enabled=true,lifecycle_state='active',encryption_key_id='load-enc',encryption_public_key=$1,result_key_id='load-result',result_public_key=$2,job_queue_url=$3 WHERE id='hosted'`, workerPrivate.PublicKey().Bytes(), resultPublic, jobs.url); err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Second)
	for index := 0; index < 100; index++ {
		monitorID := insertSchedulerMonitor(t, ctx, db, organizationID, environmentID, fmt.Sprintf("Timeout %03d", index), 60, due)
		if _, err = db.Exec(ctx, `UPDATE monitors SET target_url='https://controlled-timeout.test/health',timeout_seconds=1 WHERE id=$1::uuid`, monitorID); err != nil {
			t.Fatal(err)
		}
	}
	headerKeys, _ := secureheaders.New(1, map[int32][]byte{1: make([]byte, 32)})
	scheduler, err := fifo.NewScheduler(db, platformPrivate, "load-platform", headerKeys)
	if err != nil {
		t.Fatal(err)
	}
	if created, scheduleErr := scheduler.ScheduleDue(ctx, 100); scheduleErr != nil || created != 100 {
		t.Fatalf("scheduled=%d error=%v", created, scheduleErr)
	}
	publisher := fifo.NewPublisher(db, fifo.SQSSender{Client: client})
	for index := 0; index < 100; index++ {
		worked, publishErr := publisher.PublishNext(ctx)
		if publishErr != nil || !worked {
			t.Fatalf("publish %d worked=%t error=%v", index, worked, publishErr)
		}
	}

	var targetCalls atomic.Int32
	var timingMu sync.Mutex
	var firstStart, lastFinish time.Time
	journalDirectory := t.TempDir()
	engine := checkengine.NewWithClient(engineDoer(func(request *http.Request) (*http.Response, error) {
		now := time.Now()
		timingMu.Lock()
		if firstStart.IsZero() {
			firstStart = now
		}
		timingMu.Unlock()
		targetCalls.Add(1)
		<-request.Context().Done()
		timingMu.Lock()
		lastFinish = time.Now()
		timingMu.Unlock()
		return nil, request.Context().Err()
	}))
	workerErrors := make(chan error, 20)
	var workers sync.WaitGroup
	for workerIndex := 0; workerIndex < 20; workerIndex++ {
		workerIndex := workerIndex
		workers.Add(1)
		go func() {
			defer workers.Done()
			journal, openErr := workerjournal.Open(filepath.Join(journalDirectory, fmt.Sprintf("worker-%02d.sqlite", workerIndex)))
			if openErr != nil {
				workerErrors <- openErr
				return
			}
			defer journal.Close()
			transport := &workqueue.DirectSQS{Client: client, JobQueueURL: jobs.url, ResultQueueURL: results.url, WorkerPoolID: "hosted"}
			worker, workerErr := modworker.New(transport, journal, engine, modworker.Config{WorkerID: fmt.Sprintf("load-worker-%02d", workerIndex), WorkerPoolID: "hosted", PlatformKeyID: "load-platform", WorkerEncryptionKeyID: "load-enc", ResultKeyID: "load-result", ClockTolerance: 5 * time.Second, WorkerPrivate: workerPrivate, PlatformPublic: platformPublic, ResultPrivate: resultPrivate})
			if workerErr != nil {
				workerErrors <- workerErr
				return
			}
			completed := 0
			for completed < 5 {
				worked, runErr := worker.RunOne(ctx)
				if runErr != nil {
					workerErrors <- runErr
					return
				}
				if worked {
					completed++
				}
			}
			metrics, metricErr := worker.JournalMetrics(ctx)
			if metricErr != nil || metrics.Accepted+metrics.Completed > 5 {
				workerErrors <- fmt.Errorf("journal accepted=%d completed=%d error=%v", metrics.Accepted, metrics.Completed, metricErr)
			}
		}()
	}
	workers.Wait()
	close(workerErrors)
	for workerErr := range workerErrors {
		if workerErr != nil {
			t.Fatal(workerErr)
		}
	}
	if targetCalls.Load() != 100 {
		t.Fatalf("target calls=%d", targetCalls.Load())
	}
	timingMu.Lock()
	elapsed := lastFinish.Sub(firstStart)
	timingMu.Unlock()
	rate := float64(targetCalls.Load()) / elapsed.Seconds()
	if realAWS {
		if rate < 7.5 {
			t.Fatalf("remote Amazon SQS timeout throughput %.2f checks/second is below the recorded safe floor", rate)
		}
	} else if elapsed > 10*time.Second {
		t.Fatalf("100 one-second local timeout checks took %s; expected approximately five seconds at 20 workers", elapsed)
	}
	t.Logf("100 one-second timeout checks completed in %s at 20-worker concurrency (%.2f checks/second)", elapsed, rate)

	consumer := fifo.NewResultConsumer(db, fifo.ResultSQS{Client: client, QueueURL: results.url})
	for index := 0; index < 100; index++ {
		worked, consumeErr := consumer.ConsumeNext(ctx)
		if consumeErr != nil || !worked {
			t.Fatalf("consume %d worked=%t error=%v", index, worked, consumeErr)
		}
	}
	var resultsStored, timeoutResults, nonterminal int
	if err = db.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE error_category='timeout'),(SELECT count(*) FROM check_jobs WHERE organization_id=$1::uuid AND state<>'completed') FROM health_checks WHERE organization_id=$1::uuid`, organizationID).Scan(&resultsStored, &timeoutResults, &nonterminal); err != nil {
		t.Fatal(err)
	}
	if resultsStored != 100 || timeoutResults != 100 || nonterminal != 0 {
		t.Fatalf("stored=%d timeouts=%d nonterminal=%d", resultsStored, timeoutResults, nonterminal)
	}
	for name, queue := range map[string]queueIdentity{"jobs": jobs, "results": results, "jobs_dlq": jobDLQ, "results_dlq": resultDLQ} {
		assertQueueEmpty(t, ctx, client, name, queue.url)
	}
}

func assertQueueEmpty(t *testing.T, ctx context.Context, client *sqs.Client, name, queueURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		attributes, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(queueURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible, types.QueueAttributeNameApproximateNumberOfMessagesDelayed}})
		if err != nil {
			t.Fatal(err)
		}
		if attributes.Attributes["ApproximateNumberOfMessages"] == "0" && attributes.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0" && attributes.Attributes["ApproximateNumberOfMessagesDelayed"] == "0" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s queue did not drain", strings.TrimSpace(name))
}
