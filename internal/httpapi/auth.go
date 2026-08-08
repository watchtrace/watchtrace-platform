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
	Refresh(context.Context, string) (auth.Result, error)
	Logout(context.Context, string, bool) error
	VerifyEmail(context.Context, string) (auth.User, error)
}

const refreshTokenCookieName = "watchtrace_refresh"

type credentialsRequest struct {
	Email    string `json:"email" binding:"required,email,max=254"`
	Password string `json:"password" binding:"required,min=12,max=1024"`
}

type logoutRequest struct {
	AllSessions bool `json:"all_sessions"`
}

type verifyEmailRequest struct {
	Token string `json:"token" binding:"required,max=128"`
}

type verifyEmailResponse struct {
	User authUserResponse `json:"user"`
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

func registerAuthRoutes(router *gin.Engine, service AuthenticationService, secureCookies bool) {
	router.POST("/api/v1/auth/signup", authEndpoint(service.Signup, http.StatusCreated, secureCookies))
	router.POST("/api/v1/auth/login", authEndpoint(service.Login, http.StatusOK, secureCookies))
	router.POST("/api/v1/auth/refresh", refreshAuth(service, secureCookies))
	router.POST("/api/v1/auth/logout", logoutAuth(service, secureCookies))
	router.POST("/api/v1/auth/verify-email", verifyEmailAuth(service))
}

func verifyEmailAuth(service AuthenticationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request verifyEmailRequest
		if !DecodeJSON(c, &request) {
			return
		}

		verified, err := service.VerifyEmail(c.Request.Context(), request.Token)
		if err != nil {
			respondAuthError(c, err)
			return
		}
		c.JSON(http.StatusOK, verifyEmailResponse{User: authUserResponse{
			ID:            verified.ID,
			Email:         verified.Email,
			EmailVerified: verified.EmailVerified,
		}})
	}
}

func logoutAuth(service AuthenticationService, secureCookies bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request logoutRequest
		if !DecodeJSON(c, &request) {
			return
		}

		refreshToken, _ := c.Cookie(refreshTokenCookieName)
		if err := service.Logout(c.Request.Context(), refreshToken, request.AllSessions); err != nil {
			respondAuthError(c, err)
			return
		}

		clearRefreshTokenCookie(c, secureCookies)
		c.Status(http.StatusNoContent)
	}
}

func authEndpoint(
	operation func(context.Context, string, string) (auth.Result, error),
	successStatus int,
	secureCookies bool,
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

		setRefreshTokenCookie(c, result.Session, secureCookies)
		respondAuthSuccess(c, successStatus, result)
	}
}

func refreshAuth(service AuthenticationService, secureCookies bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		refreshToken, err := c.Cookie(refreshTokenCookieName)
		if err != nil {
			clearRefreshTokenCookie(c, secureCookies)
			respondAuthError(c, auth.ErrInvalidRefreshToken)
			return
		}

		result, err := service.Refresh(c.Request.Context(), refreshToken)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidRefreshToken) {
				clearRefreshTokenCookie(c, secureCookies)
			}
			respondAuthError(c, err)
			return
		}

		setRefreshTokenCookie(c, result.Session, secureCookies)
		respondAuthSuccess(c, http.StatusOK, result)
	}
}

func respondAuthSuccess(c *gin.Context, status int, result auth.Result) {
	c.JSON(status, authResponse{
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

func setRefreshTokenCookie(c *gin.Context, session auth.Session, secure bool) {
	maxAge := int(time.Until(session.RefreshTokenExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     refreshTokenCookieName,
		Value:    session.RefreshToken,
		Path:     "/api/v1/auth",
		Expires:  session.RefreshTokenExpiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearRefreshTokenCookie(c *gin.Context, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     refreshTokenCookieName,
		Path:     "/api/v1/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func respondAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		RespondError(c, http.StatusUnprocessableEntity, "validation_failed", "email or password is invalid")
	case errors.Is(err, auth.ErrEmailInUse):
		RespondError(c, http.StatusConflict, "email_in_use", "an account with this email already exists")
	case errors.Is(err, auth.ErrInvalidCredentials):
		RespondError(c, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
	case errors.Is(err, auth.ErrInvalidRefreshToken):
		RespondError(c, http.StatusUnauthorized, "invalid_refresh_token", "a valid refresh token is required")
	case errors.Is(err, auth.ErrInvalidVerificationToken):
		RespondError(c, http.StatusBadRequest, "invalid_verification_token", "a valid unused email verification token is required")
	default:
		RespondError(c, http.StatusInternalServerError, "internal_error", "an internal error occurred")
	}
}
