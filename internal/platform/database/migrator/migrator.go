// Package migrator creates migration runners for the WatchTrace database.
package migrator

import (
	"database/sql"
	"errors"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/watchtrace/watchtrace-platform/db/migrations"
)

const statementTimeout = 30 * time.Second

// Open creates a migration runner backed by the embedded migration files.
// Connection errors are intentionally generalized so a database URL containing
// credentials cannot be copied into command logs.
func Open(databaseURL string) (*migrate.Migrate, error) {
	sourceDriver, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return nil, errors.New("load embedded migrations")
	}

	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		_ = sourceDriver.Close()
		return nil, errors.New("open database connection")
	}

	databaseDriver, err := migratepgx.WithInstance(database, &migratepgx.Config{
		StatementTimeout: statementTimeout,
	})
	if err != nil {
		_ = database.Close()
		_ = sourceDriver.Close()
		return nil, errors.New("connect migration driver to database")
	}

	runner, err := migrate.NewWithInstance("embedded", sourceDriver, "pgx5", databaseDriver)
	if err != nil {
		_ = databaseDriver.Close()
		_ = sourceDriver.Close()
		return nil, errors.New("initialize migration runner")
	}

	return runner, nil
}
