package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
)

const authenticatedUserKey = "authenticated_user"

// SessionAuthenticator resolves the bearer session used by protected routes.
type SessionAuthenticator interface {
	Authenticate(context.Context, string) (auth.User, error)
}

func requireAuthenticatedUser(authenticator SessionAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			respondInvalidSession(c)
			return
		}

		user, err := authenticator.Authenticate(c.Request.Context(), token)
		if errors.Is(err, auth.ErrInvalidSession) {
			respondInvalidSession(c)
			return
		}
		if err != nil {
			RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}

		c.Set(authenticatedUserKey, user)
		c.Next()
	}
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func authenticatedUser(c *gin.Context) (auth.User, bool) {
	value, exists := c.Get(authenticatedUserKey)
	if !exists {
		return auth.User{}, false
	}
	user, ok := value.(auth.User)
	return user, ok
}

func respondInvalidSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("WWW-Authenticate", "Bearer")
	RespondError(c, http.StatusUnauthorized, "invalid_session", "a valid bearer session is required")
}
