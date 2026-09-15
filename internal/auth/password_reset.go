package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

// ForgotPassword creates and delivers a replacement reset token only when the
// normalized email belongs to an account. Callers must return the same public
// response for nil and non-validation errors so neither account existence nor
// local delivery health is disclosed.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	normalizedEmail, err := validateEmail(email)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin forgot-password transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	queries := database.New(tx)
	user, err := queries.GetUserForPasswordReset(ctx, normalizedEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load password-reset identity: %w", err)
	}
	if _, err := queries.InvalidateActivePasswordResetTokens(ctx, user.ID); err != nil {
		return fmt.Errorf("invalidate password-reset tokens: %w", err)
	}
	token, digest, expiresAt, err := issuePasswordResetToken()
	if err != nil {
		return err
	}
	if err := queries.CreatePasswordResetToken(ctx, database.CreatePasswordResetTokenParams{
		UserID: user.ID, TokenDigest: digest, ExpiresAt: timestamp(expiresAt),
	}); err != nil {
		return fmt.Errorf("create password-reset token: %w", err)
	}
	if s.actionSender == nil {
		return errors.New("password-reset sender is unavailable")
	}
	if err := s.actionSender.SendPasswordReset(ctx, user.Email, token); err != nil {
		return fmt.Errorf("deliver password reset: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit forgot-password transaction: %w", err)
	}
	return nil
}

// ResetPassword atomically consumes one reset token, replaces the password,
// invalidates sibling reset tokens, and revokes every existing session.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if !validPasswordResetToken(token) {
		return ErrInvalidPasswordResetToken
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	passwordHash, err := hashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash replacement password: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password-reset transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	queries := database.New(tx)
	stored, err := queries.LockPasswordResetToken(ctx, tokenDigest(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidPasswordResetToken
	}
	if err != nil {
		return fmt.Errorf("lock password-reset token: %w", err)
	}
	if stored.UsedAt.Valid || !stored.ExpiresAt.Valid || !stored.ExpiresAt.Time.After(time.Now().UTC()) {
		return ErrInvalidPasswordResetToken
	}
	updated, err := queries.CompletePasswordReset(ctx, database.CompletePasswordResetParams{
		TokenID: stored.ID, PasswordHash: passwordHash,
	})
	if err != nil {
		return fmt.Errorf("complete password reset: %w", err)
	}
	if updated != 1 {
		return ErrInvalidPasswordResetToken
	}
	if _, err := queries.InvalidateActivePasswordResetTokens(ctx, stored.UserID); err != nil {
		return fmt.Errorf("invalidate sibling password-reset tokens: %w", err)
	}
	if err := revokeUserSessions(ctx, queries, stored.UserID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit password reset: %w", err)
	}
	return nil
}

func issuePasswordResetToken() (string, []byte, time.Time, error) {
	token, digest, err := newPasswordResetToken()
	if err != nil {
		return "", nil, time.Time{}, err
	}
	expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(passwordResetLifetime)
	return token, digest, expiresAt, nil
}
