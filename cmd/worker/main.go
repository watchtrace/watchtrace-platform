package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/watchtrace/watchtrace-platform/internal/checkengine"
	"github.com/watchtrace/watchtrace-platform/internal/destination"
	"github.com/watchtrace/watchtrace-platform/internal/modworker"
	"github.com/watchtrace/watchtrace-platform/internal/workerjournal"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
	"log/slog"
	"net/http"
	"net/netip"
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
	worker, cleanup, err := build(ctx)
	if err != nil {
		logger.Error("configure worker")
		os.Exit(1)
	}
	defer cleanup()
	go health(ctx, worker)
	go maintain(ctx, worker)
	for ctx.Err() == nil {
		worked, err := worker.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Warn("worker attempt failed", "category", "internal")
			time.Sleep(time.Second)
		} else if !worked {
			time.Sleep(100 * time.Millisecond)
		}
	}
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
func build(ctx context.Context) (*modworker.Worker, func(), error) {
	pool := required("WATCHTRACE_WORKER_POOL_ID")
	workerID := required("WATCHTRACE_WORKER_ID")
	encBytes, err := readKey("WATCHTRACE_WORKER_ENCRYPTION_KEY")
	if err != nil {
		return nil, nil, err
	}
	enc, err := ecdh.X25519().NewPrivateKey(encBytes)
	if err != nil {
		return nil, nil, err
	}
	result, err := readKey("WATCHTRACE_WORKER_RESULT_KEY")
	if err != nil {
		return nil, nil, err
	}
	platform, err := readKey("WATCHTRACE_PLATFORM_SIGNING_PUBLIC_KEY")
	if err != nil {
		return nil, nil, err
	}
	workerKeys := map[string]*ecdh.PrivateKey{required("WATCHTRACE_WORKER_ENCRYPTION_KEY_ID"): enc}
	platformKeys := map[string]ed25519.PublicKey{required("WATCHTRACE_PLATFORM_SIGNING_KEY_ID"): ed25519.PublicKey(platform)}
	revoked := map[string]struct{}{}
	if keyringPath := strings.TrimSpace(os.Getenv("WATCHTRACE_WORKER_KEYRING")); keyringPath != "" {
		var trusted struct {
			WorkerEncryption map[string]string `json:"worker_encryption"`
			PlatformSigning  map[string]string `json:"platform_signing"`
			Revoked          []string          `json:"revoked"`
		}
		data, e := os.ReadFile(keyringPath)
		if e != nil || json.Unmarshal(data, &trusted) != nil {
			return nil, nil, errors.New("invalid worker keyring")
		}
		for id, path := range trusted.WorkerEncryption {
			raw, e := readKeyPath(path)
			if e != nil {
				return nil, nil, e
			}
			key, e := ecdh.X25519().NewPrivateKey(raw)
			if e != nil {
				return nil, nil, errors.New("invalid worker keyring")
			}
			workerKeys[id] = key
		}
		for id, path := range trusted.PlatformSigning {
			raw, e := readKeyPath(path)
			if e != nil || len(raw) != ed25519.PublicKeySize {
				return nil, nil, errors.New("invalid worker keyring")
			}
			platformKeys[id] = ed25519.PublicKey(raw)
		}
		for _, id := range trusted.Revoked {
			revoked[id] = struct{}{}
		}
	}
	journal, err := workerjournal.Open(value("WATCHTRACE_WORKER_JOURNAL", "/var/lib/watchtrace-worker/journal.sqlite"))
	if err != nil {
		return nil, nil, err
	}
	policy := destination.Policy{MaxRedirects: 3}
	for _, raw := range strings.Split(os.Getenv("WATCHTRACE_PRIVATE_CIDRS"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		prefix, e := netip.ParsePrefix(strings.TrimSpace(raw))
		if e != nil {
			journal.Close()
			return nil, nil, e
		}
		policy.AllowPrivateCIDRs = append(policy.AllowPrivateCIDRs, prefix)
	}
	engine := checkengine.New(policy, nil, nil)
	var transport workqueue.Transport
	if value("WATCHTRACE_WORKER_TRANSPORT", "https") == "direct_sqs" {
		cfg, e := awsconfig.LoadDefaultConfig(ctx)
		if e != nil {
			journal.Close()
			return nil, nil, e
		}
		client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
			if endpoint := os.Getenv("WATCHTRACE_SQS_ENDPOINT"); endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
			}
		})
		transport = &workqueue.DirectSQS{Client: client, JobQueueURL: required("WATCHTRACE_JOB_QUEUE_URL"), ResultQueueURL: required("WATCHTRACE_RESULT_QUEUE_URL"), WorkerPoolID: pool}
	} else {
		client, e := mtlsClient()
		if e != nil {
			journal.Close()
			return nil, nil, e
		}
		transport = &workqueue.HTTPS{BaseURL: required("WATCHTRACE_GATEWAY_URL"), Client: client, PoolToken: os.Getenv("WATCHTRACE_POOL_TOKEN")}
	}
	w, err := modworker.New(transport, journal, engine, modworker.Config{WorkerID: workerID, WorkerPoolID: pool, PlatformKeyID: required("WATCHTRACE_PLATFORM_SIGNING_KEY_ID"), WorkerEncryptionKeyID: required("WATCHTRACE_WORKER_ENCRYPTION_KEY_ID"), ResultKeyID: required("WATCHTRACE_RESULT_KEY_ID"), ClockTolerance: 5 * time.Second, WorkerPrivate: enc, PlatformPublic: ed25519.PublicKey(platform), ResultPrivate: ed25519.PrivateKey(result), WorkerPrivateKeys: workerKeys, PlatformPublicKeys: platformKeys, RevokedKeyIDs: revoked})
	if err != nil {
		journal.Close()
		return nil, nil, err
	}
	return w, func() { journal.Close() }, nil
}
func mtlsClient() (*http.Client, error) {
	certPath, keyPath, caPath := os.Getenv("WATCHTRACE_MTLS_CERT"), os.Getenv("WATCHTRACE_MTLS_KEY"), os.Getenv("WATCHTRACE_GATEWAY_CA")
	if certPath == "" || keyPath == "" || caPath == "" {
		return nil, errors.New("mTLS certificate, key, and CA are required for HTTPS transport")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	caData, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return nil, errors.New("invalid gateway CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 30 * time.Second}, nil
}
func health(ctx context.Context, worker *modworker.Worker) {
	address := value("WATCHTRACE_WORKER_HEALTH_ADDRESS", "127.0.0.1:8090")
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if worker == nil || !worker.Ready() {
			http.Error(w, "result_path_unavailable", 503)
			return
		}
		offset, _ := time.ParseDuration(value("WATCHTRACE_CLOCK_OFFSET", "0s"))
		if !modworker.ClockHealthy(offset, 5*time.Second) {
			http.Error(w, "clock_unsynchronized", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics, err := worker.JournalMetrics(r.Context())
		if err != nil {
			http.Error(w, "journal_unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metrics)
	})
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	_ = server.ListenAndServe()
}
func readKey(name string) ([]byte, error) {
	return readKeyPath(required(name))
}
func readKeyPath(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid key file")
	}
	return decoded, nil
}
func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		panic(name + " is required")
	}
	return value
}
func value(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
