package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
)

// AuthenticationService is the account boundary used by the HTTP API.
type AuthenticationService interface {
	Signup(context.Context, string, string) (auth.Result, error)
	Login(context.Context, string, string) (auth.Result, error)
}

type credentialsRequest struct {
	Email    string `json:"email" binding:"required,email,max=254"`
	Password string `json:"password" binding:"required,min=12,max=1024"`
}

type authResponse struct {
	User    authUserResponse    `json:"user"`
	Session authSessionResponse `json:"session"`
}

type authUserResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

type authSessionResponse struct {
	Token     string    `json:"token"`
	TokenType string    `json:"token_type"`
	ExpiresAt time.Time `json:"expires_at"`
}

func registerAuthRoutes(router *gin.Engine, service AuthenticationService) {
	router.POST("/api/v1/auth/signup", authEndpoint(service.Signup, http.StatusCreated))
	router.POST("/api/v1/auth/login", authEndpoint(service.Login, http.StatusOK))
}

func authEndpoint(
	operation func(context.Context, string, string) (auth.Result, error),
	successStatus int,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")

		var request credentialsRequest
		if !DecodeJSON(c, &request) {
			return
		}

		result, err := operation(c.Request.Context(), request.Email, request.Password)
		if err != nil {
			respondAuthError(c, err)
			return
		}

		c.JSON(successStatus, authResponse{
			User: authUserResponse{
				ID:            result.User.ID,
				Email:         result.User.Email,
				EmailVerified: result.User.EmailVerified,
			},
			Session: authSessionResponse{
				Token:     result.Session.Token,
				TokenType: "Bearer",
				ExpiresAt: result.Session.ExpiresAt,
			},
		})
	}
}

func respondAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		RespondError(c, http.StatusUnprocessableEntity, "validation_failed", "email or password is invalid")
	case errors.Is(err, auth.ErrEmailInUse):
		RespondError(c, http.StatusConflict, "email_in_use", "an account with this email already exists")
	case errors.Is(err, auth.ErrInvalidCredentials):
		RespondError(c, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
	default:
		RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
	}
}
