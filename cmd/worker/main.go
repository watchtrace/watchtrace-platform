package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/watchtrace/watchtrace-platform/internal/checkengine"
	"github.com/watchtrace/watchtrace-platform/internal/destination"
	"github.com/watchtrace/watchtrace-platform/internal/modworker"
	platformconfig "github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
	"github.com/watchtrace/watchtrace-platform/internal/workerjournal"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type workerRuntime struct {
	worker        *modworker.Worker
	healthAddress string
	clockOffset   time.Duration
	close         func()
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := runCommand(logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func runCommand(logger *slog.Logger) error {
	configuration, err := platformconfig.LoadWorker()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtime, err := build(ctx, configuration, logger)
	if err != nil {
		return fmt.Errorf("build worker: %w", err)
	}
	defer runtime.close()
	return runtime.run(ctx, logger)
}

func build(ctx context.Context, configuration platformconfig.WorkerConfig, logger *slog.Logger) (*workerRuntime, error) {
	encryptionKey, err := ecdh.X25519().NewPrivateKey(configuration.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("configure worker encryption: %w", err)
	}
	workerKeys := map[string]*ecdh.PrivateKey{configuration.EncryptionKeyID: encryptionKey}
	for id, raw := range configuration.Keyring.WorkerEncryption {
		key, keyErr := ecdh.X25519().NewPrivateKey(raw)
		if keyErr != nil {
			return nil, fmt.Errorf("configure worker keyring: %w", keyErr)
		}
		workerKeys[id] = key
	}
	platformKeys := map[string]ed25519.PublicKey{configuration.PlatformKeyID: configuration.PlatformPublicKey}
	for id, key := range configuration.Keyring.PlatformSigning {
		platformKeys[id] = key
	}

	journal, err := workerjournal.Open(configuration.JournalPath)
	if err != nil {
		return nil, err
	}
	closeJournal := func() { _ = journal.Close() }

	engine := checkengine.New(destination.Policy{MaxRedirects: 3, AllowPrivateCIDRs: configuration.PrivateCIDRs}, nil, nil)
	transport, err := buildTransport(ctx, configuration)
	if err != nil {
		closeJournal()
		return nil, err
	}
	transport = loggingTransport{next: transport, logger: logger, transportName: configuration.Transport}

	worker, err := modworker.New(transport, journal, engine, modworker.Config{
		WorkerID:              configuration.WorkerID,
		WorkerPoolID:          configuration.PoolID,
		PlatformKeyID:         configuration.PlatformKeyID,
		WorkerEncryptionKeyID: configuration.EncryptionKeyID,
		ResultKeyID:           configuration.ResultKeyID,
		ClockTolerance:        5 * time.Second,
		WorkerPrivate:         encryptionKey,
		PlatformPublic:        configuration.PlatformPublicKey,
		ResultPrivate:         ed25519.PrivateKey(configuration.ResultKey),
		WorkerPrivateKeys:     workerKeys,
		PlatformPublicKeys:    platformKeys,
		RevokedKeyIDs:         configuration.Keyring.Revoked,
	})
	if err != nil {
		closeJournal()
		return nil, err
	}
	return &workerRuntime{
		worker: worker, healthAddress: configuration.HealthAddress,
		clockOffset: configuration.ClockOffset, close: closeJournal,
	}, nil
}

func buildTransport(ctx context.Context, configuration platformconfig.WorkerConfig) (workqueue.Transport, error) {
	if configuration.Transport == platformconfig.WorkerTransportDirectSQS {
		awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, err
		}
		client := sqs.NewFromConfig(awsConfiguration, func(options *sqs.Options) {
			if configuration.SQSEndpoint != "" {
				options.BaseEndpoint = aws.String(configuration.SQSEndpoint)
			}
		})
		return &workqueue.DirectSQS{
			Client: client, JobQueueURL: configuration.JobQueueURL,
			ResultQueueURL: configuration.ResultQueueURL, WorkerPoolID: configuration.PoolID,
		}, nil
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: configuration.ClientTLS.Clone()},
		Timeout:   30 * time.Second,
	}
	return &workqueue.HTTPS{BaseURL: configuration.GatewayURL, Client: client}, nil
}

func (runtime *workerRuntime) run(ctx context.Context, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", runtime.healthAddress)
	if err != nil {
		return fmt.Errorf("listen for health checks: %w", err)
	}
	healthDone := make(chan error, 1)
	go func() {
		healthDone <- httpserver.New(healthHandler(runtime.worker, runtime.clockOffset), 5*time.Second).Serve(ctx, listener)
	}()
	go maintain(ctx, runtime.worker)

	for ctx.Err() == nil {
		select {
		case healthErr := <-healthDone:
			if healthErr != nil {
				return fmt.Errorf("serve health checks: %w", healthErr)
			}
			if ctx.Err() == nil {
				return fmt.Errorf("health server stopped unexpectedly")
			}
			return nil
		default:
		}
		worked, runErr := runtime.worker.RunOne(ctx)
		if runErr != nil && ctx.Err() == nil {
			logger.Warn("worker attempt failed", "category", "internal", "error", runErr)
			waitFor(ctx, time.Second)
		} else if !worked {
			waitFor(ctx, 100*time.Millisecond)
		}
	}
	if healthErr := <-healthDone; healthErr != nil {
		return fmt.Errorf("serve health checks: %w", healthErr)
	}
	return nil
}

func healthHandler(worker *modworker.Worker, clockOffset time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(writer http.ResponseWriter, _ *http.Request) {
		if worker == nil || !worker.Ready() {
			http.Error(writer, "result_path_unavailable", http.StatusServiceUnavailable)
			return
		}
		if !modworker.ClockHealthy(clockOffset, 5*time.Second) {
			http.Error(writer, "clock_unsynchronized", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		metrics, err := worker.JournalMetrics(request.Context())
		if err != nil {
			http.Error(writer, "journal_unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(metrics)
	})
	return mux
}

func maintain(ctx context.Context, worker *modworker.Worker) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = worker.CleanupJournal(ctx)
		}
	}
}

func waitFor(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
