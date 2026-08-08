package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/httpapi"
)

func TestSignupAndLoginAPIWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL connection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		email    = "auth-integration@example.test"
		password = "P1-102-test-password!"
	)
	deleteAuthTestUser(t, ctx, pool, email)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		deleteAuthTestUser(t, cleanupCtx, pool, email)
	})

	service := auth.NewService(pool, &recordingVerificationSender{})
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{
		Logger:        slog.New(slog.NewJSONHandler(&logs, nil)),
		AuthService:   service,
		SecureCookies: true,
	})

	signup := performAuthRequest(t, router, "/api/v1/auth/signup", email, password)
	if signup.Status != http.StatusCreated {
		t.Fatalf("signup status = %d, want %d: %s", signup.Status, http.StatusCreated, signup.RawBody)
	}
	if signup.Body.User.Email != email || signup.Body.User.EmailVerified {
		t.Fatalf("unexpected signup user: %+v", signup.Body.User)
	}
	if signup.Body.Session.Token == "" || signup.Body.Session.TokenType != "Bearer" {
		t.Fatalf("unexpected signup session: %+v", signup.Body.Session)
	}
	if signup.Body.Session.ExpiresAt.Before(time.Now().Add(14 * time.Minute)) {
		t.Fatalf("signup session expires too soon: %s", signup.Body.Session.ExpiresAt)
	}
	signupRefreshToken := requireRefreshCookie(t, signup, true)
	if strings.Contains(signup.RawBody, signupRefreshToken) {
		t.Fatal("signup JSON exposed its refresh token")
	}

	var passwordHash string
	var accessDigest []byte
	var refreshDigest []byte
	var storedAccessExpiry time.Time
	var storedRefreshExpiry time.Time
	var accessFamilyID string
	var refreshFamilyID string
	err = pool.QueryRow(ctx, `
		SELECT
			users.password_hash,
			auth_sessions.token_digest,
			auth_sessions.expires_at,
			auth_sessions.family_id::text,
			refresh_tokens.token_digest,
			refresh_tokens.expires_at,
			refresh_tokens.family_id::text
		FROM users
		JOIN auth_sessions ON auth_sessions.user_id = users.id
		JOIN refresh_tokens ON refresh_tokens.user_id = users.id
		WHERE users.email = $1
	`, email).Scan(
		&passwordHash,
		&accessDigest,
		&storedAccessExpiry,
		&accessFamilyID,
		&refreshDigest,
		&storedRefreshExpiry,
		&refreshFamilyID,
	)
	if err != nil {
		t.Fatalf("inspect stored credentials: %v", err)
	}
	if passwordHash == password || !strings.HasPrefix(passwordHash, "$argon2id$") {
		t.Fatal("database does not contain an Argon2id password hash")
	}
	expectedAccessDigest := sha256.Sum256([]byte(signup.Body.Session.Token))
	if !bytes.Equal(accessDigest, expectedAccessDigest[:]) {
		t.Fatal("database access digest does not match the returned token")
	}
	expectedRefreshDigest := sha256.Sum256([]byte(signupRefreshToken))
	if !bytes.Equal(refreshDigest, expectedRefreshDigest[:]) {
		t.Fatal("database refresh digest does not match the cookie token")
	}
	if bytes.Contains(accessDigest, []byte(signup.Body.Session.Token)) ||
		bytes.Contains(refreshDigest, []byte(signupRefreshToken)) {
		t.Fatal("database contains a raw access or refresh token")
	}
	if !storedAccessExpiry.Equal(signup.Body.Session.ExpiresAt) {
		t.Fatalf("stored expiry %s differs from API expiry %s", storedAccessExpiry, signup.Body.Session.ExpiresAt)
	}
	if storedRefreshExpiry.Before(time.Now().Add(29*24*time.Hour)) || accessFamilyID != refreshFamilyID {
		t.Fatalf("unexpected refresh expiry or token family: %s %q/%q",
			storedRefreshExpiry, accessFamilyID, refreshFamilyID)
	}

	authenticatedUser, err := service.Authenticate(ctx, signup.Body.Session.Token)
	if err != nil {
		t.Fatalf("authenticate signup session: %v", err)
	}
	if authenticatedUser.ID != signup.Body.User.ID {
		t.Fatalf("authenticated user ID = %q, want %q", authenticatedUser.ID, signup.Body.User.ID)
	}

	refreshed := performRefreshRequest(t, router, signupRefreshToken)
	if refreshed.Status != http.StatusOK || refreshed.Body.User.ID != signup.Body.User.ID ||
		refreshed.Body.Session.Token == signup.Body.Session.Token {
		t.Fatalf("refresh response is incorrect: status=%d body=%s", refreshed.Status, refreshed.RawBody)
	}
	rotatedRefreshToken := requireRefreshCookie(t, refreshed, true)
	if rotatedRefreshToken == signupRefreshToken || strings.Contains(refreshed.RawBody, rotatedRefreshToken) {
		t.Fatal("refresh token was not rotated exclusively through the cookie")
	}
	if _, err := service.Authenticate(ctx, refreshed.Body.Session.Token); err != nil {
		t.Fatalf("authenticate refreshed access token: %v", err)
	}

	reused := performRefreshRequest(t, router, signupRefreshToken)
	assertAuthAPIError(t, reused, http.StatusUnauthorized, "invalid_refresh_token")
	if len(reused.Cookies) != 1 || reused.Cookies[0].MaxAge != -1 {
		t.Fatalf("reused refresh cookie was not cleared: %+v", reused.Cookies)
	}
	if _, err := service.Authenticate(ctx, refreshed.Body.Session.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("reused refresh token left family access valid: %v", err)
	}
	revokedReplacement := performRefreshRequest(t, router, rotatedRefreshToken)
	assertAuthAPIError(t, revokedReplacement, http.StatusUnauthorized, "invalid_refresh_token")

	var activeRefreshTokens int
	var activeAccessTokens int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE revoked_at IS NULL),
			(SELECT count(*) FROM auth_sessions WHERE family_id = $1::text::uuid AND revoked_at IS NULL)
		FROM refresh_tokens
		WHERE family_id = $1::text::uuid
	`, refreshFamilyID).Scan(&activeRefreshTokens, &activeAccessTokens); err != nil {
		t.Fatalf("inspect revoked token family: %v", err)
	}
	if activeRefreshTokens != 0 || activeAccessTokens != 0 {
		t.Fatalf("reused family retains refresh/access tokens: %d/%d", activeRefreshTokens, activeAccessTokens)
	}

	login := performAuthRequest(t, router, "/api/v1/auth/login", strings.ToUpper(email), password)
	if login.Status != http.StatusOK {
		t.Fatalf("login status = %d, want %d: %s", login.Status, http.StatusOK, login.RawBody)
	}
	if login.Body.Session.Token == signup.Body.Session.Token {
		t.Fatal("login reused the signup session token")
	}
	loginRefreshToken := requireRefreshCookie(t, login, true)
	if loginRefreshToken == signupRefreshToken || loginRefreshToken == rotatedRefreshToken {
		t.Fatal("login reused a previous refresh token")
	}

	duplicate := performAuthRequest(t, router, "/api/v1/auth/signup", email, password)
	assertAuthAPIError(t, duplicate, http.StatusConflict, "email_in_use")

	wrongPassword := performAuthRequest(t, router, "/api/v1/auth/login", email, "wrong-test-password!")
	unknownUser := performAuthRequest(t, router, "/api/v1/auth/login", "missing@example.test", "wrong-test-password!")
	assertAuthAPIError(t, wrongPassword, http.StatusUnauthorized, "invalid_credentials")
	assertAuthAPIError(t, unknownUser, http.StatusUnauthorized, "invalid_credentials")
	if wrongPassword.ErrorMessage != unknownUser.ErrorMessage {
		t.Fatal("login error message reveals whether the account exists")
	}

	loginAccessDigest := sha256.Sum256([]byte(login.Body.Session.Token))
	_, err = pool.Exec(ctx, `
		UPDATE auth_sessions
		SET created_at = CURRENT_TIMESTAMP - INTERVAL '2 hours',
		    expires_at = CURRENT_TIMESTAMP - INTERVAL '1 hour'
		WHERE token_digest = $1
	`, loginAccessDigest[:])
	if err != nil {
		t.Fatalf("expire signup session: %v", err)
	}
	if _, err := service.Authenticate(ctx, login.Body.Session.Token); err != auth.ErrInvalidSession {
		t.Fatalf("authenticate expired session: %v, want ErrInvalidSession", err)
	}

	for _, secret := range []string{
		password,
		signup.Body.Session.Token,
		signupRefreshToken,
		refreshed.Body.Session.Token,
		rotatedRefreshToken,
		login.Body.Session.Token,
		loginRefreshToken,
	} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("authentication logs contain a password or session token")
		}
	}
}

func TestLogoutRevocationAndSessionCleanupWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL connection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		email    = "logout-integration@example.test"
		password = "P1-202-test-password!"
	)
	deleteAuthTestUser(t, ctx, pool, email)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		deleteAuthTestUser(t, cleanupCtx, pool, email)
	})

	service := auth.NewService(pool, &recordingVerificationSender{})
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{
		Logger:        slog.New(slog.NewJSONHandler(&logs, nil)),
		AuthService:   service,
		SecureCookies: true,
	})

	signup := performAuthRequest(t, router, "/api/v1/auth/signup", email, password)
	if signup.Status != http.StatusCreated {
		t.Fatalf("signup status = %d: %s", signup.Status, signup.RawBody)
	}
	signupRefresh := requireRefreshCookie(t, signup, true)
	login := performAuthRequest(t, router, "/api/v1/auth/login", email, password)
	if login.Status != http.StatusOK {
		t.Fatalf("login status = %d: %s", login.Status, login.RawBody)
	}
	loginRefresh := requireRefreshCookie(t, login, true)
	rotatedLogin := performRefreshRequest(t, router, loginRefresh)
	if rotatedLogin.Status != http.StatusOK {
		t.Fatalf("rotate login refresh status = %d: %s", rotatedLogin.Status, rotatedLogin.RawBody)
	}
	currentLoginRefresh := requireRefreshCookie(t, rotatedLogin, true)

	var signupFamilyID string
	var loginFamilyID string
	if err := pool.QueryRow(ctx, `SELECT family_id::text FROM refresh_tokens WHERE token_digest = $1`,
		tokenSHA256(signupRefresh)).Scan(&signupFamilyID); err != nil {
		t.Fatalf("load signup family: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT family_id::text FROM refresh_tokens WHERE token_digest = $1`,
		tokenSHA256(loginRefresh)).Scan(&loginFamilyID); err != nil {
		t.Fatalf("load login family: %v", err)
	}

	currentLogout := performLogoutRequest(t, router, currentLoginRefresh, false)
	assertLogoutSuccess(t, currentLogout, true)
	if _, err := service.Authenticate(ctx, rotatedLogin.Body.Session.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("current logout left its access token active: %v", err)
	}
	if _, err := service.Authenticate(ctx, signup.Body.Session.Token); err != nil {
		t.Fatalf("current logout revoked another family: %v", err)
	}
	assertAuthAPIError(t, performRefreshRequest(t, router, currentLoginRefresh),
		http.StatusUnauthorized, "invalid_refresh_token")

	allLogout := performLogoutRequest(t, router, signupRefresh, true)
	assertLogoutSuccess(t, allLogout, true)
	if _, err := service.Authenticate(ctx, signup.Body.Session.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("all-session logout left an access token active: %v", err)
	}
	assertAuthAPIError(t, performRefreshRequest(t, router, signupRefresh),
		http.StatusUnauthorized, "invalid_refresh_token")

	if _, err := pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET created_at = CURRENT_TIMESTAMP - INTERVAL '40 days',
		    expires_at = CURRENT_TIMESTAMP - INTERVAL '1 day'
		WHERE family_id = $1::text::uuid
	`, loginFamilyID); err != nil {
		t.Fatalf("expire logged-out refresh family: %v", err)
	}
	deleted, err := service.CleanupSessions(ctx)
	if err != nil {
		t.Fatalf("clean sessions: %v", err)
	}
	if deleted < 5 {
		t.Fatalf("cleanup deleted %d rows, want at least 5", deleted)
	}

	var loginRefreshRows int
	var retainedRevokedRefreshRows int
	var accessRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE family_id = $1::text::uuid`,
		loginFamilyID).Scan(&loginRefreshRows); err != nil {
		t.Fatalf("count expired refresh family: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM refresh_tokens
		WHERE family_id = $1::text::uuid
		  AND revoked_at IS NOT NULL
		  AND expires_at > CURRENT_TIMESTAMP
	`, signupFamilyID).Scan(&retainedRevokedRefreshRows); err != nil {
		t.Fatalf("count retained revoked refresh rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id = $1::text::uuid`,
		signup.Body.User.ID).Scan(&accessRows); err != nil {
		t.Fatalf("count cleaned access rows: %v", err)
	}
	if loginRefreshRows != 0 || retainedRevokedRefreshRows != 1 || accessRows != 0 {
		t.Fatalf("cleanup state expired=%d retained=%d access=%d",
			loginRefreshRows, retainedRevokedRefreshRows, accessRows)
	}

	for _, secret := range []string{
		password,
		signup.Body.Session.Token,
		signupRefresh,
		login.Body.Session.Token,
		loginRefresh,
		rotatedLogin.Body.Session.Token,
		currentLoginRefresh,
	} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("logout logs contain credentials or session tokens")
		}
	}
}

func TestEmailVerificationWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL connection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	emails := []string{"verify-valid@example.test", "verify-expired@example.test", "verify-delivery-failure@example.test"}
	for _, email := range emails {
		deleteAuthTestUser(t, ctx, pool, email)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, email := range emails {
			deleteAuthTestUser(t, cleanupCtx, pool, email)
		}
	})

	delivery := &recordingVerificationSender{}
	service := auth.NewService(pool, delivery)
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{
		Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
		AuthService: service,
	})

	validSignup := performAuthRequest(t, router, "/api/v1/auth/signup", emails[0], "P1-203-valid-password!")
	if validSignup.Status != http.StatusCreated || len(delivery.tokens) != 1 {
		t.Fatalf("verification signup = %d deliveries=%d", validSignup.Status, len(delivery.tokens))
	}
	validToken := delivery.tokens[0]
	if strings.Contains(validSignup.RawBody, validToken) || strings.Contains(logs.String(), validToken) {
		t.Fatal("signup response or logs exposed the verification token")
	}

	var storedDigest []byte
	var storedExpiry time.Time
	var usedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT token_digest, expires_at, used_at
		FROM user_action_tokens
		WHERE user_id = $1::text::uuid AND purpose = 'email_verification'
	`, validSignup.Body.User.ID).Scan(&storedDigest, &storedExpiry, &usedAt); err != nil {
		t.Fatalf("inspect verification token: %v", err)
	}
	if !bytes.Equal(storedDigest, tokenSHA256(validToken)) ||
		bytes.Contains(storedDigest, []byte(validToken)) ||
		storedExpiry.Before(time.Now().Add(23*time.Hour)) || usedAt != nil {
		t.Fatalf("unsafe stored verification token: expiry=%s used=%v", storedExpiry, usedAt)
	}

	verified := performVerificationRequest(t, router, validToken)
	if verified.Status != http.StatusOK || !verified.Body.User.EmailVerified ||
		verified.Body.User.Email != emails[0] {
		t.Fatalf("verification response = %d %s", verified.Status, verified.RawBody)
	}
	if strings.Contains(verified.RawBody, validToken) || strings.Contains(logs.String(), validToken) {
		t.Fatal("verification response or logs exposed the token")
	}
	reused := performVerificationRequest(t, router, validToken)
	assertAuthAPIError(t, reused, http.StatusBadRequest, "invalid_verification_token")

	expiredSignup := performAuthRequest(t, router, "/api/v1/auth/signup", emails[1], "P1-203-expired-password!")
	if expiredSignup.Status != http.StatusCreated || len(delivery.tokens) != 2 {
		t.Fatalf("expired-token signup = %d deliveries=%d", expiredSignup.Status, len(delivery.tokens))
	}
	expiredToken := delivery.tokens[1]
	if _, err := pool.Exec(ctx, `
		UPDATE user_action_tokens
		SET created_at = CURRENT_TIMESTAMP - INTERVAL '2 days',
		    expires_at = CURRENT_TIMESTAMP - INTERVAL '1 day'
		WHERE token_digest = $1
	`, tokenSHA256(expiredToken)); err != nil {
		t.Fatalf("expire verification token: %v", err)
	}
	expired := performVerificationRequest(t, router, expiredToken)
	assertAuthAPIError(t, expired, http.StatusBadRequest, "invalid_verification_token")

	failingService := auth.NewService(pool, &recordingVerificationSender{err: errors.New("local SMTP unavailable")})
	failingRouter := httpapi.NewRouter(httpapi.Options{Logger: discardIntegrationLogger(), AuthService: failingService})
	failedSignup := performAuthRequest(t, failingRouter, "/api/v1/auth/signup", emails[2], "P1-203-failure-password!")
	assertAuthAPIError(t, failedSignup, http.StatusInternalServerError, "internal_error")
	var strandedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1`, emails[2]).Scan(&strandedRows); err != nil {
		t.Fatalf("inspect failed verification delivery: %v", err)
	}
	if strandedRows != 0 {
		t.Fatal("failed verification delivery committed a stranded account")
	}
}

func TestEmailVerificationSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	var relationName *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.user_action_tokens')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back user_action_tokens table: %v", err)
	}
	if relationName != nil {
		t.Fatal("user_action_tokens still exists after email-verification rollback")
	}
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.refresh_tokens')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect preserved refresh_tokens table: %v", err)
	}
	if relationName == nil {
		t.Fatal("preceding refresh_tokens table is absent after email-verification rollback")
	}
}

func TestAuthSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_AUTH_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_AUTH_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	var relationName *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.auth_sessions')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back auth_sessions table: %v", err)
	}
	if relationName != nil {
		t.Fatal("auth_sessions still exists after migration rollback")
	}

	for _, tableName := range []string{"users", "organizations", "org_members", "projects", "environments"} {
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+tableName).Scan(&relationName); err != nil {
			t.Fatalf("inspect preserved table %s: %v", tableName, err)
		}
		if relationName == nil {
			t.Errorf("preceding migration table %s is absent", tableName)
		}
	}
}

type authAPIResult struct {
	Status       int
	RawBody      string
	ErrorCode    string
	ErrorMessage string
	Cookies      []*http.Cookie
	Body         struct {
		User struct {
			ID            string `json:"id"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
		} `json:"user"`
		Session struct {
			Token     string    `json:"token"`
			TokenType string    `json:"token_type"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"session"`
	}
}

type recordingVerificationSender struct {
	recipients []string
	tokens     []string
	err        error
}

func (sender *recordingVerificationSender) SendVerification(_ context.Context, recipient, token string) error {
	sender.recipients = append(sender.recipients, recipient)
	sender.tokens = append(sender.tokens, token)
	return sender.err
}

func performAuthRequest(t *testing.T, handler http.Handler, path, email, password string) authAPIResult {
	t.Helper()

	requestBody, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("encode authentication request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	result := authAPIResult{
		Status:  response.Code,
		RawBody: response.Body.String(),
		Cookies: response.Result().Cookies(),
	}
	if response.Code >= http.StatusBadRequest {
		var errorBody httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil {
			t.Fatalf("decode authentication error: %v", err)
		}
		result.ErrorCode = errorBody.Error.Code
		result.ErrorMessage = errorBody.Error.Message
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatalf("decode authentication response: %v", err)
	}
	return result
}

func TestProductionAuthSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	var relationName *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.refresh_tokens')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect rolled-back refresh_tokens table: %v", err)
	}
	if relationName != nil {
		t.Fatal("refresh_tokens still exists after production-auth rollback")
	}
	var productionColumns int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'auth_sessions'
		  AND column_name IN ('family_id', 'revoked_at')
	`).Scan(&productionColumns); err != nil {
		t.Fatalf("inspect rolled-back auth session columns: %v", err)
	}
	if productionColumns != 0 {
		t.Fatalf("auth_sessions retains %d production columns", productionColumns)
	}
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.auth_sessions')::text`).Scan(&relationName); err != nil {
		t.Fatalf("inspect preserved auth_sessions table: %v", err)
	}
	if relationName == nil {
		t.Fatal("preceding auth_sessions table is absent after production-auth rollback")
	}
}

func performRefreshRequest(t *testing.T, handler http.Handler, refreshToken string) authAPIResult {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	if refreshToken != "" {
		request.AddCookie(&http.Cookie{Name: "watchtrace_refresh", Value: refreshToken})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := authAPIResult{
		Status:  response.Code,
		RawBody: response.Body.String(),
		Cookies: response.Result().Cookies(),
	}
	if response.Code >= http.StatusBadRequest {
		var errorBody httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil {
			t.Fatalf("decode refresh error: %v", err)
		}
		result.ErrorCode = errorBody.Error.Code
		result.ErrorMessage = errorBody.Error.Message
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}
	return result
}

func performVerificationRequest(t *testing.T, handler http.Handler, token string) authAPIResult {
	t.Helper()
	requestBody, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		t.Fatalf("encode email verification request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verify-email", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := authAPIResult{Status: response.Code, RawBody: response.Body.String()}
	if response.Code >= http.StatusBadRequest {
		var errorBody httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil {
			t.Fatalf("decode email verification error: %v", err)
		}
		result.ErrorCode = errorBody.Error.Code
		result.ErrorMessage = errorBody.Error.Message
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatalf("decode email verification response: %v", err)
	}
	return result
}

func discardIntegrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func performLogoutRequest(t *testing.T, handler http.Handler, refreshToken string, allSessions bool) authAPIResult {
	t.Helper()
	requestBody, err := json.Marshal(map[string]bool{"all_sessions": allSessions})
	if err != nil {
		t.Fatalf("encode logout request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	if refreshToken != "" {
		request.AddCookie(&http.Cookie{Name: "watchtrace_refresh", Value: refreshToken})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return authAPIResult{
		Status:  response.Code,
		RawBody: response.Body.String(),
		Cookies: response.Result().Cookies(),
	}
}

func assertLogoutSuccess(t *testing.T, result authAPIResult, secure bool) {
	t.Helper()
	if result.Status != http.StatusNoContent || result.RawBody != "" {
		t.Fatalf("logout response = %d %q", result.Status, result.RawBody)
	}
	if len(result.Cookies) != 1 || result.Cookies[0].MaxAge != -1 ||
		!result.Cookies[0].HttpOnly || result.Cookies[0].Secure != secure {
		t.Fatalf("logout cookie was not cleared safely: %+v", result.Cookies)
	}
}

func tokenSHA256(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func assertAuthAPIError(t *testing.T, result authAPIResult, status int, code string) {
	t.Helper()
	if result.Status != status || result.ErrorCode != code {
		t.Fatalf("authentication error = status %d code %q, want status %d code %q: %s", result.Status, result.ErrorCode, status, code, result.RawBody)
	}
}

func requireRefreshCookie(t *testing.T, result authAPIResult, secure bool) string {
	t.Helper()
	if len(result.Cookies) != 1 {
		t.Fatalf("refresh cookies = %d, want 1", len(result.Cookies))
	}
	cookie := result.Cookies[0]
	if cookie.Name != "watchtrace_refresh" || cookie.Value == "" ||
		cookie.Path != "/api/v1/auth" || !cookie.HttpOnly || cookie.Secure != secure ||
		cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge < 1 ||
		cookie.Expires.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("unsafe refresh cookie: %+v", cookie)
	}
	return cookie.Value
}

func deleteAuthTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		DELETE FROM user_action_tokens
		WHERE user_id IN (SELECT id FROM users WHERE email = $1)
	`, email); err != nil {
		t.Fatalf("delete test user action tokens: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM refresh_tokens
		WHERE user_id IN (SELECT id FROM users WHERE email = $1)
	`, email); err != nil {
		t.Fatalf("delete test refresh tokens: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM auth_sessions
		WHERE user_id IN (SELECT id FROM users WHERE email = $1)
	`, email); err != nil {
		t.Fatalf("delete test sessions: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE email = $1`, email); err != nil {
		t.Fatalf("delete test user: %v", err)
	}
}
