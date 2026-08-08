package integration_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestOwnershipSchemaCreatesTenantHierarchy(t *testing.T) {
	ctx, tx := beginOwnershipSchemaTest(t)

	var userID string
	var createdAt time.Time
	var updatedAt time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('owner@example.test', 'test-only-password-hash')
		RETURNING id::text, created_at, updated_at
	`).Scan(&userID, &createdAt, &updatedAt)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	assertGeneratedUUID(t, userID)
	assertRecentTimestamp(t, createdAt)
	assertRecentTimestamp(t, updatedAt)

	var organizationID string
	err = tx.QueryRow(ctx, `
		INSERT INTO organizations (name, slug)
		VALUES ('Example Organization', 'example')
		RETURNING id::text
	`).Scan(&organizationID)
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	assertGeneratedUUID(t, organizationID)

	_, err = tx.Exec(ctx, `
		INSERT INTO org_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, organizationID, userID)
	if err != nil {
		t.Fatalf("insert organization owner: %v", err)
	}

	var projectID string
	err = tx.QueryRow(ctx, `
		INSERT INTO projects (organization_id, name, description)
		VALUES ($1, 'WatchTrace', 'Tenant-owned project')
		RETURNING id::text
	`, organizationID).Scan(&projectID)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	assertGeneratedUUID(t, projectID)

	var environmentID string
	err = tx.QueryRow(ctx, `
		INSERT INTO environments (
			organization_id,
			project_id,
			name,
			environment_type
		)
		VALUES ($1, $2, 'Production', 'production')
		RETURNING id::text
	`, organizationID, projectID).Scan(&environmentID)
	if err != nil {
		t.Fatalf("insert environment: %v", err)
	}
	assertGeneratedUUID(t, environmentID)

	if userID == organizationID || userID == projectID || userID == environmentID {
		t.Fatal("database generated duplicate public identifiers")
	}
}

func TestOwnershipSchemaUsesTimeZoneAwareTimestamps(t *testing.T) {
	ctx, tx := beginOwnershipSchemaTest(t)

	expectedColumns := map[string][]string{
		"users":         {"email_verified_at", "created_at", "updated_at"},
		"organizations": {"deleted_at", "created_at", "updated_at"},
		"org_members":   {"created_at", "updated_at"},
		"projects":      {"created_at", "updated_at"},
		"environments":  {"created_at", "updated_at"},
	}

	for tableName, columns := range expectedColumns {
		for _, columnName := range columns {
			var dataType string
			err := tx.QueryRow(ctx, `
				SELECT data_type
				FROM information_schema.columns
				WHERE table_schema = 'public'
				  AND table_name = $1
				  AND column_name = $2
			`, tableName, columnName).Scan(&dataType)
			if err != nil {
				t.Fatalf("inspect %s.%s: %v", tableName, columnName, err)
			}
			if dataType != "timestamp with time zone" {
				t.Errorf("%s.%s has type %q, want timestamp with time zone", tableName, columnName, dataType)
			}
		}
	}
}

func TestOwnershipSchemaRejectsCrossTenantEnvironment(t *testing.T) {
	ctx, tx := beginOwnershipSchemaTest(t)

	firstOrganizationID := insertTestOrganization(t, ctx, tx, "First", "first")
	secondOrganizationID := insertTestOrganization(t, ctx, tx, "Second", "second")
	projectID := insertTestProject(t, ctx, tx, firstOrganizationID)

	_, err := tx.Exec(ctx, `
		INSERT INTO environments (
			organization_id,
			project_id,
			name,
			environment_type
		)
		VALUES ($1, $2, 'Production', 'production')
	`, secondOrganizationID, projectID)
	assertPostgreSQLErrorCode(t, err, "23503")
}

func TestOwnershipSchemaRejectsDuplicateCaseInsensitiveIdentity(t *testing.T) {
	ctx, tx := beginOwnershipSchemaTest(t)

	_, err := tx.Exec(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('Owner@Example.Test', 'first-test-hash')
	`)
	if err != nil {
		t.Fatalf("insert first user: %v", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES (' owner@example.test ', 'second-test-hash')
	`)
	assertPostgreSQLErrorCode(t, err, "23505")
}

func TestOwnershipSchemaRestrictsRolesAndOwners(t *testing.T) {
	t.Run("role", func(t *testing.T) {
		ctx, tx := beginOwnershipSchemaTest(t)
		userID := insertTestUser(t, ctx, tx, "member@example.test")
		organizationID := insertTestOrganization(t, ctx, tx, "Example", "example")

		_, err := tx.Exec(ctx, `
			INSERT INTO org_members (organization_id, user_id, role)
			VALUES ($1, $2, 'superuser')
		`, organizationID, userID)
		assertPostgreSQLErrorCode(t, err, "23514")
	})

	t.Run("one owner", func(t *testing.T) {
		ctx, tx := beginOwnershipSchemaTest(t)
		firstUserID := insertTestUser(t, ctx, tx, "first@example.test")
		secondUserID := insertTestUser(t, ctx, tx, "second@example.test")
		organizationID := insertTestOrganization(t, ctx, tx, "Example", "example")

		_, err := tx.Exec(ctx, `
			INSERT INTO org_members (organization_id, user_id, role)
			VALUES ($1, $2, 'owner')
		`, organizationID, firstUserID)
		if err != nil {
			t.Fatalf("insert first owner: %v", err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO org_members (organization_id, user_id, role)
			VALUES ($1, $2, 'owner')
		`, organizationID, secondUserID)
		assertPostgreSQLErrorCode(t, err, "23505")
	})
}

func TestOwnershipSchemaRollback(t *testing.T) {
	if os.Getenv("WATCHTRACE_EXPECT_OWNERSHIP_SCHEMA_ABSENT") != "1" {
		t.Skip("WATCHTRACE_EXPECT_OWNERSHIP_SCHEMA_ABSENT is not set")
	}

	ctx, tx := beginOwnershipSchemaTest(t)
	for _, tableName := range []string{
		"users",
		"organizations",
		"org_members",
		"projects",
		"environments",
	} {
		var relationName *string
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+tableName).Scan(&relationName); err != nil {
			t.Fatalf("inspect rolled-back table %s: %v", tableName, err)
		}
		if relationName != nil {
			t.Errorf("table %s still exists after migration rollback", tableName)
		}
	}
}

func beginOwnershipSchemaTest(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()

	databaseURL := os.Getenv("WATCHTRACE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("WATCHTRACE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL connection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin PostgreSQL transaction: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
	})

	return ctx, tx
}

func insertTestUser(t *testing.T, ctx context.Context, tx pgx.Tx, email string) string {
	t.Helper()

	var userID string
	err := tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, 'test-only-password-hash')
		RETURNING id::text
	`, email).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return userID
}

func insertTestOrganization(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	name string,
	slug string,
) string {
	t.Helper()

	var organizationID string
	err := tx.QueryRow(ctx, `
		INSERT INTO organizations (name, slug)
		VALUES ($1, $2)
		RETURNING id::text
	`, name, slug).Scan(&organizationID)
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	return organizationID
}

func insertTestProject(t *testing.T, ctx context.Context, tx pgx.Tx, organizationID string) string {
	t.Helper()

	var projectID string
	err := tx.QueryRow(ctx, `
		INSERT INTO projects (organization_id, name)
		VALUES ($1, 'Test project')
		RETURNING id::text
	`, organizationID).Scan(&projectID)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

func assertPostgreSQLErrorCode(t *testing.T, err error, code string) {
	t.Helper()

	if err == nil {
		t.Fatalf("operation succeeded, want PostgreSQL error %s", code)
	}
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		t.Fatalf("error type %T, want *pgconn.PgError: %v", err, err)
	}
	if postgresError.Code != code {
		t.Fatalf("PostgreSQL error code %s, want %s: %v", postgresError.Code, code, err)
	}
}

func assertGeneratedUUID(t *testing.T, identifier string) {
	t.Helper()

	if !uuidPattern.MatchString(identifier) {
		t.Fatalf("identifier %q is not a database-generated UUIDv4", identifier)
	}
}

func assertRecentTimestamp(t *testing.T, value time.Time) {
	t.Helper()

	now := time.Now()
	if value.Before(now.Add(-time.Minute)) || value.After(now.Add(time.Minute)) {
		t.Fatalf("timestamp %s is not close to application time %s", value, now)
	}
}
