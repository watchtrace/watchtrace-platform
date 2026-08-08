package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/watchtrace/watchtrace-platform/internal/platform/config"
	"github.com/watchtrace/watchtrace-platform/internal/platform/database/migrator"
)

const usage = "usage: go run ./cmd/migrate <up|down|version>"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) != 1 {
		return errors.New(usage)
	}

	action := arguments[0]
	if action != "up" && action != "down" && action != "version" {
		return errors.New(usage)
	}

	databaseURL, err := config.LoadDatabaseURL()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	runner, err := migrator.Open(databaseURL)
	if err != nil {
		return err
	}

	operationErr := execute(runner, action, output)
	sourceCloseErr, databaseCloseErr := runner.Close()
	if operationErr != nil {
		return operationErr
	}
	if sourceCloseErr != nil || databaseCloseErr != nil {
		return errors.New("close migration runner")
	}

	return nil
}

func execute(runner *migrate.Migrate, action string, output io.Writer) error {
	switch action {
	case "up":
		if err := runner.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("apply migrations: %w", err)
		}
	case "down":
		if err := runner.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("roll back migration: %w", err)
		}
	case "version":
		version, dirty, err := runner.Version()
		if errors.Is(err, migrate.ErrNilVersion) {
			_, err = fmt.Fprintln(output, "version 0 (clean)")
			return err
		}
		if err != nil {
			return errors.New("read migration version")
		}
		state := "clean"
		if dirty {
			state = "dirty"
		}
		if _, err := fmt.Fprintf(output, "version %d (%s)\n", version, state); err != nil {
			return fmt.Errorf("write migration version: %w", err)
		}
	default:
		return errors.New(usage)
	}

	return nil
}
