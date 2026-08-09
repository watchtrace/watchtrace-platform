package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
)

func TestMembershipAuthorizationAndTenantSecurityWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	emails := []string{"tenant-owner@example.test", "tenant-admin@example.test", "tenant-member@example.test", "tenant-viewer@example.test", "tenant-outsider@example.test", "tenant-unverified@example.test", "tenant-expired@example.test"}
	slugs := []string{"tenant-security-main", "tenant-security-foreign"}
	deleteOwnershipTestData(t, ctx, pool, emails, slugs)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		deleteOwnershipTestData(t, cleanupCtx, pool, emails, slugs)
	})

	delivery := &recordingVerificationSender{}
	authService := auth.NewService(pool, delivery)
	ownershipService := ownership.NewService(pool, delivery)
	monitorService := monitor.NewService(pool)
	var logs bytes.Buffer
	router := httpapi.NewRouter(httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), AuthService: authService, Authenticator: authService, OwnershipService: ownershipService, MonitorService: monitorService, SecureCookies: true})

	tokens := map[string]string{}
	for index, email := range emails {
		signup := performAuthRequest(t, router, "/api/v1/auth/signup", email, "P1-205-secure-password!")
		if signup.Status != http.StatusCreated {
			t.Fatalf("signup %s: %d %s", email, signup.Status, signup.RawBody)
		}
		tokens[email] = signup.Body.Session.Token
		if email != emails[5] {
			verified := performVerificationRequest(t, router, delivery.tokens[index])
			if verified.Status != http.StatusOK {
				t.Fatalf("verify %s: %d %s", email, verified.Status, verified.RawBody)
			}
		}
	}

	mainOrg := performOwnershipRequest(t, router, tokens[emails[0]], ownershipRequestBody{OrganizationName: "Tenant Security", OrganizationSlug: slugs[0], ProjectName: "API"})
	foreignOrg := performOwnershipRequest(t, router, tokens[emails[4]], ownershipRequestBody{OrganizationName: "Foreign Tenant", OrganizationSlug: slugs[1], ProjectName: "API"})
	if mainOrg.Status != http.StatusCreated || foreignOrg.Status != http.StatusCreated {
		t.Fatalf("create tenant roots: %d/%d", mainOrg.Status, foreignOrg.Status)
	}

	roles := []string{"admin", "member", "viewer"}
	for index, role := range roles {
		invite := performInvitationRequest(t, router, tokens[emails[0]], mainOrg.Body.Organization.ID, emails[index+1], role)
		if invite.Status != http.StatusCreated || invite.TokenLeaked {
			t.Fatalf("invite %s: %+v", role, invite)
		}
		rawToken := delivery.inviteTokens[len(delivery.inviteTokens)-1]
		accepted := performInvitationAcceptance(t, router, tokens[emails[index+1]], rawToken)
		if accepted.Status != http.StatusCreated || accepted.Role != role {
			t.Fatalf("accept %s: %+v", role, accepted)
		}
		if strings.Contains(logs.String(), rawToken) {
			t.Fatal("invitation token appeared in logs")
		}
	}

	duplicate := performInvitationRequest(t, router, tokens[emails[0]], mainOrg.Body.Organization.ID, emails[1], "admin")
	if duplicate.Status != http.StatusConflict || duplicate.ErrorCode != "already_member" {
		t.Fatalf("duplicate member = %+v", duplicate)
	}

	for index, role := range []string{"owner", "admin", "member", "viewer"} {
		memberToken := tokens[emails[index]]
		list := performMemberList(t, router, memberToken, mainOrg.Body.Organization.ID)
		if list.Status != http.StatusOK || list.Count != 4 {
			t.Fatalf("%s member list = %+v", role, list)
		}
		invite := performInvitationRequest(t, router, memberToken, mainOrg.Body.Organization.ID, fmt.Sprintf("permission-%s@example.test", role), "viewer")
		want := http.StatusForbidden
		if role == "owner" || role == "admin" {
			want = http.StatusCreated
		}
		if invite.Status != want {
			t.Fatalf("%s invite status = %d, want %d", role, invite.Status, want)
		}
	}

	var firstMonitorID string
	for index, role := range []string{"owner", "admin", "member", "viewer"} {
		created := performMonitorCreate(t, router, tokens[emails[index]], mainOrg.Body.Environment.ID, monitorCreateBody{Name: "Role " + role, URL: "https://example.com/health"})
		want := http.StatusCreated
		if role == "viewer" {
			want = http.StatusForbidden
		}
		if created.Status != want {
			t.Fatalf("%s monitor create = %d/%s", role, created.Status, created.ErrorCode)
		}
		if firstMonitorID == "" && created.Status == http.StatusCreated {
			firstMonitorID = created.Body.ID
		}
		if list := performMonitorList(t, router, tokens[emails[index]], mainOrg.Body.Environment.ID); list.Status != http.StatusOK {
			t.Fatalf("%s monitor list = %d", role, list.Status)
		}
		if detail := performMonitorGet(t, router, tokens[emails[index]], mainOrg.Body.Environment.ID, firstMonitorID); detail.Status != http.StatusOK {
			t.Fatalf("%s monitor get = %d", role, detail.Status)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE org_members SET role = 'viewer', updated_at = CURRENT_TIMESTAMP WHERE organization_id = $1::text::uuid AND user_id = (SELECT id FROM users WHERE email = $2)`, mainOrg.Body.Organization.ID, emails[2]); err != nil {
		t.Fatalf("change current role: %v", err)
	}
	changedRole := performMonitorCreate(t, router, tokens[emails[2]], mainOrg.Body.Environment.ID, monitorCreateBody{Name: "Stale role", URL: "https://example.com/health"})
	if changedRole.Status != http.StatusForbidden {
		t.Fatalf("session retained stale member role: %d", changedRole.Status)
	}

	for _, foreignToken := range []string{tokens[emails[0]], tokens[emails[1]], tokens[emails[2]], tokens[emails[3]]} {
		if result := performMonitorList(t, router, foreignToken, foreignOrg.Body.Environment.ID); result.Status != http.StatusNotFound {
			t.Fatalf("cross-tenant list = %d", result.Status)
		}
		if result := performMonitorCreate(t, router, foreignToken, foreignOrg.Body.Environment.ID, monitorCreateBody{Name: "Cross tenant", URL: "https://example.com"}); result.Status != http.StatusNotFound {
			t.Fatalf("cross-tenant create = %d", result.Status)
		}
		if result := performMonitorGet(t, router, foreignToken, foreignOrg.Body.Environment.ID, firstMonitorID); result.Status != http.StatusNotFound {
			t.Fatalf("cross-tenant get = %d", result.Status)
		}
	}
	foreignMonitor := performMonitorCreate(t, router, tokens[emails[4]], foreignOrg.Body.Environment.ID, monitorCreateBody{Name: "Foreign monitor", URL: "https://example.com/health"})
	if foreignMonitor.Status != http.StatusCreated {
		t.Fatalf("foreign monitor setup = %d", foreignMonitor.Status)
	}
	var mainJobID string
	if err := pool.QueryRow(ctx, `INSERT INTO check_jobs (organization_id, environment_id, monitor_id, scheduled_at) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, CURRENT_TIMESTAMP + INTERVAL '1 minute') RETURNING id::text`, mainOrg.Body.Organization.ID, mainOrg.Body.Environment.ID, firstMonitorID).Scan(&mainJobID); err != nil {
		t.Fatalf("create tenant constraint job: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO health_checks (job_id, organization_id, environment_id, monitor_id, job_type, scheduled_at, started_at, completed_at, succeeded, status_code, total_duration_microseconds) VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4::text::uuid, 'scheduled', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, true, 200, 1)`, mainJobID, foreignOrg.Body.Organization.ID, foreignOrg.Body.Environment.ID, foreignMonitor.Body.ID); err == nil || !strings.Contains(err.Error(), "health_checks_tenant_job_fkey") {
		t.Fatalf("cross-tenant job/result link was not rejected by the composite constraint: %v", err)
	}

	unverifiedInvite := performInvitationRequest(t, router, tokens[emails[0]], mainOrg.Body.Organization.ID, emails[5], "viewer")
	if unverifiedInvite.Status != http.StatusCreated {
		t.Fatalf("unverified pending invite = %d", unverifiedInvite.Status)
	}
	unverifiedToken := delivery.inviteTokens[len(delivery.inviteTokens)-1]
	if result := performInvitationAcceptance(t, router, tokens[emails[5]], unverifiedToken); result.Status != http.StatusForbidden || result.ErrorCode != "email_not_verified" {
		t.Fatalf("unverified acceptance = %+v", result)
	}
	if verified := performVerificationRequest(t, router, delivery.tokens[5]); verified.Status != http.StatusOK {
		t.Fatalf("verify pending invitee = %d", verified.Status)
	}
	replacement := performInvitationRequest(t, router, tokens[emails[0]], mainOrg.Body.Organization.ID, emails[5], "member")
	if replacement.Status != http.StatusCreated {
		t.Fatalf("replace pending invitation = %d", replacement.Status)
	}
	replacementToken := delivery.inviteTokens[len(delivery.inviteTokens)-1]
	if result := performInvitationAcceptance(t, router, tokens[emails[5]], unverifiedToken); result.Status != http.StatusBadRequest || result.ErrorCode != "invalid_invitation" {
		t.Fatalf("replaced invitation remained valid: %+v", result)
	}
	if result := performInvitationAcceptance(t, router, tokens[emails[5]], replacementToken); result.Status != http.StatusCreated || result.Role != "member" {
		t.Fatalf("replacement invitation acceptance = %+v", result)
	}

	expiredInvite := performInvitationRequest(t, router, tokens[emails[0]], mainOrg.Body.Organization.ID, emails[6], "viewer")
	if expiredInvite.Status != http.StatusCreated {
		t.Fatalf("expired invite setup = %d", expiredInvite.Status)
	}
	expiredToken := delivery.inviteTokens[len(delivery.inviteTokens)-1]
	digest := sha256.Sum256([]byte(expiredToken))
	if _, err := pool.Exec(ctx, `UPDATE org_invitations SET created_at = CURRENT_TIMESTAMP - INTERVAL '8 days', expires_at = CURRENT_TIMESTAMP - INTERVAL '1 day' WHERE token_digest = $1`, digest[:]); err != nil {
		t.Fatalf("expire invitation: %v", err)
	}
	if result := performInvitationAcceptance(t, router, tokens[emails[6]], expiredToken); result.Status != http.StatusBadRequest || result.ErrorCode != "invalid_invitation" {
		t.Fatalf("expired acceptance = %+v", result)
	}

	var storedDigest []byte
	if err := pool.QueryRow(ctx, `SELECT token_digest FROM org_invitations WHERE email = $1 ORDER BY created_at DESC LIMIT 1`, emails[6]).Scan(&storedDigest); err != nil {
		t.Fatalf("read invitation digest: %v", err)
	}
	if bytes.Contains(storedDigest, []byte(expiredToken)) || strings.Contains(expiredInvite.RawBody, expiredToken) {
		t.Fatal("raw invitation token was persisted or returned")
	}
}

func TestMembershipTenantSecuritySchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT is not set")
	}
	ctx, tx := beginOwnershipSchemaTest(t)
	var relation *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.org_invitations')::text`).Scan(&relation); err != nil {
		t.Fatalf("inspect invitations: %v", err)
	}
	if relation != nil {
		t.Fatal("org_invitations remains after rollback")
	}
	var constraints int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname IN ('check_jobs_tenant_job_id_key', 'health_checks_tenant_job_fkey')`).Scan(&constraints); err != nil {
		t.Fatalf("inspect tenant constraints: %v", err)
	}
	if constraints != 0 {
		t.Fatalf("%d Phase 1.1 tenant constraints remain after rollback", constraints)
	}
}

type invitationAPIResult struct {
	Status      int
	RawBody     string
	ErrorCode   string
	Role        string
	Count       int
	TokenLeaked bool
}

func performInvitationRequest(t *testing.T, handler http.Handler, token, organizationID, email, role string) invitationAPIResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "role": role})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/organizations/"+organizationID+"/invitations", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	return executeMembershipRequest(t, handler, request)
}

func performInvitationAcceptance(t *testing.T, handler http.Handler, token, invitationToken string) invitationAPIResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": invitationToken})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/accept-invitation", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	result := executeMembershipRequest(t, handler, request)
	result.TokenLeaked = strings.Contains(result.RawBody, invitationToken)
	return result
}

func performMemberList(t *testing.T, handler http.Handler, token, organizationID string) invitationAPIResult {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/organizations/"+organizationID+"/members", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return executeMembershipRequest(t, handler, request)
}

func executeMembershipRequest(t *testing.T, handler http.Handler, request *http.Request) invitationAPIResult {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := invitationAPIResult{Status: response.Code, RawBody: response.Body.String()}
	if response.Code >= 400 {
		var body httpapi.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode membership error: %v", err)
		}
		result.ErrorCode = body.Error.Code
		return result
	}
	var body struct {
		Role    string            `json:"role"`
		Members []json.RawMessage `json:"members"`
	}
	if response.Body.Len() > 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode membership response: %v", err)
		}
	}
	result.Role = body.Role
	result.Count = len(body.Members)
	return result
}
