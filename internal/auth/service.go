// Package auth implements the minimal Phase 1 account and session flow.
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

const (
	minimumPasswordBytes = 12
	maximumPasswordBytes = 1024
	sessionLifetime      = 15 * time.Minute
)

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

// Session is the raw token returned once to the caller and its expiry time.
// Only a digest of Token is persisted.
type Session struct {
	Token     string
	ExpiresAt time.Time
}

// Result combines the authenticated user with a newly issued session.
type Result struct {
	User    User
	Session Session
}

// Service implements signup, login, and lookup of the minimal bearer session.
type Service struct {
	db databaseConnection
}

// NewService constructs an authentication service backed by PostgreSQL.
func NewService(db databaseConnection) *Service {
	// Warm the fixed dummy hash once so unknown-account login follows the same
	// single Argon2id verification path as a wrong password for a known user.
	_ = dummyPasswordHash()
	return &Service{db: db}
}

// Signup creates one user and one session atomically. Ownership records are
// intentionally not created until P1-103.
func (s *Service) Signup(ctx context.Context, email, password string) (Result, error) {
	normalizedEmail, err := validateCredentials(email, password)
	if err != nil {
		return Result{}, err
	}

	passwordHash, err := hashPassword(password)
	if err != nil {
		return Result{}, fmt.Errorf("hash password: %w", err)
	}
	token, digest, expiresAt, err := issueSession()
	if err != nil {
		return Result{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin signup transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	created, err := queries.CreateUser(ctx, database.CreateUserParams{
		Email:        normalizedEmail,
		PasswordHash: passwordHash,
	})
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return Result{}, ErrEmailInUse
		}
		return Result{}, fmt.Errorf("create user: %w", err)
	}

	if err := queries.CreateAuthSession(ctx, database.CreateAuthSessionParams{
		UserID:      created.ID,
		TokenDigest: digest,
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return Result{}, fmt.Errorf("create signup session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit signup transaction: %w", err)
	}

	return Result{
		User: User{
			ID:            created.ID,
			Email:         created.Email,
			EmailVerified: created.EmailVerified,
		},
		Session: Session{Token: token, ExpiresAt: expiresAt},
	}, nil
}

// Login verifies credentials with a constant-work password check and issues a
// new short-lived session without changing ownership data.
func (s *Service) Login(ctx context.Context, email, password string) (Result, error) {
	normalizedEmail, err := validateCredentials(email, password)
	if err != nil {
		return Result{}, err
	}

	queries := database.New(s.db)
	stored, err := queries.GetUserForLogin(ctx, normalizedEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		verifyPassword(password, dummyPasswordHash())
		return Result{}, ErrInvalidCredentials
	}
	if err != nil {
		return Result{}, fmt.Errorf("load login identity: %w", err)
	}
	if !verifyPassword(password, stored.PasswordHash) {
		return Result{}, ErrInvalidCredentials
	}

	token, digest, expiresAt, err := issueSession()
	if err != nil {
		return Result{}, err
	}
	if err := queries.CreateAuthSession(ctx, database.CreateAuthSessionParams{
		UserID:      stored.ID,
		TokenDigest: digest,
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return Result{}, fmt.Errorf("create login session: %w", err)
	}

	return Result{
		User: User{
			ID:            stored.ID,
			Email:         stored.Email,
			EmailVerified: stored.EmailVerified,
		},
		Session: Session{Token: token, ExpiresAt: expiresAt},
	}, nil
}

// Authenticate resolves a valid unexpired session without exposing its stored
// digest. Protected ownership and later tenant APIs use this boundary.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if !validSessionToken(token) {
		return User{}, ErrInvalidSession
	}

	stored, err := database.New(s.db).GetUserByAuthSession(ctx, sessionTokenDigest(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidSession
	}
	if err != nil {
		return User{}, fmt.Errorf("load authenticated session: %w", err)
	}

	return User{
		ID:            stored.ID,
		Email:         stored.Email,
		EmailVerified: stored.EmailVerified,
	}, nil
}

func issueSession() (string, []byte, time.Time, error) {
	token, digest, err := newSessionToken()
	if err != nil {
		return "", nil, time.Time{}, err
	}
	return token, digest, time.Now().UTC().Add(sessionLifetime), nil
}

func validateCredentials(email, password string) (string, error) {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	if normalizedEmail == "" || len(normalizedEmail) > 254 {
		return "", ErrInvalidInput
	}
	parsed, err := mail.ParseAddress(normalizedEmail)
	if err != nil || parsed.Address != normalizedEmail {
		return "", ErrInvalidInput
	}
	if len(password) < minimumPasswordBytes || len(password) > maximumPasswordBytes {
		return "", ErrInvalidInput
	}

	return normalizedEmail, nil
}

func validSessionToken(token string) bool {
	if !strings.HasPrefix(token, sessionTokenPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, sessionTokenPrefix))
	return err == nil && len(raw) == sessionTokenBytes
}
