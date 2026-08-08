package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

	service := auth.NewService(pool)
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{
		Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
		AuthService: service,
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

	var passwordHash string
	var tokenDigest []byte
	var storedExpiry time.Time
	err = pool.QueryRow(ctx, `
		SELECT users.password_hash, auth_sessions.token_digest, auth_sessions.expires_at
		FROM users
		JOIN auth_sessions ON auth_sessions.user_id = users.id
		WHERE users.email = $1
	`, email).Scan(&passwordHash, &tokenDigest, &storedExpiry)
	if err != nil {
		t.Fatalf("inspect stored credentials: %v", err)
	}
	if passwordHash == password || !strings.HasPrefix(passwordHash, "$argon2id$") {
		t.Fatal("database does not contain an Argon2id password hash")
	}
	expectedDigest := sha256.Sum256([]byte(signup.Body.Session.Token))
	if !bytes.Equal(tokenDigest, expectedDigest[:]) {
		t.Fatal("database session digest does not match the returned token")
	}
	if bytes.Contains(tokenDigest, []byte(signup.Body.Session.Token)) {
		t.Fatal("database contains the raw session token")
	}
	if !storedExpiry.Equal(signup.Body.Session.ExpiresAt) {
		t.Fatalf("stored expiry %s differs from API expiry %s", storedExpiry, signup.Body.Session.ExpiresAt)
	}

	authenticatedUser, err := service.Authenticate(ctx, signup.Body.Session.Token)
	if err != nil {
		t.Fatalf("authenticate signup session: %v", err)
	}
	if authenticatedUser.ID != signup.Body.User.ID {
		t.Fatalf("authenticated user ID = %q, want %q", authenticatedUser.ID, signup.Body.User.ID)
	}

	login := performAuthRequest(t, router, "/api/v1/auth/login", strings.ToUpper(email), password)
	if login.Status != http.StatusOK {
		t.Fatalf("login status = %d, want %d: %s", login.Status, http.StatusOK, login.RawBody)
	}
	if login.Body.Session.Token == signup.Body.Session.Token {
		t.Fatal("login reused the signup session token")
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

	_, err = pool.Exec(ctx, `
		UPDATE auth_sessions
		SET created_at = CURRENT_TIMESTAMP - INTERVAL '2 hours',
		    expires_at = CURRENT_TIMESTAMP - INTERVAL '1 hour'
		WHERE token_digest = $1
	`, expectedDigest[:])
	if err != nil {
		t.Fatalf("expire signup session: %v", err)
	}
	if _, err := service.Authenticate(ctx, signup.Body.Session.Token); err != auth.ErrInvalidSession {
		t.Fatalf("authenticate expired session: %v, want ErrInvalidSession", err)
	}

	for _, secret := range []string{password, signup.Body.Session.Token, login.Body.Session.Token} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("authentication logs contain a password or session token")
		}
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

	result := authAPIResult{Status: response.Code, RawBody: response.Body.String()}
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

func assertAuthAPIError(t *testing.T, result authAPIResult, status int, code string) {
	t.Helper()
	if result.Status != status || result.ErrorCode != code {
		t.Fatalf("authentication error = status %d code %q, want status %d code %q: %s", result.Status, result.ErrorCode, status, code, result.RawBody)
	}
}

func deleteAuthTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) {
	t.Helper()
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
