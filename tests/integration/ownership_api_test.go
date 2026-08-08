package integration_test

import (
	"bytes"
	"context"
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
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

func TestDefaultOwnershipAPIWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL connection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	emails := []string{"ownership-first@example.test", "ownership-second@example.test"}
	slugs := []string{"ownership-first", "ownership-second", "ownership-rollback"}
	deleteOwnershipTestData(t, ctx, pool, emails, slugs)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		deleteOwnershipTestData(t, cleanupCtx, pool, emails, slugs)
	})

	authService := auth.NewService(pool, &recordingVerificationSender{})
	ownershipService := ownership.NewService(pool)
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		AuthService:      authService,
		Authenticator:    authService,
		OwnershipService: ownershipService,
	})

	firstSignup := performAuthRequest(t, router, "/api/v1/auth/signup", emails[0], "P1-103-first-password!")
	if firstSignup.Status != http.StatusCreated {
		t.Fatalf("first signup status = %d: %s", firstSignup.Status, firstSignup.RawBody)
	}
	first := performOwnershipRequest(t, router, firstSignup.Body.Session.Token, ownershipRequestBody{
		OrganizationName:   " First Organization ",
		OrganizationSlug:   "Ownership-First",
		ProjectName:        " Main API ",
		ProjectDescription: " Production monitoring ",
	})
	if first.Status != http.StatusCreated {
		t.Fatalf("ownership status = %d, want %d: %s", first.Status, http.StatusCreated, first.RawBody)
	}
	assertDefaultOwnershipResponse(t, first, firstSignup.Body.User.ID, "ownership-first")
	assertStoredOwnershipHierarchy(t, ctx, pool, first, firstSignup.Body.User.ID)

	missingSession := performOwnershipRequest(t, router, "", ownershipRequestBody{
		OrganizationName: "Unauthorized",
		OrganizationSlug: "unauthorized",
		ProjectName:      "API",
	})
	if missingSession.Status != http.StatusUnauthorized || missingSession.ErrorCode != "invalid_session" {
		t.Fatalf("missing-session response = %d %q", missingSession.Status, missingSession.ErrorCode)
	}

	secondSignup := performAuthRequest(t, router, "/api/v1/auth/signup", emails[1], "P1-103-second-password!")
	if secondSignup.Status != http.StatusCreated {
		t.Fatalf("second signup status = %d: %s", secondSignup.Status, secondSignup.RawBody)
	}
	duplicate := performOwnershipRequest(t, router, secondSignup.Body.Session.Token, ownershipRequestBody{
		OrganizationName: "Duplicate",
		OrganizationSlug: "OWNERSHIP-FIRST",
		ProjectName:      "API",
	})
	if duplicate.Status != http.StatusConflict || duplicate.ErrorCode != "organization_slug_in_use" {
		t.Fatalf("duplicate-slug response = %d %q: %s", duplicate.Status, duplicate.ErrorCode, duplicate.RawBody)
	}

	second := performOwnershipRequest(t, router, secondSignup.Body.Session.Token, ownershipRequestBody{
		OrganizationName: "Second Organization",
		OrganizationSlug: "ownership-second",
		ProjectName:      "Second API",
	})
	if second.Status != http.StatusCreated {
		t.Fatalf("second ownership status = %d: %s", second.Status, second.RawBody)
	}
	assertDefaultOwnershipResponse(t, second, secondSignup.Body.User.ID, "ownership-second")
	if second.Body.Membership.UserID == first.Body.Membership.UserID {
		t.Fatal("two authenticated users received the same owner membership")
	}

	installEnvironmentFailureTrigger(t, ctx, pool)
	failed := performOwnershipRequest(t, router, firstSignup.Body.Session.Token, ownershipRequestBody{
		OrganizationName: "Rollback Organization",
		OrganizationSlug: "ownership-rollback",
		ProjectName:      "Rollback API",
	})
	if failed.Status != http.StatusInternalServerError || failed.ErrorCode != "internal_error" {
		t.Fatalf("forced-failure response = %d %q: %s", failed.Status, failed.ErrorCode, failed.RawBody)
	}
	var rolledBackOrganizations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM organizations
		WHERE slug = 'ownership-rollback'
	`).Scan(&rolledBackOrganizations); err != nil {
		t.Fatalf("count rolled-back organizations: %v", err)
	}
	if rolledBackOrganizations != 0 {
		t.Fatal("ownership transaction left an organization after the environment insert failed")
	}

	for _, secret := range []string{firstSignup.Body.Session.Token, secondSignup.Body.Session.Token} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("ownership logs contain a bearer token")
		}
	}
}

type ownershipRequestBody struct {
	OrganizationName   string
	OrganizationSlug   string
	ProjectName        string
	ProjectDescription string
}

type ownershipAPIResult struct {
	Status    int
	RawBody   string
	ErrorCode string
	Body      struct {
		Organization struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"organization"`
		Membership struct {
			OrganizationID string `json:"organization_id"`
			UserID         string `json:"user_id"`
			Role           string `json:"role"`
		} `json:"membership"`
		Project struct {
			ID             string `json:"id"`
			OrganizationID string `json:"organization_id"`
			Name           string `json:"name"`
			Description    string `json:"description"`
		} `json:"project"`
		Environment struct {
			ID             string `json:"id"`
			OrganizationID string `json:"organization_id"`
			ProjectID      string `json:"project_id"`
			Name           string `json:"name"`
			Type           string `json:"type"`
		} `json:"environment"`
	}
}

func performOwnershipRequest(
	t *testing.T,
	handler http.Handler,
	token string,
	input ownershipRequestBody,
) ownershipAPIResult {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"organization": map[string]string{
			"name": input.OrganizationName,
			"slug": input.OrganizationSlug,
		},
		"project": map[string]string{
			"name":        input.ProjectName,
			"description": input.ProjectDescription,
		},
	})
	if err != nil {
		t.Fatalf("encode ownership request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	result := ownershipAPIResult{Status: response.Code, RawBody: response.Body.String()}
	if response.Code >= http.StatusBadRequest {
		var errorBody httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil {
			t.Fatalf("decode ownership error: %v", err)
		}
		result.ErrorCode = errorBody.Error.Code
		return result
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result.Body); err != nil {
		t.Fatalf("decode ownership response: %v", err)
	}
	return result
}

func assertDefaultOwnershipResponse(
	t *testing.T,
	result ownershipAPIResult,
	userID string,
	slug string,
) {
	t.Helper()
	organizationID := result.Body.Organization.ID
	if organizationID == "" || result.Body.Organization.Slug != slug {
		t.Fatalf("unexpected organization: %+v", result.Body.Organization)
	}
	if result.Body.Membership.OrganizationID != organizationID ||
		result.Body.Membership.UserID != userID ||
		result.Body.Membership.Role != "owner" {
		t.Fatalf("unexpected owner membership: %+v", result.Body.Membership)
	}
	if result.Body.Project.OrganizationID != organizationID {
		t.Fatalf("project belongs to organization %q, want %q", result.Body.Project.OrganizationID, organizationID)
	}
	if result.Body.Environment.OrganizationID != organizationID ||
		result.Body.Environment.ProjectID != result.Body.Project.ID ||
		result.Body.Environment.Name != "Production" ||
		result.Body.Environment.Type != "production" {
		t.Fatalf("unexpected production environment: %+v", result.Body.Environment)
	}
}

func assertStoredOwnershipHierarchy(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	result ownershipAPIResult,
	userID string,
) {
	t.Helper()

	var memberCount int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM organizations
		JOIN org_members ON org_members.organization_id = organizations.id
		JOIN projects ON projects.organization_id = organizations.id
		JOIN environments
		  ON environments.organization_id = organizations.id
		 AND environments.project_id = projects.id
		WHERE organizations.id = $1::text::uuid
		  AND org_members.user_id = $2::text::uuid
		  AND org_members.role = 'owner'
		  AND projects.id = $3::text::uuid
		  AND environments.id = $4::text::uuid
		  AND environments.environment_type = 'production'
	`, result.Body.Organization.ID, userID, result.Body.Project.ID, result.Body.Environment.ID).Scan(&memberCount)
	if err != nil {
		t.Fatalf("inspect stored ownership hierarchy: %v", err)
	}
	if memberCount != 1 {
		t.Fatalf("stored ownership hierarchy count = %d, want 1", memberCount)
	}

	var ownerCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM org_members
		WHERE organization_id = $1::text::uuid AND role = 'owner'
	`, result.Body.Organization.ID).Scan(&ownerCount); err != nil {
		t.Fatalf("count organization owners: %v", err)
	}
	if ownerCount != 1 {
		t.Fatalf("organization owner count = %d, want exactly 1", ownerCount)
	}
}

func installEnvironmentFailureTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	_, err := pool.Exec(ctx, `
		CREATE FUNCTION watchtrace_test_reject_environment() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			RAISE EXCEPTION 'forced ownership transaction failure';
		END;
		$function$;

		CREATE TRIGGER watchtrace_test_reject_environment
		BEFORE INSERT ON environments
		FOR EACH ROW EXECUTE FUNCTION watchtrace_test_reject_environment();
	`)
	if err != nil {
		t.Fatalf("install environment failure trigger: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `
			DROP TRIGGER IF EXISTS watchtrace_test_reject_environment ON environments;
			DROP FUNCTION IF EXISTS watchtrace_test_reject_environment();
		`); err != nil {
			t.Errorf("remove environment failure trigger: %v", err)
		}
	})
}

func deleteOwnershipTestData(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	emails []string,
	slugs []string,
) {
	t.Helper()

	statements := []string{
		`DELETE FROM health_checks WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM check_jobs WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM monitors WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM environments WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM projects WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM org_members WHERE organization_id IN (SELECT id FROM organizations WHERE slug = ANY($1::text[]))`,
		`DELETE FROM organizations WHERE slug = ANY($1::text[])`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, slugs); err != nil {
			t.Fatalf("delete ownership test data: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM user_action_tokens
		WHERE user_id IN (SELECT id FROM users WHERE email = ANY($1::text[]))
	`, emails); err != nil {
		t.Fatalf("delete ownership test user action tokens: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM refresh_tokens
		WHERE user_id IN (SELECT id FROM users WHERE email = ANY($1::text[]))
	`, emails); err != nil {
		t.Fatalf("delete ownership test refresh tokens: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM auth_sessions
		WHERE user_id IN (SELECT id FROM users WHERE email = ANY($1::text[]))
	`, emails); err != nil {
		t.Fatalf("delete ownership test sessions: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE email = ANY($1::text[])`, emails); err != nil {
		t.Fatalf("delete ownership test users: %v", err)
	}
}
