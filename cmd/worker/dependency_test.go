package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestWorkerDependencyGraphHasNoPostgreSQLDriver(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("list dependencies: %v: %s", err, output)
	}
	if strings.Contains(string(output), "pgx") || strings.Contains(string(output), "lib/pq") {
		t.Fatal("worker dependency graph contains a PostgreSQL driver")
	}
}
