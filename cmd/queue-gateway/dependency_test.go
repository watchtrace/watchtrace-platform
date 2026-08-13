package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGatewayDependencyGraphHasNoPostgreSQLDriver(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("list dependencies: %v: %s", err, output)
	}
	if strings.Contains(string(output), "pgx") || strings.Contains(string(output), "lib/pq") {
		t.Fatal("gateway dependency graph contains a PostgreSQL driver")
	}
}
