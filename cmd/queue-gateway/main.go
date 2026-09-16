package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
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
	"github.com/watchtrace/watchtrace-platform/internal/gatewayconfig"
	platformconfig "github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/httpserver"
	"github.com/watchtrace/watchtrace-platform/internal/queuegateway"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := runCommand(); err != nil {
		logger.Error("queue gateway stopped", "error", err)
		os.Exit(1)
	}
}

func runCommand() error {
	configuration, err := platformconfig.LoadQueueGateway()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler, err := build(ctx, configuration)
	if err != nil {
		return fmt.Errorf("build queue gateway: %w", err)
	}
	listener, err := net.Listen("tcp", configuration.Address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	tlsListener := tls.NewListener(listener, configuration.ServerTLS.Clone())
	server := httpserver.NewConfigured(handler, httpserver.Config{
		ShutdownTimeout:   10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	})
	if err = server.Serve(ctx, tlsListener); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func build(ctx context.Context, configuration platformconfig.QueueGatewayConfig) (http.Handler, error) {
	verified, err := gatewayconfig.Verify(configuration.SignedConfig, configuration.SigningPublicKey, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := sqs.NewFromConfig(awsConfiguration, func(options *sqs.Options) {
		if configuration.SQSEndpoint != "" {
			options.BaseEndpoint = aws.String(configuration.SQSEndpoint)
		}
	})

	pools := make([]queuegateway.Pool, 0, len(verified.Pools))
	for _, configuredPool := range verified.Pools {
		publicKey, decodeErr := base64.StdEncoding.DecodeString(configuredPool.ResultPublicKey)
		if decodeErr != nil {
			return nil, decodeErr
		}
		revoked := make(map[string]struct{}, len(configuredPool.RevokedCertificateSerials))
		for _, serial := range configuredPool.RevokedCertificateSerials {
			revoked[serial] = struct{}{}
		}
		pools = append(pools, queuegateway.Pool{
			ID: configuredPool.ID, ResultKeyID: configuredPool.ResultKeyID,
			ResultPublic: ed25519.PublicKey(publicKey), SchemaMin: configuredPool.SchemaMin, SchemaMax: configuredPool.SchemaMax,
			MaxRequestsPerMinute:      configuredPool.Limits.RequestsPerMinute,
			MaxBytesPerMinute:         configuredPool.Limits.BytesPerMinute,
			MaxConcurrentPulls:        configuredPool.Limits.ConcurrentPulls,
			MaxResultsPerMinute:       configuredPool.Limits.ResultsPerMinute,
			RevokedCertificateSerials: revoked,
			Transport: &workqueue.DirectSQS{
				Client: client, JobQueueURL: configuredPool.JobQueueURL,
				ResultQueueURL: verified.ResultQueueURL, WorkerPoolID: configuredPool.ID,
			},
		})
	}
	gateway, err := queuegateway.New(pools, configuration.LeaseKey, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return gateway.Handler(), nil
}
