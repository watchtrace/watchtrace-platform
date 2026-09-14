package monitor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

const (
	testUserID        = "97867dd1-1283-4477-a9a5-289cac23151a"
	testEnvironmentID = "3249c694-c0fc-4430-95d3-f12419307dd2"
)

type constructorDB struct{ database.DBTX }

func (constructorDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("not used")
}

func TestNewServiceRequiresCompleteConfiguration(t *testing.T) {
	headers, err := secureheaders.New(1, map[int32][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{Headers: headers, SigningKey: make(ed25519.PrivateKey, ed25519.PrivateKeySize), SigningKeyID: "platform-v1"}
	tests := []struct {
		name   string
		db     databaseConnection
		config Config
	}{
		{name: "database", config: valid},
		{name: "header keys", db: constructorDB{}, config: Config{SigningKey: valid.SigningKey, SigningKeyID: valid.SigningKeyID}},
		{name: "signing key", db: constructorDB{}, config: Config{Headers: headers, SigningKeyID: valid.SigningKeyID}},
		{name: "signing key ID", db: constructorDB{}, config: Config{Headers: headers, SigningKey: valid.SigningKey}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewService(test.db, test.config); err == nil {
				t.Fatalf("NewService accepted missing %s", test.name)
			}
		})
	}
	if service, err := NewService(constructorDB{}, valid); err != nil || service == nil {
		t.Fatalf("valid service = %v, error = %v", service, err)
	}
}

func TestNormalizeCreateInputAppliesDefaults(t *testing.T) {
	input, err := normalizeCreateInput(testUserID, testEnvironmentID, CreateInput{
		Name:      " API Health ",
		TargetURL: " https://example.test/health ",
	})
	if err != nil {
		t.Fatalf("normalize monitor: %v", err)
	}
	if input.Name != "API Health" || input.TargetURL != "https://example.test/health" {
		t.Fatalf("unexpected normalized strings: %+v", input)
	}
	if input.IntervalSeconds != 300 || input.TimeoutSeconds != 5 ||
		input.ExpectedStatusMin != 200 || input.ExpectedStatusMax != 299 {
		t.Fatalf("unexpected defaults: %+v", input)
	}
}

func TestNormalizeCreateInputAcceptsDocumentedBounds(t *testing.T) {
	for _, interval := range []int32{60, 120, 300, 600, 1800} {
		input := CreateInput{
			Name:              "API",
			TargetURL:         "https://example.test/health",
			IntervalSeconds:   interval,
			TimeoutSeconds:    10,
			ExpectedStatusMin: 201,
			ExpectedStatusMax: 399,
		}
		if _, err := normalizeCreateInput(testUserID, testEnvironmentID, input); err != nil {
			t.Fatalf("interval %d rejected: %v", interval, err)
		}
	}
}

func TestNormalizeCreateInputRejectsInvalidValues(t *testing.T) {
	valid := CreateInput{Name: "API", TargetURL: "https://example.test/health"}
	tests := []struct {
		name  string
		user  string
		env   string
		input CreateInput
	}{
		{name: "missing user", env: testEnvironmentID, input: valid},
		{name: "missing environment", user: testUserID, input: valid},
		{name: "missing name", user: testUserID, env: testEnvironmentID, input: CreateInput{TargetURL: valid.TargetURL}},
		{name: "relative URL", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "/health"}},
		{name: "unsupported scheme", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "ftp://example.test/health"}},
		{name: "unsupported port", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "https://example.test:8443/health"}},
		{name: "loopback IPv4", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "http://127.0.0.1/health"}},
		{name: "private IPv6", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "https://[fd00::1]/health"}},
		{name: "URL credentials", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "https://user:password@example.test"}},
		{name: "URL fragment", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "https://example.test/#secret"}},
		{name: "unsupported interval", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: valid.TargetURL, IntervalSeconds: 61}},
		{name: "long timeout", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: valid.TargetURL, TimeoutSeconds: 11}},
		{name: "invalid status", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: valid.TargetURL, ExpectedStatusMin: 300, ExpectedStatusMax: 200}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeCreateInput(test.user, test.env, test.input); err != ErrInvalidInput {
				t.Fatalf("normalization error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestStateFromLatestScheduledResultUsesSharedVocabulary(t *testing.T) {
	if state := stateFromLatestScheduledResult(true); state != StateHealthy {
		t.Fatalf("successful latest result state = %q, want %q", state, StateHealthy)
	}
	if state := stateFromLatestScheduledResult(false); state != StateDegraded {
		t.Fatalf("failed latest result state = %q, want %q", state, StateDegraded)
	}
}
