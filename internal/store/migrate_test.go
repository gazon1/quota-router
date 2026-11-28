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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrokenMigrationRollsBack is the rollback path: a migration whose second
// statement fails must leave the database exactly at the previous version, with
// no half-created table and the failing version unrecorded.
func TestBrokenMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(Config{Path: filepath.Join(dir, "test.db"), BusyTimeout: 5e9}, testLog())
	require.NoError(t, err, "Open")
	defer s.Close()
	require.NoError(t, s.Migrate(ctx), "Migrate")

	// A copy of the real migrations plus one that creates a table and then fails.
	mig := t.TempDir()
	apply(t, mig, "0001_persistence.sql", readMigration(t, "0001_persistence.sql"))
	apply(t, mig, "0002_poll_state.sql", readMigration(t, "0002_poll_state.sql"))
	apply(t, mig, "0003_usage_attempts.sql", readMigration(t, "0003_usage_attempts.sql"))
	apply(t, mig, "0004_routing_decisions.sql", readMigration(t, "0004_routing_decisions.sql"))
	apply(t, mig, "0005_provider_cooldown.sql", readMigration(t, "0005_provider_cooldown.sql"))
	apply(t, mig, "0006_session_affinity.sql", readMigration(t, "0006_session_affinity.sql"))
	apply(t, mig, "0007_broken.sql", `
-- +goose Up
CREATE TABLE half_applied (id INTEGER PRIMARY KEY);
INSERT INTO no_such_table (id) VALUES (1);

-- +goose Down
DROP TABLE half_applied;
`)
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, os.DirFS(mig),
		goose.WithDisableGlobalRegistry(true))
	require.NoError(t, err, "NewProvider")
	_, err = provider.Up(ctx)
	require.Error(t, err, "a broken migration was applied without error")

	// The failed migration's first statement must have rolled back with it.
	var name string
	err = s.db.QueryRow("SELECT name FROM sqlite_master WHERE name='half_applied'").Scan(&name)
	assert.Error(t, err, "half_applied survived: the failed migration was not rolled back")
	var applied int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM goose_db_version WHERE version_id = 7 AND is_applied = 1").Scan(&applied),
		"count versions")
	assert.Equal(t, 0, applied, "the failed version is recorded as applied")
	// The versions that already committed must still be intact and usable:
	// startup can report where it stopped.
	v, err := s.Version(ctx)
	require.NoError(t, err, "Version")
	assert.Equal(t, int64(6), v, "version after a failed migration")
	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Reported, ObservedAt: time.Now()}),
		"the database is not usable after a rolled-back migration")
}

// TestMigrateReportsAFailureClearly asserts startup gets a real cause, not a
// bare "migration failed".
func TestMigrateReportsAFailureClearly(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file should be: SQLite cannot open it.
	bad := filepath.Join(dir, "adir")
	require.NoError(t, os.Mkdir(bad, 0o700), "mkdir")
	s, err := Open(Config{Path: bad, BusyTimeout: 5e9}, testLog())
	if err == nil {
		s.Close()
		require.Fail(t, "Open succeeded on a directory")
	}
	assert.True(t,
		errors.Is(err, ErrIO) || errors.Is(err, ErrBusy),
		"error = %v, want a classified storage error naming the path", err)
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(migrationsFS, "migrations/"+name)
	require.NoError(t, err, "read %s", name)
	return string(b)
}

func apply(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600), "write %s", name)
}
