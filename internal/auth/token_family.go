package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

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

func revokeUserSessions(ctx context.Context, queries *database.Queries, userID string) error {
	if _, err := queries.RevokeRefreshTokensForUser(ctx, userID); err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	if _, err := queries.RevokeAccessTokensForUser(ctx, userID); err != nil {
		return fmt.Errorf("revoke user access tokens: %w", err)
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
