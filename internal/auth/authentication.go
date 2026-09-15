package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	database "github.com/watchtrace/watchtrace-platform/internal/platform/database/sqlc"
)

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
