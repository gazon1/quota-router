package store

import "embed"

// migrationsFS carries the versioned SQL migrations inside the binary, so the
// controller needs no migration files beside it at run time. Goose reads the
// embedded filesystem directly; there is no second copy of the SQL.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS
