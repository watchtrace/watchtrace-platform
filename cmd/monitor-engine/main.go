package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	platformconfig "github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

var receiptHandlePattern = regexp.MustCompile(`Value [^ ]+ for parameter ReceiptHandle`)

type engineRuntime struct {
	database      *pgxpool.Pool
	scheduler     *fifo.Scheduler
	publisher     *fifo.Publisher
	consumer      *fifo.ResultConsumer
	dlq           *fifo.DLQReconciler
	reports       *reliability.Service
	operations    *operations.Service
	healthAddress string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := runCommand(logger); err != nil {
		logger.Error("monitor engine stopped", "error", safeError(err))
		os.Exit(1)
	}
}

func runCommand(logger *slog.Logger) error {
	configuration, err := platformconfig.LoadMonitorEngine()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtime, err := build(ctx, configuration, logger)
	if err != nil {
		return fmt.Errorf("build monitor engine: %w", err)
	}
	defer runtime.database.Close()
	return runtime.run(ctx, logger)
}

func build(ctx context.Context, configuration platformconfig.MonitorEngineConfig, logger *slog.Logger) (*engineRuntime, error) {
	database, err := pgxpool.New(ctx, configuration.DatabaseURL)
	if err != nil {
		return nil, err
	}
	fail := func(buildErr error) (*engineRuntime, error) {
		database.Close()
		return nil, buildErr
	}

	quarantineSealer, err := quarantine.New(configuration.QuarantineKey)
	if err != nil {
		return fail(err)
	}
	headers, err := secureheaders.New(1, map[int32][]byte{1: configuration.HeaderKey})
	if err != nil {
		return fail(err)
	}
	scheduler, err := fifo.NewScheduler(database, configuration.SigningKey, configuration.SigningKeyID, headers)
	if err != nil {
		return fail(err)
	}
	awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fail(err)
	}
	client := sqs.NewFromConfig(awsConfiguration, func(options *sqs.Options) {
		if configuration.SQSEndpoint != "" {
			options.BaseEndpoint = aws.String(configuration.SQSEndpoint)
		}
	})
	queueURLs := operations.QueueURLs{
		Jobs: configuration.JobQueueURL, Results: configuration.ResultQueueURL,
		JobDLQ: configuration.JobDLQURL, ResultDLQ: configuration.ResultDLQURL,
	}
	if err = queueURLs.Validate(); err != nil {
		return fail(err)
	}
	publisher := fifo.NewPublisher(database, loggingSender{next: fifo.SQSSender{Client: client}, logger: logger})
	consumer, err := fifo.NewResultConsumer(
		database,
		loggingResultSource{next: fifo.ResultSQS{Client: client, QueueURL: queueURLs.Results}, logger: logger},
		quarantineSealer,
	)
	if err != nil {
		return fail(err)
	}
	dlq, err := fifo.NewDLQReconciler(database, &fifo.SQSDLQSource{
		Client: client, JobDLQURL: queueURLs.JobDLQ, ResultDLQURL: queueURLs.ResultDLQ,
	}, quarantineSealer)
	if err != nil {
		return fail(err)
	}
	return &engineRuntime{
		database: database, scheduler: scheduler, publisher: publisher, consumer: consumer, dlq: dlq,
		reports: reliability.New(database), operations: operations.NewWithSQS(database, client, queueURLs),
		healthAddress: configuration.HealthAddress,
	}, nil
}

func (runtime *engineRuntime) run(ctx context.Context, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", runtime.healthAddress)
	if err != nil {
		return fmt.Errorf("listen for health checks: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var ready atomic.Bool
	ready.Store(true)
	defer ready.Store(false)
	healthDone := make(chan error, 1)

	var workers sync.WaitGroup
	start := func(run func()) {
		workers.Add(1)
		go func() { defer workers.Done(); run() }()
	}
	start(func() { runScheduler(runCtx, runtime.scheduler, logger) })
	start(func() { runPublisher(runCtx, runtime.publisher, logger) })
	start(func() { runConsumer(runCtx, runtime.consumer, logger) })
	start(func() { runDLQ(runCtx, runtime.dlq, logger) })
	start(func() {
		healthErr := httpserver.New(healthHandler(runtime.database, runtime.operations, &ready), 5*time.Second).Serve(runCtx, listener)
		healthDone <- healthErr
		if runCtx.Err() == nil {
			cancel()
		}
	})

	runMaintenance(runCtx, runtime.database, runtime.consumer, runtime.reports, runtime.operations, logger)
	ready.Store(false)
	cancel()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logger.Warn("monitor engine shutdown deadline reached")
		return nil
	}

	select {
	case healthErr := <-healthDone:
		if healthErr != nil {
			return fmt.Errorf("serve health checks: %w", healthErr)
		}
		if ctx.Err() == nil {
			return errors.New("health server stopped unexpectedly")
		}
	default:
	}
	return nil
}

func healthHandler(database *pgxpool.Pool, operationsService *operations.Service, ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(writer http.ResponseWriter, request *http.Request) {
		if !ready.Load() || database.Ping(request.Context()) != nil {
			http.Error(writer, "not_ready", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		metrics, err := operationsService.Read(request.Context())
		if err != nil {
			http.Error(writer, "metrics_unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(metrics)
	})
	return mux
}

func runDLQ(ctx context.Context, reconciler *fifo.DLQReconciler, logger *slog.Logger) {
	for ctx.Err() == nil {
		worked, err := reconciler.ReconcileNext(ctx)
		if err != nil {
			logger.Warn("DLQ reconciliation failed", "error", safeError(err))
			wait(ctx, time.Second)
		} else if !worked {
			wait(ctx, time.Second)
		}
	}
}

func runScheduler(ctx context.Context, scheduler *fifo.Scheduler, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := scheduler.ScheduleDue(ctx, 20); err != nil {
				logger.Warn("schedule failed", "error", safeError(err))
			}
		}
	}
}

func runPublisher(ctx context.Context, publisher *fifo.Publisher, logger *slog.Logger) {
	for ctx.Err() == nil {
		worked, err := publisher.PublishNext(ctx)
		if err != nil {
			logger.Warn("publish failed", "error", safeError(err))
			wait(ctx, time.Second)
		} else if !worked {
			wait(ctx, 100*time.Millisecond)
		}
	}
}

func runConsumer(ctx context.Context, consumer *fifo.ResultConsumer, logger *slog.Logger) {
	for ctx.Err() == nil {
		worked, err := consumer.ConsumeNext(ctx)
		if err != nil {
			logger.Warn("result consume failed", "error", safeError(err))
			wait(ctx, time.Second)
		} else if !worked {
			wait(ctx, 100*time.Millisecond)
		}
	}
}

func runMaintenance(ctx context.Context, database *pgxpool.Pool, consumer *fifo.ResultConsumer, reports *reliability.Service, operationsService *operations.Service, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	maintain := func(now time.Time) {
		started := time.Now().UTC()
		leased, firstErr := fifo.ReclaimPublisherLeases(ctx, database)
		expired, secondErr := consumer.SweepDeadlines(ctx)
		deleted, thirdErr := fifo.CleanupLedger(ctx, database, now)
		queueErr := errors.Join(firstErr, secondErr, thirdErr)
		_ = operationsService.Record(context.Background(), "queue_maintenance", started, leased+expired+deleted, queueErr)
		started = time.Now().UTC()
		if err := reports.Maintain(ctx, now); err != nil {
			logger.Warn("reliability maintenance failed", "error", safeError(err))
			_ = operationsService.Record(context.Background(), "rollup_retention", started, 0, err)
		} else {
			_ = operationsService.Record(context.Background(), "rollup_retention", started, 0, nil)
		}
	}
	maintain(time.Now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			maintain(now.UTC())
		}
	}
}

func wait(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return receiptHandlePattern.ReplaceAllString(err.Error(), "Value [redacted] for parameter ReceiptHandle")
}
