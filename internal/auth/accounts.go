package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

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
	verificationToken, verificationDigest, verificationExpiresAt, err := issueVerificationToken()
	if err != nil {
		return Result{}, err
	}
	if err := queries.CreateEmailVerificationToken(ctx, database.CreateEmailVerificationTokenParams{
		UserID:      created.ID,
		TokenDigest: verificationDigest,
		ExpiresAt:   timestamp(verificationExpiresAt),
	}); err != nil {
		return Result{}, fmt.Errorf("create email verification token: %w", err)
	}
	if s.actionSender == nil {
		return Result{}, errors.New("email verification sender is unavailable")
	}
	if err := s.actionSender.SendVerification(ctx, created.Email, verificationToken); err != nil {
		return Result{}, fmt.Errorf("deliver email verification: %w", err)
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

func validateCredentials(email, password string) (string, error) {
	normalizedEmail, err := validateEmail(email)
	if err != nil {
		return "", err
	}
	if err := validatePassword(password); err != nil {
		return "", err
	}
	return normalizedEmail, nil
}

func validateEmail(email string) (string, error) {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	if normalizedEmail == "" || len(normalizedEmail) > 254 {
		return "", ErrInvalidInput
	}
	parsed, err := mail.ParseAddress(normalizedEmail)
	if err != nil || parsed.Address != normalizedEmail {
		return "", ErrInvalidInput
	}
	return normalizedEmail, nil
}

func validatePassword(password string) error {
	if len(password) < minimumPasswordBytes || len(password) > maximumPasswordBytes {
		return ErrInvalidInput
	}
	return nil
}
