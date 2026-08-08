// Package migrations exposes the versioned SQL migrations to backend commands.
package migrations

import "embed"

// Files contains every up and down migration compiled into the migration
// command so migrations do not depend on its current working directory.
//
//go:embed *.sql
var Files embed.FS
