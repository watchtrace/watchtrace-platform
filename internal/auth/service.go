// Package auth implements Phase 1 accounts and rotating access/refresh sessions.
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	minimumPasswordBytes  = 12
	maximumPasswordBytes  = 1024
	accessTokenLifetime   = 15 * time.Minute
	refreshTokenLifetime  = 30 * 24 * time.Hour
	verificationLifetime  = 24 * time.Hour
	passwordResetLifetime = time.Hour
	cleanupAccessBatch    = 500
	cleanupFamilyBatch    = 100
)

// DefaultCleanupInterval bounds how long expired and revoked session records
// normally remain before the API asks PostgreSQL to remove another batch.
const DefaultCleanupInterval = time.Hour

var (
	// ErrInvalidInput indicates that an account request does not satisfy the
	// service's bounded identity and password rules.
	ErrInvalidInput = errors.New("invalid account input")
	// ErrEmailInUse indicates that the normalized signup email already exists.
	ErrEmailInUse = errors.New("email already in use")
	// ErrInvalidCredentials deliberately covers both an unknown email and an
	// incorrect password so login does not disclose account existence.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrInvalidSession indicates that a bearer token is malformed, unknown, or
	// expired.
	ErrInvalidSession = errors.New("invalid session")
	// ErrInvalidRefreshToken deliberately covers malformed, unknown, expired,
	// revoked, and reused refresh tokens.
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	// ErrInvalidVerificationToken deliberately covers malformed, unknown,
	// expired, and already used email-verification tokens.
	ErrInvalidVerificationToken = errors.New("invalid email verification token")
	// ErrInvalidPasswordResetToken deliberately covers malformed, unknown,
	// expired, and already used password-reset tokens.
	ErrInvalidPasswordResetToken = errors.New("invalid password reset token")
)

type databaseConnection interface {
	database.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

// User is the safe account representation returned by authentication flows.
type User struct {
	ID            string
	Email         string
	EmailVerified bool
}

// Session contains the short-lived access token returned in JSON and the
// rotating refresh token consumed by the HTTP cookie boundary. Only digests
// of either raw token are persisted.
type Session struct {
	Token                 string
	ExpiresAt             time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
}

// Result combines the authenticated user with a newly issued session.
type Result struct {
	User    User
	Session Session
}

// Service implements signup, login, rotation, and access-token lookup.
type Service struct {
	db           databaseConnection
	actionSender AccountActionSender
}

// NewService constructs an authentication service backed by PostgreSQL.
func NewService(db databaseConnection, sender AccountActionSender) *Service {
	// Warm the fixed dummy hash once so unknown-account login follows the same
	// single Argon2id verification path as a wrong password for a known user.
	_ = dummyPasswordHash()
	return &Service{db: db, actionSender: sender}
}
