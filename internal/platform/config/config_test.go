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
	if configuration.Production {
		t.Fatal("default development environment enabled production cookies")
	}
	if configuration.VerificationSMTPAddress != defaultVerificationSMTP ||
		configuration.VerificationProvider != "local" ||
		configuration.VerificationFrom != defaultVerificationFrom ||
		configuration.VerificationURL != defaultVerificationURL ||
		configuration.PasswordResetURL != defaultPasswordResetURL ||
		configuration.InvitationURL != defaultInvitationURL {
		t.Fatalf("unexpected verification defaults: %+v", configuration)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv(databaseURLEnvironment, validDatabaseURL)
	t.Setenv(httpAddressEnvironment, "[::1]:9090")
	t.Setenv(shutdownTimeoutEnvironment, "25s")
	t.Setenv(deploymentEnvironment, "production")
	t.Setenv(verificationSMTPEnvironment, "127.0.0.1:2025")
	t.Setenv(verificationProviderEnvironment, "oci")
	t.Setenv(verificationUsernameEnvironment, "smtp-user")
	t.Setenv(verificationPasswordEnvironment, "smtp-password")
	t.Setenv(verificationFromEnvironment, "local@example.test")
	t.Setenv(verificationURLEnvironment, "http://localhost:3001/verify")
	t.Setenv(passwordResetURLEnvironment, "http://localhost:3001/reset")
	t.Setenv(invitationURLEnvironment, "http://localhost:3001/invite")
	t.Setenv(monitorHeaderKeyEnvironment, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv(platformSigningKeyEnvironment, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA7aie8zrakLWKjqNAqbw1zZTIVdx3iQ6Y6wEihi1naKQ==")

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
	if !configuration.Production {
		t.Fatal("production environment did not enable production cookie security")
	}
	if configuration.VerificationSMTPAddress != "127.0.0.1:2025" ||
		configuration.VerificationProvider != "oci" ||
		configuration.VerificationSMTPUsername != "smtp-user" ||
		configuration.VerificationSMTPPassword != "smtp-password" ||
		configuration.VerificationFrom != "local@example.test" ||
		configuration.VerificationURL != "http://localhost:3001/verify" ||
		configuration.PasswordResetURL != "http://localhost:3001/reset" ||
		configuration.InvitationURL != "http://localhost:3001/invite" {
		t.Fatalf("verification configuration was not loaded: %+v", configuration)
	}
}

func TestLoadDatabaseURLIgnoresUnrelatedApplicationSettings(t *testing.T) {
	t.Setenv(databaseURLEnvironment, validDatabaseURL)
	t.Setenv(httpAddressEnvironment, "not-a-listen-address")
	t.Setenv(shutdownTimeoutEnvironment, "not-a-duration")

	databaseURL, err := LoadDatabaseURL()
	if err != nil {
		t.Fatalf("LoadDatabaseURL: %v", err)
	}
	if databaseURL != validDatabaseURL {
		t.Fatalf("database URL = %q, want supplied URL", databaseURL)
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
		{
			name: "invalid deployment environment",
			values: map[string]string{
				databaseURLEnvironment: validDatabaseURL,
				deploymentEnvironment:  "staging",
			},
			wantMessage: "WATCHTRACE_ENVIRONMENT must be development or production",
		},
		{
			name: "empty verification sender",
			values: map[string]string{
				databaseURLEnvironment:      validDatabaseURL,
				verificationFromEnvironment: " ",
			},
			wantMessage: "email verification settings must not be empty",
		},
		{
			name: "invalid verification provider",
			values: map[string]string{
				databaseURLEnvironment:          validDatabaseURL,
				verificationProviderEnvironment: "ses",
			},
			wantMessage: "WATCHTRACE_VERIFICATION_PROVIDER must be local or oci",
		},
		{
			name: "missing OCI verification credentials",
			values: map[string]string{
				databaseURLEnvironment:          validDatabaseURL,
				verificationProviderEnvironment: "oci",
			},
			wantMessage: "OCI verification SMTP credentials are required",
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

func TestProductionRequiresExternalMonitoringKeys(t *testing.T) {
	_, err := load(environment(map[string]string{
		databaseURLEnvironment:      validDatabaseURL,
		deploymentEnvironment:       "production",
		monitorHeaderKeyEnvironment: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	}))
	if err == nil || err.Error() != "WATCHTRACE_PLATFORM_SIGNING_PRIVATE_KEY is required in production" {
		t.Fatalf("production signing-key error = %v", err)
	}
}

func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
