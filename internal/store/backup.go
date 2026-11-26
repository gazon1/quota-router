package store

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Backup writes a consistent copy of the database to path using VACUUM INTO.
//
// A plain file copy of a WAL database is not a backup: rows still in "-wal" are
// missed, and copying while the controller writes yields a torn image.
// VACUUM INTO resolves the WAL inside SQLite, holds a read transaction for
// consistency, and writes one self-contained file with no "-wal"/"-shm"
// sidecars. Restore is: stop the controller, replace the database with the
// backup, remove any stale "-wal"/"-shm", start.
//
// VACUUM INTO refuses to overwrite, so the target must not exist.
func (s *Store) Backup(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("backup: path must not be empty")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup: %s already exists", path)
	}
	// The path is interpolated into SQL, and SQLite string literals escape a
	// single quote by doubling it.
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+sqlQuote(path)+"'"); err != nil {
		os.Remove(path) // a failed VACUUM INTO can leave a partial file behind
		return fmt.Errorf("backup to %s: %w", path, classify(err))
	}
	return nil
}

func sqlQuote(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, v[i])
	}
	return string(out)
}
