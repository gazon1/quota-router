package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// VACUUM INTO refuses to overwrite, so an existing target is an error rather
// than a silent replacement. The snapshot goes to a private temporary file in
// the same directory and is published under the target name with a hard link,
// which cannot clobber: of two concurrent backups with one target, one wins and
// the loser removes only its own temporary file.
func (s *Store) Backup(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("backup: path must not be empty")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup: %s already exists", path)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".backup-*.db")
	if err != nil {
		return fmt.Errorf("backup to %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// VACUUM INTO needs to create the file itself.
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("backup to %s: %w", path, err)
	}

	// The path is interpolated into SQL, and SQLite string literals escape a
	// single quote by doubling it.
	_, err = s.db.ExecContext(ctx, "VACUUM INTO '"+sqlQuote(tmpPath)+"'")
	if err != nil {
		os.Remove(tmpPath) // a failed VACUUM INTO can leave a partial file behind
		return fmt.Errorf("backup to %s: %w", path, classify(err))
	}
	// os.Rename would overwrite a target that appeared meanwhile, so publish with a
	// hard link instead: it fails with EEXIST, which is the contract callers
	// asked for — a new file, never a silent replacement.
	if err := os.Link(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("backup: %s already exists", path)
		}
		return fmt.Errorf("backup to %s: %w", path, err)
	}
	// The link is the published backup; the temporary name is now one more link
	// to the same inode and can go.
	return os.Remove(tmpPath)
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
