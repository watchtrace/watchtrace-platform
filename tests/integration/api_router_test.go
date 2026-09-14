package integration_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/backendapi"
	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

func newIntegrationAPIRouter(t *testing.T, pool *pgxpool.Pool, options httpapi.Options) *gin.Engine {
	t.Helper()
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.ReadinessCheck == nil {
		options.ReadinessCheck = pool.Ping
	}
	if options.AuthService == nil {
		options.AuthService = auth.NewService(pool, &recordingVerificationSender{})
	}
	if options.OwnershipService == nil {
		options.OwnershipService = newIntegrationOwnershipService(t, pool, &recordingVerificationSender{})
	}
	if options.MonitorService == nil {
		options.MonitorService = newIntegrationMonitorService(t, pool)
	}
	if options.BackendService == nil {
		options.BackendService = backendapi.New(pool)
	}
	if options.RealtimeService == nil {
		options.RealtimeService = realtime.New(pool)
	}
	if options.OperationsService == nil {
		options.OperationsService = operations.New(pool)
	}
	router, err := httpapi.NewRouter(options)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func newIntegrationOwnershipService(t *testing.T, pool *pgxpool.Pool, sender auth.AccountActionSender) *ownership.Service {
	t.Helper()
	service, err := ownership.NewService(pool, sender)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newIntegrationMonitorService(t *testing.T, pool *pgxpool.Pool) *monitor.Service {
	t.Helper()
	keys, err := secureheaders.New(1, map[int32][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := monitor.NewService(pool, monitor.Config{
		Headers:      keys,
		SigningKey:   signingKey,
		SigningKeyID: "integration-platform-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
