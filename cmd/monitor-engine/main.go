package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/reliability"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var receiptHandlePattern = regexp.MustCompile(`Value [^ ]+ for parameter ReceiptHandle`)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := pgxpool.New(ctx, required("WATCHTRACE_DATABASE_URL"))
	if err != nil {
		logger.Error("configure database")
		os.Exit(1)
	}
	defer db.Close()
	signing, err := readKey(required("WATCHTRACE_PLATFORM_SIGNING_KEY"))
	if err != nil {
		logger.Error("configure signing key")
		os.Exit(1)
	}
	header, err := readKey(required("WATCHTRACE_MONITOR_HEADER_KEY_FILE"))
	if err != nil {
		logger.Error("configure header key")
		os.Exit(1)
	}
	quarantineKey, err := readKey(required("WATCHTRACE_QUARANTINE_KEY"))
	if err != nil {
		logger.Error("configure quarantine encryption")
		os.Exit(1)
	}
	quarantineSealer, err := quarantine.New(quarantineKey)
	if err != nil {
		logger.Error("configure quarantine encryption")
		os.Exit(1)
	}
	headers, err := secureheaders.New(1, map[int32][]byte{1: header})
	if err != nil {
		logger.Error("configure header encryption")
		os.Exit(1)
	}
	scheduler, err := fifo.NewScheduler(db, ed25519.PrivateKey(signing), value("WATCHTRACE_PLATFORM_SIGNING_KEY_ID", "platform-v1"), headers)
	if err != nil {
		logger.Error("configure scheduler")
		os.Exit(1)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("configure AWS")
		os.Exit(1)
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if endpoint := os.Getenv("WATCHTRACE_SQS_ENDPOINT"); endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	queueURLs := operations.QueueURLs{Jobs: required("WATCHTRACE_SQS_HOSTED_JOB_QUEUE_URL"), Results: required("WATCHTRACE_SQS_RESULT_QUEUE_URL"), JobDLQ: required("WATCHTRACE_SQS_HOSTED_JOB_DLQ_URL"), ResultDLQ: required("WATCHTRACE_SQS_RESULT_DLQ_URL")}
	if err = queueURLs.Validate(); err != nil {
		logger.Error("configure SQS queue URLs", "error", safeError(err))
		os.Exit(1)
	}
	publisher := fifo.NewPublisher(db, loggingSender{next: fifo.SQSSender{Client: client}, logger: logger})
	consumer := fifo.NewResultConsumerWithQuarantine(db, loggingResultSource{next: fifo.ResultSQS{Client: client, QueueURL: queueURLs.Results}, logger: logger}, quarantineSealer)
	dlq := fifo.NewDLQReconciler(db, &fifo.SQSDLQSource{Client: client, JobDLQURL: queueURLs.JobDLQ, ResultDLQURL: queueURLs.ResultDLQ}, quarantineSealer)
	operationsService := operations.NewWithSQS(db, client, queueURLs)
	var workers sync.WaitGroup
	start := func(run func()) { workers.Add(1); go func() { defer workers.Done(); run() }() }
	start(func() { runScheduler(ctx, scheduler, logger) })
	start(func() { runPublisher(ctx, publisher, logger) })
	start(func() { runConsumer(ctx, consumer, logger) })
	start(func() { runDLQ(ctx, dlq, logger) })
	start(func() { runHealth(ctx, db, operationsService) })
	runMaintenance(ctx, db, consumer, reliability.New(db), operationsService, logger)
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logger.Warn("monitor engine shutdown deadline reached")
	}
}
func runHealth(ctx context.Context, db *pgxpool.Pool, operationsService *operations.Service) {
	var ready atomic.Bool
	ready.Store(true)
	go func() { <-ctx.Done(); ready.Store(false) }()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() || db.Ping(r.Context()) != nil {
			http.Error(w, "not_ready", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics, err := operationsService.Read(r.Context())
		if err != nil {
			http.Error(w, "metrics_unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metrics)
	})
	server := &http.Server{Addr: value("WATCHTRACE_ENGINE_HEALTH_ADDRESS", "127.0.0.1:8091"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	_ = server.ListenAndServe()
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

func runMaintenance(ctx context.Context, db *pgxpool.Pool, consumer *fifo.ResultConsumer, reports *reliability.Service, operationsService *operations.Service, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	maintain := func(now time.Time) {
		started := time.Now().UTC()
		leased, firstErr := fifo.ReclaimPublisherLeases(ctx, db)
		expired, secondErr := consumer.SweepDeadlines(ctx)
		deleted, thirdErr := fifo.CleanupLedger(ctx, db, now)
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

func readKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
}
func required(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		panic(name + " required")
	}
	return v
}
func value(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
