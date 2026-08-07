package config

import (
	"strings"
	"testing"
	"time"
)

const validDatabaseURL = "postgres://watchtrace:local-password@127.0.0.1:5432/watchtrace?sslmode=disable"

func TestLoadUsesDefaults(t *testing.T) {
	configuration, err := load(environment(map[string]string{
		databaseURLEnvironment: validDatabaseURL,
	}))
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}

	if configuration.DatabaseURL != validDatabaseURL {
		t.Fatalf("DatabaseURL = %q, want supplied URL", configuration.DatabaseURL)
	}
	if configuration.HTTPAddress != defaultHTTPAddress {
		t.Fatalf("HTTPAddress = %q, want %q", configuration.HTTPAddress, defaultHTTPAddress)
	}
	if configuration.ShutdownTimeout != defaultShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %s, want %s", configuration.ShutdownTimeout, defaultShutdownTimeout)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv(databaseURLEnvironment, validDatabaseURL)
	t.Setenv(httpAddressEnvironment, "[::1]:9090")
	t.Setenv(shutdownTimeoutEnvironment, "25s")

	configuration, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if configuration.HTTPAddress != "[::1]:9090" {
		t.Fatalf("HTTPAddress = %q, want [::1]:9090", configuration.HTTPAddress)
	}
	if configuration.ShutdownTimeout != 25*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 25s", configuration.ShutdownTimeout)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		values      map[string]string
		wantMessage string
	}{
		{
			name:        "missing database URL",
			values:      map[string]string{},
			wantMessage: "WATCHTRACE_DATABASE_URL is required",
		},
		{
			name: "empty database URL",
			values: map[string]string{
				databaseURLEnvironment: "  ",
			},
			wantMessage: "WATCHTRACE_DATABASE_URL is required",
		},
		{
			name: "wrong database scheme",
			values: map[string]string{
				databaseURLEnvironment: "mysql://watchtrace@127.0.0.1/watchtrace",
			},
			wantMessage: "WATCHTRACE_DATABASE_URL must use the postgres or postgresql scheme",
		},
		{
			name: "database user missing",
			values: map[string]string{
				databaseURLEnvironment: "postgres://127.0.0.1/watchtrace",
			},
			wantMessage: "WATCHTRACE_DATABASE_URL must include a database user",
		},
		{
			name: "database host missing",
			values: map[string]string{
				databaseURLEnvironment: "postgres://watchtrace@/watchtrace",
			},
			wantMessage: "WATCHTRACE_DATABASE_URL must include a database host",
		},
		{
			name: "database name missing",
			values: map[string]string{
				databaseURLEnvironment: "postgres://watchtrace@127.0.0.1:5432",
			},
			wantMessage: "WATCHTRACE_DATABASE_URL must include a database name",
		},
		{
			name: "invalid HTTP address",
			values: map[string]string{
				databaseURLEnvironment: validDatabaseURL,
				httpAddressEnvironment: "127.0.0.1",
			},
			wantMessage: "WATCHTRACE_HTTP_ADDRESS must be in host:port form",
		},
		{
			name: "invalid HTTP port",
			values: map[string]string{
				databaseURLEnvironment: validDatabaseURL,
				httpAddressEnvironment: "127.0.0.1:70000",
			},
			wantMessage: "WATCHTRACE_HTTP_ADDRESS contains an invalid port",
		},
		{
			name: "invalid shutdown timeout",
			values: map[string]string{
				databaseURLEnvironment:     validDatabaseURL,
				shutdownTimeoutEnvironment: "immediately",
			},
			wantMessage: "WATCHTRACE_SHUTDOWN_TIMEOUT must be a positive duration",
		},
		{
			name: "zero shutdown timeout",
			values: map[string]string{
				databaseURLEnvironment:     validDatabaseURL,
				shutdownTimeoutEnvironment: "0s",
			},
			wantMessage: "WATCHTRACE_SHUTDOWN_TIMEOUT must be a positive duration",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := load(environment(test.values))
			if err == nil {
				t.Fatal("load succeeded, want an error")
			}
			if err.Error() != test.wantMessage {
				t.Fatalf("error = %q, want %q", err, test.wantMessage)
			}
		})
	}
}

func TestDatabaseURLErrorDoesNotExposeSecret(t *testing.T) {
	const secret = "do-not-log-this-secret"
	_, err := load(environment(map[string]string{
		databaseURLEnvironment: "postgres://watchtrace:" + secret + "@/watchtrace",
	}))
	if err == nil {
		t.Fatal("load succeeded, want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error exposed the database password: %q", err)
	}
}

func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
