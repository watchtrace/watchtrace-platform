// Package config loads and validates process configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	databaseURLEnvironment      = "WATCHTRACE_DATABASE_URL"
	httpAddressEnvironment      = "WATCHTRACE_HTTP_ADDRESS"
	shutdownTimeoutEnvironment  = "WATCHTRACE_SHUTDOWN_TIMEOUT"
	deploymentEnvironment       = "WATCHTRACE_ENVIRONMENT"
	verificationSMTPEnvironment = "WATCHTRACE_VERIFICATION_SMTP_ADDRESS"
	verificationFromEnvironment = "WATCHTRACE_VERIFICATION_FROM"
	verificationURLEnvironment  = "WATCHTRACE_VERIFICATION_URL"
	passwordResetURLEnvironment = "WATCHTRACE_PASSWORD_RESET_URL"
	invitationURLEnvironment    = "WATCHTRACE_INVITATION_URL"

	defaultHTTPAddress      = "127.0.0.1:8080"
	defaultShutdownTimeout  = 10 * time.Second
	defaultEnvironment      = "development"
	defaultVerificationSMTP = "127.0.0.1:1025"
	defaultVerificationFrom = "watchtrace@localhost"
	defaultVerificationURL  = "http://127.0.0.1:3000/verify-email"
	defaultPasswordResetURL = "http://127.0.0.1:3000/reset-password"
	defaultInvitationURL    = "http://127.0.0.1:3000/accept-invitation"
)

// Config contains the validated settings needed to start the API.
type Config struct {
	DatabaseURL             string
	HTTPAddress             string
	ShutdownTimeout         time.Duration
	Production              bool
	VerificationSMTPAddress string
	VerificationFrom        string
	VerificationURL         string
	PasswordResetURL        string
	InvitationURL           string
}

// Load reads configuration from the process environment and validates it.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// LoadDatabaseURL reads and validates only the database setting needed by
// database administration commands.
func LoadDatabaseURL() (string, error) {
	return loadDatabaseURL(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	databaseURL, err := loadDatabaseURL(lookup)
	if err != nil {
		return Config{}, err
	}

	httpAddress := defaultHTTPAddress
	if value, exists := lookup(httpAddressEnvironment); exists {
		httpAddress = strings.TrimSpace(value)
		if httpAddress == "" {
			return Config{}, fmt.Errorf("%s must not be empty", httpAddressEnvironment)
		}
	}
	if err := validateHTTPAddress(httpAddress); err != nil {
		return Config{}, err
	}

	shutdownTimeout := defaultShutdownTimeout
	if value, exists := lookup(shutdownTimeoutEnvironment); exists {
		value = strings.TrimSpace(value)
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive duration", shutdownTimeoutEnvironment)
		}
		shutdownTimeout = parsed
	}

	environment := defaultEnvironment
	if value, exists := lookup(deploymentEnvironment); exists {
		environment = strings.ToLower(strings.TrimSpace(value))
	}
	if environment != "development" && environment != "production" {
		return Config{}, fmt.Errorf("%s must be development or production", deploymentEnvironment)
	}

	verificationSMTP := stringSetting(lookup, verificationSMTPEnvironment, defaultVerificationSMTP)
	verificationFrom := stringSetting(lookup, verificationFromEnvironment, defaultVerificationFrom)
	verificationURL := stringSetting(lookup, verificationURLEnvironment, defaultVerificationURL)
	passwordResetURL := stringSetting(lookup, passwordResetURLEnvironment, defaultPasswordResetURL)
	invitationURL := stringSetting(lookup, invitationURLEnvironment, defaultInvitationURL)
	if verificationSMTP == "" || verificationFrom == "" || verificationURL == "" || passwordResetURL == "" || invitationURL == "" {
		return Config{}, errors.New("email verification settings must not be empty")
	}

	return Config{
		DatabaseURL:             databaseURL,
		HTTPAddress:             httpAddress,
		ShutdownTimeout:         shutdownTimeout,
		Production:              environment == "production",
		VerificationSMTPAddress: verificationSMTP,
		VerificationFrom:        verificationFrom,
		VerificationURL:         verificationURL,
		PasswordResetURL:        passwordResetURL,
		InvitationURL:           invitationURL,
	}, nil
}

func stringSetting(lookup func(string) (string, bool), name, fallback string) string {
	if value, exists := lookup(name); exists {
		return strings.TrimSpace(value)
	}
	return fallback
}

func loadDatabaseURL(lookup func(string) (string, bool)) (string, error) {
	databaseURL, ok := lookup(databaseURLEnvironment)
	databaseURL = strings.TrimSpace(databaseURL)
	if !ok || databaseURL == "" {
		return "", fmt.Errorf("%s is required", databaseURLEnvironment)
	}
	if err := validateDatabaseURL(databaseURL); err != nil {
		return "", err
	}

	return databaseURL, nil
}

func validateDatabaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s must be a valid PostgreSQL URL", databaseURLEnvironment)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("%s must use the postgres or postgresql scheme", databaseURLEnvironment)
	}
	if parsed.User == nil || parsed.User.Username() == "" {
		return fmt.Errorf("%s must include a database user", databaseURLEnvironment)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("%s must include a database host", databaseURLEnvironment)
	}
	if parsed.Port() != "" {
		port, portErr := strconv.Atoi(parsed.Port())
		if portErr != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s contains an invalid database port", databaseURLEnvironment)
		}
	}
	if strings.Trim(parsed.Path, "/") == "" {
		return fmt.Errorf("%s must include a database name", databaseURLEnvironment)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a fragment", databaseURLEnvironment)
	}

	return nil
}

func validateHTTPAddress(value string) error {
	host, portValue, err := net.SplitHostPort(value)
	if err != nil || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("%s must be in host:port form", httpAddressEnvironment)
	}

	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s contains an invalid port", httpAddressEnvironment)
	}

	return nil
}
