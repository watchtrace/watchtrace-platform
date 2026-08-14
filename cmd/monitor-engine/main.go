package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/fifo"
	"github.com/watchtrace/watchtrace-platform/internal/quarantine"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

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
	publisher := fifo.NewPublisher(db, fifo.SQSSender{Client: client})
	consumer := fifo.NewResultConsumerWithQuarantine(db, fifo.ResultSQS{Client: client, QueueURL: required("WATCHTRACE_SQS_RESULT_QUEUE_URL")}, quarantineSealer)
	dlq := fifo.NewDLQReconciler(db, &fifo.SQSDLQSource{Client: client, JobDLQURL: required("WATCHTRACE_SQS_HOSTED_JOB_DLQ_URL"), ResultDLQURL: required("WATCHTRACE_SQS_RESULT_DLQ_URL")}, quarantineSealer)
	go runScheduler(ctx, scheduler, logger)
	go runPublisher(ctx, publisher, logger)
	go runConsumer(ctx, consumer, logger)
	go runDLQ(ctx, dlq, logger)
	go runHealth(ctx, db)
	runMaintenance(ctx, db, consumer)
}
func runHealth(ctx context.Context, db *pgxpool.Pool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if db.Ping(r.Context()) != nil {
			http.Error(w, "database_unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics, err := fifo.ReadMetrics(r.Context(), db, time.Now())
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
			logger.Warn("DLQ reconciliation failed")
			time.Sleep(time.Second)
		} else if !worked {
			time.Sleep(time.Second)
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
				logger.Warn("schedule failed")
			}
		}
	}
}

func runPublisher(ctx context.Context, publisher *fifo.Publisher, logger *slog.Logger) {
	for ctx.Err() == nil {
		worked, err := publisher.PublishNext(ctx)
		if err != nil {
			logger.Warn("publish failed")
			time.Sleep(time.Second)
		} else if !worked {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func runConsumer(ctx context.Context, consumer *fifo.ResultConsumer, logger *slog.Logger) {
	for ctx.Err() == nil {
		worked, err := consumer.ConsumeNext(ctx)
		if err != nil {
			logger.Warn("result consume failed")
			time.Sleep(time.Second)
		} else if !worked {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func runMaintenance(ctx context.Context, db *pgxpool.Pool, consumer *fifo.ResultConsumer) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = fifo.ReclaimPublisherLeases(ctx, db)
			_, _ = consumer.SweepDeadlines(ctx)
			_, _ = fifo.CleanupLedger(ctx, db, time.Now())
		}
	}
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
