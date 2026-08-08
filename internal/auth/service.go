// Package auth implements Phase 1 accounts and rotating access/refresh sessions.
package auth

import (
	"context"
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
	accessTokenLifetime  = 15 * time.Minute
	refreshTokenLifetime = 30 * 24 * time.Hour
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
	// ErrInvalidRefreshToken deliberately covers malformed, unknown, expired,
	// revoked, and reused refresh tokens.
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
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
	db databaseConnection
}

// NewService constructs an authentication service backed by PostgreSQL.
func NewService(db databaseConnection) *Service {
	// Warm the fixed dummy hash once so unknown-account login follows the same
	// single Argon2id verification path as a wrong password for a known user.
	_ = dummyPasswordHash()
	return &Service{db: db}
}

// Signup creates one user and one access/refresh token family atomically.
func (s *Service) Signup(ctx context.Context, email, password string) (Result, error) {
	normalizedEmail, err := validateCredentials(email, password)
	if err != nil {
		return Result{}, err
	}

	passwordHash, err := hashPassword(password)
	if err != nil {
		return Result{}, fmt.Errorf("hash password: %w", err)
	}
	tokens, err := issueTokenPair()
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

	if err := persistNewTokenFamily(ctx, queries, created.ID, tokens); err != nil {
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
		Session: sessionFromTokenPair(tokens),
	}, nil
}

// Login verifies credentials with a constant-work password check and issues a
// new access/refresh token family without changing ownership data.
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

	session, err := s.createTokenFamily(ctx, stored.ID)
	if err != nil {
		return Result{}, fmt.Errorf("create login session: %w", err)
	}

	return Result{
		User: User{
			ID:            stored.ID,
			Email:         stored.Email,
			EmailVerified: stored.EmailVerified,
		},
		Session: session,
	}, nil
}

// Authenticate resolves a valid unexpired session without exposing its stored
// digest. Protected ownership and later tenant APIs use this boundary.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if !validAccessToken(token) {
		return User{}, ErrInvalidSession
	}

	stored, err := database.New(s.db).GetUserByAuthSession(ctx, tokenDigest(token))
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

// Refresh rotates one active refresh token and issues a new access token. A
// replay of an already-rotated token revokes all access and refresh tokens in
// its family before returning the same public error as any invalid token.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Result, error) {
	if !validRefreshToken(refreshToken) {
		return Result{}, ErrInvalidRefreshToken
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin refresh transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	stored, err := queries.LockRefreshTokenForRotation(ctx, tokenDigest(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrInvalidRefreshToken
	}
	if err != nil {
		return Result{}, fmt.Errorf("lock refresh token: %w", err)
	}
	if stored.RotatedAt.Valid {
		if err := revokeTokenFamily(ctx, queries, stored.FamilyID); err != nil {
			return Result{}, fmt.Errorf("revoke reused refresh token family: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit reused refresh token revocation: %w", err)
		}
		return Result{}, ErrInvalidRefreshToken
	}
	if stored.RevokedAt.Valid || !stored.ExpiresAt.Valid ||
		!stored.ExpiresAt.Time.After(time.Now().UTC()) {
		return Result{}, ErrInvalidRefreshToken
	}

	tokens, err := issueTokenPair()
	if err != nil {
		return Result{}, err
	}
	replacementID, err := queries.CreateRotatedRefreshToken(
		ctx,
		database.CreateRotatedRefreshTokenParams{
			UserID:      stored.UserID,
			FamilyID:    stored.FamilyID,
			TokenDigest: tokens.RefreshDigest,
			ExpiresAt:   timestamp(tokens.RefreshExpiresAt),
		},
	)
	if err != nil {
		return Result{}, fmt.Errorf("create rotated refresh token: %w", err)
	}
	rotated, err := queries.MarkRefreshTokenRotated(ctx, database.MarkRefreshTokenRotatedParams{
		ReplacedByID: replacementID,
		ID:           stored.ID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("mark refresh token rotated: %w", err)
	}
	if rotated != 1 {
		return Result{}, errors.New("mark refresh token rotated affected an unexpected number of rows")
	}
	if err := createAccessToken(ctx, queries, stored.UserID, stored.FamilyID, tokens); err != nil {
		return Result{}, fmt.Errorf("create refreshed access token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit refresh transaction: %w", err)
	}

	return Result{
		User: User{
			ID:            stored.UserID,
			Email:         stored.Email,
			EmailVerified: stored.EmailVerified,
		},
		Session: sessionFromTokenPair(tokens),
	}, nil
}

type tokenPair struct {
	AccessToken      string
	AccessDigest     []byte
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshDigest    []byte
	RefreshExpiresAt time.Time
}

func issueTokenPair() (tokenPair, error) {
	accessToken, accessDigest, err := newAccessToken()
	if err != nil {
		return tokenPair{}, err
	}
	refreshToken, refreshDigest, err := newRefreshToken()
	if err != nil {
		return tokenPair{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	return tokenPair{
		AccessToken:      accessToken,
		AccessDigest:     accessDigest,
		AccessExpiresAt:  now.Add(accessTokenLifetime),
		RefreshToken:     refreshToken,
		RefreshDigest:    refreshDigest,
		RefreshExpiresAt: now.Add(refreshTokenLifetime),
	}, nil
}

func (s *Service) createTokenFamily(ctx context.Context, userID string) (Session, error) {
	tokens, err := issueTokenPair()
	if err != nil {
		return Session{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin token family transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()
	if err := persistNewTokenFamily(ctx, database.New(tx), userID, tokens); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit token family transaction: %w", err)
	}
	return sessionFromTokenPair(tokens), nil
}

func persistNewTokenFamily(
	ctx context.Context,
	queries *database.Queries,
	userID string,
	tokens tokenPair,
) error {
	refresh, err := queries.CreateRefreshTokenFamily(ctx, database.CreateRefreshTokenFamilyParams{
		UserID:      userID,
		TokenDigest: tokens.RefreshDigest,
		ExpiresAt:   timestamp(tokens.RefreshExpiresAt),
	})
	if err != nil {
		return fmt.Errorf("create refresh token family: %w", err)
	}
	return createAccessToken(ctx, queries, userID, refresh.FamilyID, tokens)
}

func createAccessToken(
	ctx context.Context,
	queries *database.Queries,
	userID string,
	familyID string,
	tokens tokenPair,
) error {
	if err := queries.CreateAuthSession(ctx, database.CreateAuthSessionParams{
		UserID:      userID,
		FamilyID:    familyID,
		TokenDigest: tokens.AccessDigest,
		ExpiresAt:   timestamp(tokens.AccessExpiresAt),
	}); err != nil {
		return fmt.Errorf("create access token: %w", err)
	}
	return nil
}

func revokeTokenFamily(ctx context.Context, queries *database.Queries, familyID string) error {
	if _, err := queries.RevokeRefreshTokenFamily(ctx, familyID); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	if _, err := queries.RevokeAccessTokenFamily(ctx, familyID); err != nil {
		return fmt.Errorf("revoke access tokens: %w", err)
	}
	return nil
}

func sessionFromTokenPair(tokens tokenPair) Session {
	return Session{
		Token:                 tokens.AccessToken,
		ExpiresAt:             tokens.AccessExpiresAt,
		RefreshToken:          tokens.RefreshToken,
		RefreshTokenExpiresAt: tokens.RefreshExpiresAt,
	}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
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
