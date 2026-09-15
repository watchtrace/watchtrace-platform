package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

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

// Logout revokes either the refresh token's current family or every family for
// its user. Unknown and malformed tokens are successful no-ops so logout does
// not expose session validity. An inactive token can revoke only its own family;
// account-wide revocation requires an active refresh token.
func (s *Service) Logout(ctx context.Context, refreshToken string, allSessions bool) error {
	if !validRefreshToken(refreshToken) {
		return nil
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin logout transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	stored, err := queries.LockRefreshTokenForRotation(ctx, tokenDigest(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock logout refresh token: %w", err)
	}

	active := !stored.RotatedAt.Valid && !stored.RevokedAt.Valid &&
		stored.ExpiresAt.Valid && stored.ExpiresAt.Time.After(time.Now().UTC())
	if allSessions && active {
		if err := revokeUserSessions(ctx, queries, stored.UserID); err != nil {
			return err
		}
	} else if err := revokeTokenFamily(ctx, queries, stored.FamilyID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit logout transaction: %w", err)
	}
	return nil
}

// CleanupSessions deletes a bounded batch of expired or revoked access tokens
// and fully expired refresh-token families. Unexpired refresh rows are retained
// after revocation so reuse detection remains possible for their lifetime.
func (s *Service) CleanupSessions(ctx context.Context) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin session cleanup transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	queries := database.New(tx)
	accessCount, err := queries.DeleteExpiredOrRevokedAccessTokens(ctx, cleanupAccessBatch)
	if err != nil {
		return 0, fmt.Errorf("delete expired or revoked access tokens: %w", err)
	}
	refreshCount, err := queries.DeleteExpiredRefreshTokenFamilies(ctx, cleanupFamilyBatch)
	if err != nil {
		return 0, fmt.Errorf("delete expired refresh token families: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit session cleanup transaction: %w", err)
	}
	return accessCount + refreshCount, nil
}
