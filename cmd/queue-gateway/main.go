package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/watchtrace/watchtrace-platform/internal/gatewayconfig"
	"github.com/watchtrace/watchtrace-platform/internal/queuegateway"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
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
	handler, tlsConfig, address, err := build(ctx)
	if err != nil {
		logger.Error("configure queue gateway")
		os.Exit(1)
	}
	server := &http.Server{Addr: address, Handler: handler, TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err = server.ListenAndServeTLS(required("WATCHTRACE_GATEWAY_CERT"), required("WATCHTRACE_GATEWAY_KEY")); err != nil && err != http.ErrServerClosed {
		logger.Error("queue gateway stopped")
		os.Exit(1)
	}
}
func build(ctx context.Context) (http.Handler, *tls.Config, string, error) {
	data, err := os.ReadFile(required("WATCHTRACE_GATEWAY_CONFIG"))
	if err != nil {
		return nil, nil, "", err
	}
	configPublic, err := readKey(required("WATCHTRACE_GATEWAY_CONFIG_SIGNING_PUBLIC_KEY"))
	if err != nil {
		return nil, nil, "", err
	}
	c, err := gatewayconfig.Verify(data, ed25519.PublicKey(configPublic), time.Now().UTC())
	if err != nil {
		return nil, nil, "", err
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if endpoint := os.Getenv("WATCHTRACE_SQS_ENDPOINT"); endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	pools := make([]queuegateway.Pool, 0, len(c.Pools))
	for _, p := range c.Pools {
		public, err := base64.StdEncoding.DecodeString(p.ResultPublicKey)
		if err != nil {
			return nil, nil, "", err
		}
		revoked := make(map[string]struct{}, len(p.RevokedCertificateSerials))
		for _, serial := range p.RevokedCertificateSerials {
			revoked[serial] = struct{}{}
		}
		pools = append(pools, queuegateway.Pool{ID: p.ID, ResultKeyID: p.ResultKeyID, ResultPublic: ed25519.PublicKey(public), SchemaMin: p.SchemaMin, SchemaMax: p.SchemaMax, MaxRequestsPerMinute: p.Limits.RequestsPerMinute, MaxBytesPerMinute: p.Limits.BytesPerMinute, MaxConcurrentPulls: p.Limits.ConcurrentPulls, MaxResultsPerMinute: p.Limits.ResultsPerMinute, RevokedCertificateSerials: revoked, Transport: &workqueue.DirectSQS{Client: client, JobQueueURL: p.JobQueueURL, ResultQueueURL: c.ResultQueueURL, WorkerPoolID: p.ID}})
	}
	lease, err := readKey(required("WATCHTRACE_GATEWAY_LEASE_KEY"))
	if err != nil {
		return nil, nil, "", err
	}
	gateway, err := queuegateway.New(pools, lease, 5*time.Second)
	if err != nil {
		return nil, nil, "", err
	}
	caData, err := os.ReadFile(required("WATCHTRACE_GATEWAY_CLIENT_CA"))
	if err != nil {
		return nil, nil, "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return nil, nil, "", errors.New("invalid client CA")
	}
	tlsConfig := &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS12}
	address := value("WATCHTRACE_GATEWAY_ADDRESS", "127.0.0.1:8443")
	return gateway.Handler(), tlsConfig, address, nil
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
