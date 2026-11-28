package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// openMigrated returns a store on a fresh database in a temp directory.
func openMigrated(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "Open")
	require.NoError(t, s.Migrate(context.Background()), "Migrate")
	t.Cleanup(func() { s.Close() })
	return s, path
}

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	s, path := openMigrated(t)

	v, err := s.Version(context.Background())
	require.NoError(t, err, "Version")
	assert.Equal(t, int64(7), v)

	// A restart must not re-apply or fail: this is the existing-database path.
	require.NoError(t, s.Migrate(context.Background()), "second Migrate")
	s.Close()

	again, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "reopen")
	defer again.Close()
	require.NoError(t, again.Migrate(context.Background()), "Migrate after restart")
	for _, table := range []string{"quota_snapshots", "quota_windows", "usage_records", "provider_health", "controller_state", "poll_state", "routing_decisions", "routing_decision_candidates", "session_affinity"} {
		var name string
		require.NoError(t, again.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name),
			"table %s missing after restart", table)
	}
}

func TestSettingsAppliedOnEveryConnection(t *testing.T) {
	s, _ := openMigrated(t)

	// Every pooled connection must carry the pragmas, not just the first.
	for i := 0; i < MaxConns; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := s.db.Conn(ctx)
		require.NoError(t, err, "conn %d", i)
		defer conn.Close()
		var mode string
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode), "journal_mode")
		assert.Equal(t, "wal", mode, "conn %d journal_mode", i)
		var timeout int
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout), "busy_timeout")
		assert.Equal(t, 5000, timeout, "conn %d busy_timeout", i)
	}
}

func TestQuotaSnapshotRoundTripAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s, path := openMigrated(t)

	reason := ReasonFetchFailed
	reset := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	snap := QuotaSnapshot{
		Provider:      "minimax",
		State:         QuotaUnknown,
		UnknownReason: &reason,
		Source:        "providers.minimax.quota",
		Confidence:    Reported,
		ObservedAt:    time.Date(2026, 11, 26, 17, 30, 0, 0, time.UTC),
		Error:         "http 502 from quota endpoint",
		Windows: []QuotaWindow{{
			Window: "5h", Unit: "percent",
			Limit: floatPtr(100), Used: floatPtr(40), Remaining: floatPtr(60),
			ResetAt: &reset,
		}},
	}
	require.NoError(t, s.SaveQuotaSnapshot(ctx, snap), "SaveQuotaSnapshot")
	s.Close()

	// Restart: the state must survive with its provenance and window values.
	reopened, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "reopen")
	defer reopened.Close()

	got, err := reopened.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	require.Len(t, got, 1)
	g := got[0]
	assert.Equal(t, snap.Provider, g.Provider)
	assert.Equal(t, snap.State, g.State)
	assert.Equal(t, snap.Source, g.Source)
	assert.Equal(t, snap.Confidence, g.Confidence)
	require.NotNil(t, g.UnknownReason)
	assert.Equal(t, reason, *g.UnknownReason)
	assert.Equal(t, snap.Error, g.Error)
	assert.WithinDuration(t, snap.ObservedAt, g.ObservedAt, 0)
	require.Len(t, g.Windows, 1)
	w := g.Windows[0]
	assert.Equal(t, "5h", w.Window)
	assert.Equal(t, "percent", w.Unit)
	require.NotNil(t, w.Limit)
	require.NotNil(t, w.Used)
	require.NotNil(t, w.Remaining)
	assert.Equal(t, 100.0, *w.Limit)
	assert.Equal(t, 40.0, *w.Used)
	assert.Equal(t, 60.0, *w.Remaining)
	require.NotNil(t, w.ResetAt)
	assert.WithinDuration(t, reset, *w.ResetAt, 0)
}

func TestLatestQuotaSnapshotPerProvider(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for i, snap := range []QuotaSnapshot{
		{Provider: "minimax", State: QuotaLow, Source: "a", Confidence: Reported, ObservedAt: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)},
		{Provider: "minimax", State: QuotaAvailable, Source: "a", Confidence: Reported, ObservedAt: time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)},
		{Provider: "command-code", State: QuotaExhausted, Source: "b", Confidence: Reported, ObservedAt: time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)},
	} {
		require.NoError(t, s.SaveQuotaSnapshot(ctx, snap), "SaveQuotaSnapshot #%d", i)
	}

	got, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	require.Len(t, got, 2, "one row each")
	byProvider := map[string]QuotaState{}
	for _, g := range got {
		byProvider[g.Provider] = g.State
	}
	assert.Equal(t, QuotaAvailable, byProvider["minimax"], "the newest observation wins")
	assert.Equal(t, QuotaExhausted, byProvider["command-code"])
}

func TestQuotaWindowValuesStayUnreportedWhenAbsent(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// A provider that reports only "remaining" must not have used/limit invented
	// as zero: unreported is distinct from 0.
	require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "q", Confidence: Reported,
		ObservedAt: time.Now(),
		Windows:    []QuotaWindow{{Window: "5h", Remaining: floatPtr(0)}},
	}), "SaveQuotaSnapshot")

	got, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	require.NotEmpty(t, got)
	w := got[0].Windows[0]
	assert.Nil(t, w.Limit, "limit was not reported")
	assert.Nil(t, w.Used, "used was not reported")
	require.NotNil(t, w.Remaining)
	assert.Equal(t, 0.0, *w.Remaining, "a reported 0")
}

func TestUsageIsSeparateAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	u := UsageRecord{
		Source: "test", RequestID: "req-1", Provider: "minimax", Model: "MiniMax-M3.1-Flash",
		InputTokens: intPtr(10), OutputTokens: intPtr(20),
		Provenance: Estimated, ObservedAt: time.Now(),
	}
	require.NoError(t, s.SaveUsageRecord(ctx, u), "SaveUsageRecord")
	// A retried report with the same ID must not double-count tokens.
	require.NoError(t, s.SaveUsageRecord(ctx, u), "duplicate SaveUsageRecord")

	var n, tokens int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*), SUM(input_tokens) FROM usage_records").Scan(&n, &tokens), "count")
	assert.Equal(t, 1, n)
	assert.Equal(t, 10, tokens)
	// Usage must never land in the quota tables: the two are separate signals.
	var snapshots int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots), "count snapshots")
	assert.Equal(t, 0, snapshots, "no quota snapshot after a usage write")
}

func TestUsageWithoutRequestIDIsKept(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	u := UsageRecord{Source: "test", Provider: "minimax", Provenance: Estimated, ObservedAt: time.Now()}
	for i := 0; i < 2; i++ {
		require.NoError(t, s.SaveUsageRecord(ctx, u), "SaveUsageRecord #%d", i)
	}
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 2, n, "without an id duplicates cannot be detected")
}

func TestHealthIsSeparateFromQuota(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// A healthy provider proves reachability only; it must not create a quota row
	// or imply AVAILABLE.
	require.NoError(t, s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Healthy, ObservedAt: time.Now()}),
		"SaveProviderHealth")
	health, err := s.LatestProviderHealth(ctx)
	require.NoError(t, err, "LatestProviderHealth")
	require.Len(t, health, 1)
	assert.Equal(t, Healthy, health[0].State)
	quota, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	assert.Empty(t, quota, "no quota rows after a health write")
}

func TestControllerStateUpserts(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	_, ok, err := s.ControllerState(ctx, "mode")
	require.NoError(t, err)
	assert.False(t, ok, "the state is unset before any write")
	for _, v := range []string{"shadow", "active"} {
		require.NoError(t, s.SaveControllerState(ctx, "mode", v, time.Now()), "SaveControllerState")
	}
	got, ok, err := s.ControllerState(ctx, "mode")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "active", got)
}

func TestSaveRejectsInvalidEnums(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	reason := ReasonStale
	for _, c := range []struct {
		name string
		err  error
	}{
		{"unknown state", s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "p", State: "BOGUS", Source: "s", Confidence: Reported, ObservedAt: time.Now()})},
		{"unknown without reason", s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "p", State: QuotaUnknown, Source: "s", Confidence: Reported, ObservedAt: time.Now()})},
		{"reason on a known state", s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "p", State: QuotaAvailable, UnknownReason: &reason, Source: "s", Confidence: Reported, ObservedAt: time.Now()})},
		{"bad confidence", s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "p", State: QuotaAvailable, Source: "s", Confidence: "guess", ObservedAt: time.Now()})},
		{"bad health", s.SaveProviderHealth(ctx, HealthRecord{Provider: "p", State: "SICK", ObservedAt: time.Now()})},
	} {
		assert.Error(t, c.err, "%s: saved, want an error", c.name)
	}
	// A rejected write must leave nothing behind.
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&n), "count")
	assert.Equal(t, 0, n, "no quota snapshots after rejected writes")
}

func TestUnknownReasonIsConstrainedByTheSchema(t *testing.T) {
	// The store validates first, so drive SQL directly: this asserts the database
	// itself refuses a known state with a reason, and UNKNOWN without one.
	s, _ := openMigrated(t)
	for _, q := range []string{
		`INSERT INTO quota_snapshots (provider, state, source, confidence, observed_at) VALUES ('p','UNKNOWN','s','reported','2026-01-01T00:00:00.000Z')`,
		`INSERT INTO quota_snapshots (provider, state, unknown_reason, source, confidence, observed_at) VALUES ('p','AVAILABLE','stale','s','reported','2026-01-01T00:00:00.000Z')`,
		`INSERT INTO quota_snapshots (provider, state, unknown_reason, source, confidence, observed_at) VALUES ('p','UNKNOWN','made-up','s','reported','2026-01-01T00:00:00.000Z')`,
	} {
		_, err := s.db.Exec(q)
		assert.Error(t, err, "the schema accepted %s", q)
	}
}

func TestConcurrentWritesAndReads(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// One writer per provider and readers polling the latest state: this is the
	// poller/status-server pattern, and it must not deadlock or lose writes.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
					Provider: "provider-" + string(rune('a'+p)), State: QuotaAvailable, Source: "s",
					Confidence: Reported, ObservedAt: time.Now(),
				}); err != nil {
					errs <- err
					return
				}
			}
		}(p)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := s.LatestQuotaSnapshot(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err, "concurrent access")
	}

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&n), "count")
	assert.Equal(t, 100, n)
}

func TestPruneKeepsTheLatestStateItMustNotRemove(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Two old snapshots for one provider and one old one for another: the newest
	// per provider must survive even though every row is past retention.
	for i, obs := range []time.Time{old, old.Add(time.Hour), old.Add(2 * time.Hour)} {
		provider := "minimax"
		if i == 2 {
			provider = "command-code"
		}
		require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: provider, State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: obs,
			Windows: []QuotaWindow{{Window: "5h"}},
		}), "SaveQuotaSnapshot")
	}
	require.NoError(t, s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Healthy, ObservedAt: old}),
		"SaveProviderHealth")
	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "minimax", Provenance: Estimated, ObservedAt: old}),
		"SaveUsageRecord")

	deleted, err := s.Prune(ctx, Retention{
		QuotaSnapshots: 24 * time.Hour,
		ProviderHealth: 24 * time.Hour,
		UsageRecords:   24 * time.Hour,
	}, old.Add(90*24*time.Hour))
	require.NoError(t, err, "Prune")
	// minimax: 2 of 2 rows are older than newest -> 1 kept; command-code: its
	// only row is the newest for that provider -> kept. So 1 of 3 goes.
	assert.Equal(t, int64(1), deleted["quota_snapshots"])
	latest, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	assert.Len(t, latest, 2, "one per provider")
	// The window rows of the deleted snapshot must go with it (cascade).
	var orphanWindows int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM quota_windows w WHERE NOT EXISTS (SELECT 1 FROM quota_snapshots s WHERE s.id = w.snapshot_id)`).Scan(&orphanWindows),
		"orphan windows")
	assert.Equal(t, 0, orphanWindows)
	// Health keeps its newest row per provider; usage has no such rule.
	assert.Equal(t, int64(0), deleted["provider_health"], "the newest per provider is kept")
	assert.Equal(t, int64(1), deleted["usage_records"])
}

func TestPruneKeepsEverythingWhenRetentionIsZero(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: old}),
		"SaveUsageRecord")
	deleted, err := s.Prune(ctx, Retention{}, time.Now())
	require.NoError(t, err, "Prune")
	assert.Empty(t, deleted, "nothing is deleted with zero retention")
}

func TestPruneIsAtomicOnFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// A cancelled context fails the transaction; nothing may be deleted.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: old}),
			"SaveUsageRecord")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := s.Prune(cancelled, Retention{UsageRecords: time.Hour}, time.Now())
	require.Error(t, err, "Prune with a cancelled context succeeded")
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 3, n, "nothing is deleted after a failed prune")
}

func TestBackupIsConsistentUnderWAL(t *testing.T) {
	ctx := context.Background()
	s, path := openMigrated(t)

	// Rows still in the WAL are the ones a file copy misses.
	require.NoError(t, s.SaveControllerState(ctx, "mode", "active", time.Now()), "SaveControllerState")
	backup := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, s.Backup(ctx, backup), "Backup")

	// The backup is one file: no sidecars, so no WAL to forget on restore.
	for _, sidecar := range []string{"-wal", "-shm"} {
		_, err := os.Stat(backup + sidecar)
		assert.Error(t, err, "backup left %s behind; VACUUM INTO writes a single file", sidecar)
	}
	bdb, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	require.NoError(t, err, "open backup")
	defer bdb.Close()
	var got string
	require.NoError(t, bdb.QueryRow("SELECT value FROM controller_state WHERE key='mode'").Scan(&got), "read backup")
	assert.Equal(t, "active", got, "the WAL rows were not captured")
	// A backup must not overwrite an existing file.
	require.Error(t, s.Backup(ctx, backup), "Backup overwrote an existing file")
	assert.NotEqual(t, backup, path, "test bug: backup path equals the live path")
}

func TestConcurrentBackupsToOneTargetKeepTheWinner(t *testing.T) {
	// Review finding: a failed VACUUM INTO used to remove the destination
	// unconditionally, so a racing loser could delete the winner's good backup.
	ctx := context.Background()
	s, _ := openMigrated(t)
	require.NoError(t, s.SaveControllerState(ctx, "mode", "active", time.Now()), "SaveControllerState")
	target := filepath.Join(t.TempDir(), "daily.db")

	const racers = 4
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Backup(ctx, target)
		}(i)
	}
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		assert.True(t,
			strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "file exists"),
			"racer %d: unexpected error %v", i, err)
	}
	assert.Equal(t, 1, winners, "exactly one racer must succeed")
	// The surviving file must be a complete backup, not a deleted or truncated one.
	_, err := os.Stat(target)
	require.NoError(t, err, "the winning backup is gone")
	bdb, err := sql.Open("sqlite", "file:"+target+"?mode=ro")
	require.NoError(t, err, "open backup")
	defer bdb.Close()
	var got string
	require.NoError(t, bdb.QueryRow("SELECT value FROM controller_state WHERE key='mode'").Scan(&got), "read backup")
	assert.Equal(t, "active", got)
	// No private temporary file was left behind.
	entries, err := os.ReadDir(filepath.Dir(target))
	require.NoError(t, err, "read dir")
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".backup-"), "temporary file %q survived", e.Name())
	}
}

func TestRelativePathWorks(t *testing.T) {
	// The default setting is a relative path ("quota-router.db"), so it must keep
	// working. A url.URL-based DSN rendered it as "file://quota-router.db" — an
	// authority, not a relative path — and the driver could not open anything.
	saved, err := os.Getwd()
	require.NoError(t, err, "getwd")
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir), "chdir")
	defer os.Chdir(saved)

	for _, path := range []string{"quota-router.db", filepath.Join("nested", "quota-router.db")} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o700), "mkdir")
		s, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
		require.NoError(t, err, "Open(%q)", path)
		require.NoError(t, s.Migrate(context.Background()), "Migrate(%q)", path)
		require.NoError(t, s.SaveControllerState(context.Background(), "k", "v", time.Now()), "write(%q)", path)
		s.Close()
		_, err = os.Stat(filepath.Join(dir, path))
		require.NoError(t, err, "relative path %q created no file", path)
	}
}

func TestDSNKeepsURISpecialCharactersInThePath(t *testing.T) {
	// Review finding: the path was concatenated into the SQLite URI, so "?" began
	// the query and the driver opened a different file than the one Migrate locks.
	for _, name := range []string{"qa?archive.db", "a#b.db", "with space.db", "100%.db", "plus+sign.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			s, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
			require.NoError(t, err, "Open")
			require.NoError(t, s.Migrate(context.Background()), "Migrate")
			require.NoError(t, s.SaveControllerState(context.Background(), "k", "v", time.Now()), "write")
			s.Close()

			// The file must exist under the configured name, and no other file may
			// have been created from the truncated URI.
			_, err = os.Stat(path)
			require.NoError(t, err, "database not created at %q", name)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err, "read dir")
			for _, e := range entries {
				base := e.Name()
				if base == name || base == name+"-wal" || base == name+"-shm" || base == name+".lock" {
					continue
				}
				assert.Fail(t, "unexpected file", "the path was cut at a URI delimiter: %q", base)
			}
			// And the migration lock pointed at the same file, so serialization holds.
			_, err = os.Stat(path + ".lock")
			assert.NoError(t, err, "migration lock is not beside the database")
		})
	}
}

func TestReadersAndRetentionAgreeOnTimestampTies(t *testing.T) {
	// Review finding: the readers returned every row sharing the maximum
	// timestamp while retention kept all of them, so one provider could report
	// two conflicting current states.
	ctx := context.Background()
	s, _ := openMigrated(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Two snapshots and two health rows for one provider at the same instant.
	for _, state := range []QuotaState{QuotaAvailable, QuotaExhausted} {
		require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "minimax", State: state, Source: "s", Confidence: Reported, ObservedAt: at,
		}), "SaveQuotaSnapshot")
	}
	for _, state := range []HealthState{Healthy, Unavailable} {
		require.NoError(t, s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: state, ObservedAt: at}),
			"SaveProviderHealth")
	}

	for i := 0; i < 5; i++ { // the tie must resolve the same way every time
		snaps, err := s.LatestQuotaSnapshot(ctx)
		require.NoError(t, err, "LatestQuotaSnapshot")
		assert.Len(t, snaps, 1, "exactly 1 per provider")
		health, err := s.LatestProviderHealth(ctx)
		require.NoError(t, err, "LatestProviderHealth")
		assert.Len(t, health, 1, "exactly 1 per provider")
	}

	// Retention must spare exactly the row the readers return, no more.
	before, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	_, err = s.Prune(ctx, Retention{QuotaSnapshots: time.Hour, ProviderHealth: time.Hour}, at.Add(90*24*time.Hour))
	require.NoError(t, err, "Prune")
	after, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot after prune")
	require.Len(t, after, 1)
	assert.Equal(t, before[0].State, after[0].State, "the reader and retention disagree on the tie")
	var snapshots, health int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots), "count")
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM provider_health").Scan(&health), "count health")
	assert.Equal(t, 1, snapshots)
	assert.Equal(t, 1, health)
}

func TestBackupRefusesAndLeavesNoLitter(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	dir := t.TempDir()

	for _, c := range []struct{ name, path string }{
		{"empty path", ""},
		{"missing directory", filepath.Join(dir, "nodir", "x.db")},
	} {
		assert.Error(t, s.Backup(ctx, c.path), "%s: Backup succeeded", c.name)
	}

	target := filepath.Join(dir, "ok.db")
	require.NoError(t, s.Backup(ctx, target), "Backup")
	// Refusing an existing target must not damage it either.
	require.Error(t, s.Backup(ctx, target), "Backup overwrote an existing file")
	_, err := os.Stat(target)
	require.NoError(t, err, "the existing backup was removed")
	// No private temporary file may survive either outcome.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read dir")
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".backup-"), "temporary file %q survived", e.Name())
	}
}

func TestDatabaseAndBackupFilesAreOwnerOnly(t *testing.T) {
	// Review finding: SQLite creates its files with the process umask, so under
	// the usual 0022 the database and every backup were world-readable.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	// The previously common umask, restored for the rest of the suite.
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	s, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "Open")
	require.NoError(t, s.Migrate(ctx), "Migrate")
	require.NoError(t, s.SaveControllerState(ctx, "k", "v", time.Now()), "write")

	backup := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, s.Backup(ctx, backup), "Backup")

	for _, p := range []string{path, backup} {
		st, err := os.Stat(p)
		require.NoError(t, err, "stat %s", p)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "%s mode: the file holds provider observations", filepath.Base(p))
	}
	s.Close()

	// An existing database created 0644 by an earlier build is tightened on open,
	// so an upgrade does not leave it exposed.
	require.NoError(t, os.Chmod(path, 0o644), "chmod")
	again, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "reopen")
	defer again.Close()
	st, err := os.Stat(path)
	require.NoError(t, err, "stat")
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "existing database mode after open")
}

func TestSubMillisecondObservationsDoNotTie(t *testing.T) {
	// Review finding: millisecond truncation turned distinct observations into a
	// tie, and the id tie-break then let a LATE, OLDER write outrank the newer one.
	ctx := context.Background()
	s, _ := openMigrated(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := base.Add(900 * time.Microsecond)

	// The newer observation is written first, so the older one gets the higher id.
	require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: newer,
	}), "newer")
	require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaExhausted, Source: "s", Confidence: Reported, ObservedAt: base,
	}), "older")

	got, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	require.Len(t, got, 1)
	assert.Equal(t, QuotaAvailable, got[0].State, "the late older write outranked the newer observation")
	assert.WithinDuration(t, newer.Truncate(time.Nanosecond), got[0].ObservedAt, 0, "precision was lost")
	// Retention must agree: the same row is the one that survives.
	_, err = s.Prune(ctx, Retention{QuotaSnapshots: time.Hour}, base.Add(90*24*time.Hour))
	require.NoError(t, err, "Prune")
	after, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot after prune")
	require.Len(t, after, 1)
	assert.Equal(t, QuotaAvailable, after[0].State, "the same AVAILABLE row must survive")
}

func TestMemoryPathStillPersists(t *testing.T) {
	// Review finding: a path STARTING with ":" is a SQLite URI form, so a
	// relative ":memory:" opened an in-memory database while the controller
	// believed it had a file — every observation was lost on restart, with no
	// startup error. It has to be a relative path to reproduce: an absolute
	// "/tmp/x/:memory:" is an ordinary file name.
	ctx := context.Background()
	saved, err := os.Getwd()
	require.NoError(t, err, "getwd")
	require.NoError(t, os.Chdir(t.TempDir()), "chdir")
	defer os.Chdir(saved)

	s, err := Open(Config{Path: ":memory:", BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "Open")
	require.NoError(t, s.Migrate(ctx), "Migrate")
	require.NoError(t, s.SaveControllerState(ctx, "k", "v", time.Now()), "write")
	s.Close()

	// A new process must see the row: that is the whole point of the setting.
	again, err := Open(Config{Path: ":memory:", BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "reopen")
	defer again.Close()
	v, ok, err := again.ControllerState(ctx, "k")
	require.NoError(t, err, "ControllerState")
	require.True(t, ok, "state did not survive the reopen: the path opened an in-memory database")
	assert.Equal(t, "v", v)
}

func TestBackupSnapshotIsPrivateWhileItIsWritten(t *testing.T) {
	// Review finding: the in-progress snapshot was created with the process umask
	// and only chmodded after VACUUM INTO finished, so it was readable while the
	// backup ran.
	ctx := context.Background()
	s, _ := openMigrated(t)
	dir := t.TempDir()

	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	target := filepath.Join(dir, "backup.db")
	require.NoError(t, s.Backup(ctx, target), "Backup")
	// Nothing private may be left behind, and the published file is owner-only.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read dir")
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".backup-"), "temporary backup %q survived", e.Name())
	}
	st, err := os.Stat(target)
	require.NoError(t, err, "stat")
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

func TestDirectoryPathKeepsItsMode(t *testing.T) {
	// Review finding: chmodDB could chmod a misconfigured directory path. It must
	// refuse before changing anything.
	dir := filepath.Join(t.TempDir(), "adir")
	require.NoError(t, os.Mkdir(dir, 0o700), "mkdir")
	require.NoError(t, os.Chmod(dir, 0o700), "chmod")
	_, err := Open(Config{Path: dir, BusyTimeout: time.Second}, testLog())
	require.Error(t, err, "Open succeeded on a directory")
	st, err := os.Stat(dir)
	require.NoError(t, err, "stat")
	assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "the directory mode is unchanged after a failed Open")
}

func TestStoredErrorsMustBeSafeDescriptions(t *testing.T) {
	// Review finding: Error was an unrestricted string at a persistence
	// boundary, so a provider adapter passing err.Error() would write an
	// authorization header into a database that gets backed up.
	ctx := context.Background()
	s, _ := openMigrated(t)

	unsafe := []string{
		`http 401: Authorization: Bearer abc123`,
		`request failed with header x-api-key: secret`,
		`provider said: invalid api_key=sk-live-1234`,
		`https://provider.example/api?access_token=secret123`,
		`https://provider.example/api?client_secret=abc`,
		`refresh failed for access-token=abc`,
		`refresh failed for client-secret=abc`,
		strings.Repeat("x", maxError+1), // a raw body, not a description
	}
	for _, e := range unsafe {
		snap := QuotaSnapshot{
			Provider: "p", State: QuotaUnknown, UnknownReason: ptr(ReasonFetchFailed),
			Source: "s", Confidence: Reported, ObservedAt: time.Now(), Error: e,
		}
		assert.Error(t, s.SaveQuotaSnapshot(ctx, snap), "quota snapshot stored error %q", e)
		assert.Error(t, s.SaveProviderHealth(ctx, HealthRecord{
			Provider: "p", State: Unavailable, ObservedAt: time.Now(), Error: e,
		}), "health stored error %q", e)
	}

	// A short reason that names no credential is still accepted.
	require.NoError(t, s.SaveProviderHealth(ctx, HealthRecord{
		Provider: "p", State: Degraded, ObservedAt: time.Now(), Error: "http 503 from quota endpoint",
	}), "safe description rejected")
}

func ptr[T any](v T) *T { return &v }

func TestSymlinkedDatabaseSharesOneMigrationLock(t *testing.T) {
	// Review finding: the lock name came from the configured path, so two
	// controllers naming one database through a symlink took different locks and
	// could migrate it concurrently.
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	alias := filepath.Join(dir, "alias.db")
	require.NoError(t, os.WriteFile(real, nil, 0o600), "create")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	viaReal, err := Open(Config{Path: real, BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "open real")
	defer viaReal.Close()
	viaAlias, err := Open(Config{Path: alias, BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "open alias")
	defer viaAlias.Close()

	assert.Equal(t, viaReal.path, viaAlias.path, "the aliases would migrate concurrently")
}

func TestDiskFullIsReportedAndLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// Shrinking the page budget is how SQLite reports SQLITE_FULL without
	// filling a real disk.
	_, err := s.db.Exec("PRAGMA max_page_count = 4")
	require.NoError(t, err, "max_page_count")
	// A snapshot with windows exercises the multi-statement transaction: if the
	// window insert fails, the snapshot row must not survive alone.
	var sawFull bool
	for i := 0; i < 100 && !sawFull; i++ {
		err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "p", State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: time.Now(),
			Windows: []QuotaWindow{{Window: "5h", Limit: floatPtr(1), Used: floatPtr(0)}},
		})
		if err == nil {
			continue
		}
		require.ErrorIs(t, err, ErrFull)
		sawFull = true
	}
	require.True(t, sawFull, "never hit SQLITE_FULL; the test could not exercise the failure")
	// No half-written snapshot: the transaction rolled back as a unit.
	var snapshots, windows int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots),
		"the database stopped being readable after ErrFull")
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_windows").Scan(&windows), "count windows")
	assert.Equal(t, snapshots, windows, "a failed write left a partial record")
}

func TestBusyIsClassifiedAndLosesNoWrite(t *testing.T) {
	// Another writer holding the lock past busy_timeout is the SQLITE_BUSY path
	// issue #12 names. A zero busy timeout makes the conflict happen at once
	// instead of waiting a second.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
	require.NoError(t, err, "Open")
	defer s.Close()
	require.NoError(t, s.Migrate(ctx), "Migrate")

	blocker, err := sql.Open("sqlite", "file:"+path+"?_txlock=exclusive&_pragma=busy_timeout(0)")
	require.NoError(t, err, "open blocker")
	defer blocker.Close()
	held, err := blocker.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	// An exclusive transaction with a real write, so the lock is genuinely held.
	_, err = held.Exec("INSERT INTO controller_state (key, value, updated_at) VALUES ('k','v','2026-01-01T00:00:00.000Z')")
	require.NoError(t, err, "take the write lock")

	// A second store with no patience: it must report the conflict as ErrBusy,
	// not as an unknown driver error.
	impatient, err := Open(Config{Path: path, BusyTimeout: 1 * time.Millisecond}, testLog())
	require.NoError(t, err, "open impatient")
	defer impatient.Close()
	err = impatient.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: time.Now()})
	assert.ErrorIs(t, err, ErrBusy, "the conflict must be reported while another writer holds the lock")

	require.NoError(t, held.Rollback(), "rollback")
	// Once the lock is free the write lands, and exactly once.
	require.NoError(t, impatient.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: time.Now()}),
		"write after the lock was released")
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 1, n, "the failed write must not have landed")
}

func TestMigrationLockIsExclusiveAcrossHandles(t *testing.T) {
	// Two controllers racing to migrate must serialize, not run concurrently.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	first, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "Open")
	defer first.Close()
	second, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "Open")
	defer second.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, st := range []*Store{first, second} {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			if err := st.Migrate(context.Background()); err != nil {
				errs <- err
			}
		}(st)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err, "concurrent Migrate")
	}
	// Both ran, the schema exists once, and the version is recorded once.
	v, err := first.Version(context.Background())
	require.NoError(t, err, "Version")
	assert.Equal(t, int64(7), v)
	var applied int
	require.NoError(t, first.db.QueryRow("SELECT COUNT(*) FROM goose_db_version WHERE version_id = 1 AND is_applied = 1").Scan(&applied),
		"count versions")
	assert.Equal(t, 1, applied)
}

func TestPruneSparesTheSameRowTheReadersReturn(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A late write with an older observed_at: the row with the highest id is not
	// the newest observation, so pruning by id would delete what routing reads.
	require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: base.Add(2 * time.Hour),
	}), "newest")
	require.NoError(t, s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaLow, Source: "s", Confidence: Reported, ObservedAt: base,
	}), "late stale write")
	require.NoError(t, s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Healthy, ObservedAt: base.Add(2 * time.Hour)}),
		"health newest")
	require.NoError(t, s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Unavailable, ObservedAt: base}),
		"health late write")

	before, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot")
	healthBefore, err := s.LatestProviderHealth(ctx)
	require.NoError(t, err, "LatestProviderHealth")

	// Everything is old enough to expire, so only the spared rows may survive.
	_, err = s.Prune(ctx, Retention{QuotaSnapshots: time.Hour, ProviderHealth: time.Hour}, base.Add(90*24*time.Hour))
	require.NoError(t, err, "Prune")

	after, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot after prune")
	require.Len(t, after, 1)
	assert.Equal(t, before[0].State, after[0].State, "prune removed the row the reader returns")
	assert.WithinDuration(t, before[0].ObservedAt, after[0].ObservedAt, 0)
	healthAfter, err := s.LatestProviderHealth(ctx)
	require.NoError(t, err, "LatestProviderHealth after prune")
	require.Len(t, healthAfter, 1)
	assert.Equal(t, healthBefore[0].State, healthAfter[0].State, "prune removed the row the reader returns")
}

func TestFailedWriteLeavesThePreviousStateReadable(t *testing.T) {
	// The issue's rule: a failed write must not replace valid in-memory state with
	// incomplete data. The persisted latest row must stay the last good one.
	ctx := context.Background()
	s, _ := openMigrated(t)

	good := QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "s", Confidence: Reported,
		ObservedAt: time.Now(), Windows: []QuotaWindow{{Window: "5h", Remaining: floatPtr(50)}},
	}
	require.NoError(t, s.SaveQuotaSnapshot(ctx, good), "SaveQuotaSnapshot")

	// Fail the next write at the database: a broken foreign key target cannot be
	// reached through the public API, so shrink the page budget and write large
	// records until SQLite reports it.
	_, err := s.db.Exec("PRAGMA max_page_count = 8")
	require.NoError(t, err, "max_page_count")
	committed, sawFull := 1, false
	for i := 0; i < 100 && !sawFull; i++ {
		err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "minimax", State: QuotaExhausted, Source: "s", Confidence: Reported,
			ObservedAt: time.Now().Add(time.Duration(i+1) * time.Second),
			Error:      strings.Repeat("x", maxError-1), // a full, accepted description
			Windows:    []QuotaWindow{{Window: "5h", Remaining: floatPtr(0)}},
		})
		if err == nil {
			committed++
			continue
		}
		require.ErrorIs(t, err, ErrFull)
		sawFull = true
	}
	require.True(t, sawFull, "never hit SQLITE_FULL; the test could not exercise the failure")

	// Exactly the committed writes are visible: a failed write left nothing.
	var snapshots int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots), "count snapshots")
	assert.Equal(t, committed, snapshots, "a failed write left a partial record")
	// Every readable snapshot pair is complete: no snapshot row without its window.
	var incomplete int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots q
		WHERE NOT EXISTS (SELECT 1 FROM quota_windows w WHERE w.snapshot_id = q.id)`).Scan(&incomplete), "count incomplete")
	assert.Equal(t, 0, incomplete, "a failed write left a partial record")
	// And the reader still answers, so a failed write did not break routing state.
	latest, err := s.LatestQuotaSnapshot(ctx)
	require.NoError(t, err, "LatestQuotaSnapshot after a failed write")
	assert.Len(t, latest, 1)
	t.Logf("committed %d snapshots before the page budget ran out", committed)
}

func TestOpenFailsClearlyOnAnUnusablePath(t *testing.T) {
	// A directory that does not exist must fail at startup, not on first write.
	missing := filepath.Join(t.TempDir(), "no-such-dir", "test.db")
	_, err := Open(Config{Path: missing, BusyTimeout: time.Second}, testLog())
	require.Error(t, err, "Open succeeded on a path in a missing directory")
}

func TestReadOnlyFileFailsWritesWithErrReadOnly(t *testing.T) {
	ctx := context.Background()
	s, path := openMigrated(t)
	s.Close()

	if err := os.Chmod(path, 0o400); err != nil {
		t.Skipf("cannot drop write permission: %v", err)
	}
	defer os.Chmod(path, 0o600)

	ro, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
	if err != nil {
		return // some systems refuse to open read-write at all; that is also fine
	}
	defer ro.Close()
	err = ro.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: time.Now()})
	if err == nil {
		t.Skip("the filesystem allowed a write to a read-only file (running as root?)")
	}
	assert.True(t,
		errors.Is(err, ErrReadOnly) || errors.Is(err, ErrIO) || errors.Is(err, ErrBusy),
		"error = %v, want a classified storage error", err)
}

// Last attempt and last success are independent: a failure moves one and must
// leave the other alone, and "never succeeded" stays distinguishable from zero.
func TestPollStateTracksAttemptAndSuccessSeparately(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	attempt, success, err := s.PollState(ctx, "minimax")
	require.NoError(t, err, "PollState on an unpolled provider")
	assert.True(t, attempt.IsZero(), "the last attempt is zero before any poll")
	assert.True(t, success.IsZero(), "the last success is zero before any poll")

	first := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.SavePollState(ctx, "minimax", first, first), "SavePollState")
	later := first.Add(time.Minute)
	require.NoError(t, s.SavePollState(ctx, "minimax", later, time.Time{}), "SavePollState after a failure")

	attempt, success, err = s.PollState(ctx, "minimax")
	require.NoError(t, err, "PollState")
	assert.WithinDuration(t, later, attempt, 0, "the last attempt is the failed attempt's time")
	assert.WithinDuration(t, first, success, 0, "a failure must not erase the earlier success")

	// Providers do not share a row.
	require.NoError(t, s.SavePollState(ctx, "other", later, time.Time{}), "SavePollState")
	a, succ, err := s.PollState(ctx, "other")
	require.NoError(t, err, "PollState")
	assert.WithinDuration(t, later, a, 0, "its own attempt")
	assert.True(t, succ.IsZero(), "no success for the other provider")
	a, _, err = s.PollState(ctx, "minimax")
	require.NoError(t, err, "PollState")
	assert.WithinDuration(t, later, a, 0, "untouched by another provider")

	require.Error(t, s.SavePollState(ctx, "", later, later), "SavePollState accepted an empty provider")
	require.Error(t, s.SavePollState(ctx, "minimax", time.Time{}, later), "SavePollState accepted a zero attempt time")
}

// A pin must survive a restart with its timestamps intact: that is what makes
// affinity something a controller can lose its process on and still honour.
func TestSessionAffinitySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, path := openMigrated(t)
	created := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	pin := SessionAffinity{SessionID: "sess-1", Alias: "cc-coding", CreatedAt: created, UpdatedAt: created.Add(time.Hour), ExpiresAt: created.Add(25 * time.Hour)}
	require.NoError(t, s.SaveSessionAffinity(ctx, pin), "SaveSessionAffinity")
	s.Close()

	reopened, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "reopen")
	defer reopened.Close()
	require.NoError(t, reopened.Migrate(ctx), "Migrate after restart")
	got, ok, err := reopened.SessionAffinity(ctx, "sess-1")
	require.NoError(t, err, "SessionAffinity")
	require.True(t, ok, "pin lost across restart")
	assert.Equal(t, pin, got)
}

// A re-pin moves the route and the timestamps a re-pin owns, but not when the
// session was first pinned.
func TestSessionAffinityRepinKeepsCreatedAt(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	created := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	first := SessionAffinity{SessionID: "sess-1", Alias: "cc-coding", CreatedAt: created, UpdatedAt: created, ExpiresAt: created.Add(time.Hour)}
	require.NoError(t, s.SaveSessionAffinity(ctx, first), "SaveSessionAffinity")
	second := SessionAffinity{SessionID: "sess-1", Alias: "mm-fast", CreatedAt: created, UpdatedAt: created.Add(30 * time.Minute), ExpiresAt: created.Add(2 * time.Hour)}
	require.NoError(t, s.SaveSessionAffinity(ctx, second), "re-pin")
	got, ok, err := s.SessionAffinity(ctx, "sess-1")
	require.NoError(t, err, "SessionAffinity")
	require.True(t, ok)
	assert.Equal(t, "mm-fast", got.Alias)
	assert.WithinDuration(t, second.UpdatedAt, got.UpdatedAt, 0)
	assert.WithinDuration(t, second.ExpiresAt, got.ExpiresAt, 0)
	assert.WithinDuration(t, created, got.CreatedAt, 0, "the original creation time")
}

// Release deletes the pin and is repeatable: a session that was never pinned, or
// already released, is not an error.
func TestSessionAffinityDeleteIsRepeatable(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Now().UTC()
	require.NoError(t, s.DeleteSessionAffinity(ctx, "never-pinned"), "DeleteSessionAffinity on an unpinned session")
	require.NoError(t, s.SaveSessionAffinity(ctx, SessionAffinity{SessionID: "sess-1", Alias: "cc-coding", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}),
		"SaveSessionAffinity")
	for i := 0; i < 2; i++ {
		require.NoError(t, s.DeleteSessionAffinity(ctx, "sess-1"), "DeleteSessionAffinity #%d", i+1)
	}
	_, ok, err := s.SessionAffinity(ctx, "sess-1")
	require.NoError(t, err)
	assert.False(t, ok, "pin still readable after release")
}

// Validation refuses a row that could not be honoured: no session, no route,
// missing timestamps, or an expiry that is not in the row's own future.
func TestSessionAffinityValidation(t *testing.T) {
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	good := SessionAffinity{SessionID: "s", Alias: "a", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	require.NoError(t, good.Validate(), "valid row refused")
	for _, bad := range []SessionAffinity{
		{SessionID: "", Alias: "a", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{SessionID: "s", Alias: "", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{SessionID: "s", Alias: "a", CreatedAt: now, UpdatedAt: now},
		{SessionID: "s", Alias: "a", CreatedAt: now, UpdatedAt: now, ExpiresAt: now},
		{SessionID: "s", Alias: "a", CreatedAt: now, UpdatedAt: now.Add(time.Hour), ExpiresAt: now.Add(30 * time.Minute)},
		{SessionID: "s", Alias: "a", CreatedAt: now, UpdatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	} {
		assert.Error(t, bad.Validate(), "invalid row accepted: %+v", bad)
	}
	s, _ := openMigrated(t)
	require.Error(t, s.SaveSessionAffinity(context.Background(), SessionAffinity{SessionID: "s"}),
		"SaveSessionAffinity accepted an invalid row")
}

// Expired pins are routing state whose TTL already ran out, so a retention run
// removes them even when every configured age is zero, and a live pin survives.
func TestPruneRemovesExpiredAffinityOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	old := SessionAffinity{SessionID: "old", Alias: "cc-coding", CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-24 * time.Hour)}
	live := SessionAffinity{SessionID: "live", Alias: "mm-fast", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	for _, p := range []SessionAffinity{old, live} {
		require.NoError(t, s.SaveSessionAffinity(ctx, p), "SaveSessionAffinity")
	}
	deleted, err := s.Prune(ctx, Retention{}, now)
	require.NoError(t, err, "Prune")
	assert.Equal(t, int64(1), deleted["session_affinity"])
	_, ok, err := s.SessionAffinity(ctx, "old")
	require.NoError(t, err)
	assert.False(t, ok, "expired pin survived retention")
	_, ok, err = s.SessionAffinity(ctx, "live")
	require.NoError(t, err)
	assert.True(t, ok, "live pin removed")
}
