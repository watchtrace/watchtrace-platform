package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

// VerifyEmail consumes one valid unexpired verification token and marks the
// associated user verified in the same transaction.
func (s *Service) VerifyEmail(ctx context.Context, token string) (User, error) {
	if !validVerificationToken(token) {
		return User{}, ErrInvalidVerificationToken
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("begin email verification transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	stored, err := queries.LockEmailVerificationToken(ctx, tokenDigest(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidVerificationToken
	}
	if err != nil {
		return User{}, fmt.Errorf("lock email verification token: %w", err)
	}
	if stored.UsedAt.Valid || !stored.ExpiresAt.Valid ||
		!stored.ExpiresAt.Time.After(time.Now().UTC()) {
		return User{}, ErrInvalidVerificationToken
	}

	verified, err := queries.CompleteEmailVerification(ctx, stored.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidVerificationToken
	}
	if err != nil {
		return User{}, fmt.Errorf("complete email verification: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("commit email verification: %w", err)
	}
	return User{ID: verified.ID, Email: verified.Email, EmailVerified: verified.EmailVerified}, nil
}

func issueVerificationToken() (string, []byte, time.Time, error) {
	token, digest, err := newVerificationToken()
	if err != nil {
		return "", nil, time.Time{}, err
	}
	expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(verificationLifetime)
	return token, digest, expiresAt, nil
}
