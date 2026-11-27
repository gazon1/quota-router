package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// TestBrokenMigrationRollsBack is the rollback path: a migration whose second
// statement fails must leave the database exactly at the previous version, with
// no half-created table and the failing version unrecorded.
func TestBrokenMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(Config{Path: filepath.Join(dir, "test.db"), BusyTimeout: 5e9}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A copy of the real migrations plus one that creates a table and then fails.
	mig := t.TempDir()
	apply(t, mig, "0001_persistence.sql", readMigration(t, "0001_persistence.sql"))
	apply(t, mig, "0003_broken.sql", `
-- +goose Up
CREATE TABLE half_applied (id INTEGER PRIMARY KEY);
INSERT INTO no_such_table (id) VALUES (1);

-- +goose Down
DROP TABLE half_applied;
`)
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, os.DirFS(mig),
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.Up(ctx); err == nil {
		t.Fatal("a broken migration was applied without error")
	}

	// The failed migration's first statement must have rolled back with it.
	var name string
	if err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE name='half_applied'").Scan(&name); err == nil {
		t.Fatal("half_applied survived: the failed migration was not rolled back")
	}
	var applied int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM goose_db_version WHERE version_id = 3 AND is_applied = 1").Scan(&applied); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if applied != 0 {
		t.Fatal("the failed version is recorded as applied")
	}
	// The versions that already committed must still be intact and usable:
	// startup can report where it stopped.
	v, err := s.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != 2 {
		t.Fatalf("version = %d after a failed migration, want 2", v)
	}
	if err := s.SaveUsageRecord(ctx, UsageRecord{Provider: "p", Provenance: Reported, ObservedAt: time.Now()}); err != nil {
		t.Fatalf("the database is not usable after a rolled-back migration: %v", err)
	}
}

// TestMigrateReportsAFailureClearly asserts startup gets a real cause, not a
// bare "migration failed".
func TestMigrateReportsAFailureClearly(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file should be: SQLite cannot open it.
	bad := filepath.Join(dir, "adir")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Path: bad, BusyTimeout: 5e9}, testLog())
	if err == nil {
		s.Close()
		t.Fatal("Open succeeded on a directory")
	}
	if !errors.Is(err, ErrIO) && !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want a classified storage error naming the path", err)
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(migrationsFS, "migrations/"+name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func apply(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
