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
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// openMigrated returns a store on a fresh database in a temp directory.
func openMigrated(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	s, path := openMigrated(t)

	v, err := s.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != 3 {
		t.Fatalf("version = %d, want 3", v)
	}

	// A restart must not re-apply or fail: this is the existing-database path.
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	s.Close()

	again, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if err := again.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate after restart: %v", err)
	}
	for _, table := range []string{"quota_snapshots", "quota_windows", "usage_records", "provider_health", "controller_state"} {
		var name string
		if err := again.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Errorf("table %s missing after restart: %v", table, err)
		}
	}
}

func TestSettingsAppliedOnEveryConnection(t *testing.T) {
	s, _ := openMigrated(t)

	// Every pooled connection must carry the pragmas, not just the first.
	for i := 0; i < MaxConns; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		defer conn.Close()
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("journal_mode: %v", err)
		}
		if mode != "wal" {
			t.Fatalf("conn %d journal_mode = %q, want wal", i, mode)
		}
		var timeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatalf("busy_timeout: %v", err)
		}
		if timeout != 5000 {
			t.Fatalf("conn %d busy_timeout = %d, want 5000", i, timeout)
		}
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
	if err := s.SaveQuotaSnapshot(ctx, snap); err != nil {
		t.Fatalf("SaveQuotaSnapshot: %v", err)
	}
	s.Close()

	// Restart: the state must survive with its provenance and window values.
	reopened, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(got))
	}
	g := got[0]
	if g.Provider != snap.Provider || g.State != snap.State || g.Source != snap.Source || g.Confidence != snap.Confidence {
		t.Fatalf("got %+v, want %+v", g, snap)
	}
	if g.UnknownReason == nil || *g.UnknownReason != reason {
		t.Fatalf("unknown_reason = %v, want %s", g.UnknownReason, reason)
	}
	if g.Error != snap.Error {
		t.Fatalf("error = %q, want %q", g.Error, snap.Error)
	}
	if !g.ObservedAt.Equal(snap.ObservedAt) {
		t.Fatalf("observed_at = %s, want %s", g.ObservedAt, snap.ObservedAt)
	}
	if len(g.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(g.Windows))
	}
	w := g.Windows[0]
	if w.Window != "5h" || w.Unit != "percent" || *w.Limit != 100 || *w.Used != 40 || *w.Remaining != 60 {
		t.Fatalf("window = %+v", w)
	}
	if w.ResetAt == nil || !w.ResetAt.Equal(reset) {
		t.Fatalf("reset_at = %v, want %s", w.ResetAt, reset)
	}
}

func TestLatestQuotaSnapshotPerProvider(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for i, snap := range []QuotaSnapshot{
		{Provider: "minimax", State: QuotaLow, Source: "a", Confidence: Reported, ObservedAt: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)},
		{Provider: "minimax", State: QuotaAvailable, Source: "a", Confidence: Reported, ObservedAt: time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)},
		{Provider: "command-code", State: QuotaExhausted, Source: "b", Confidence: Reported, ObservedAt: time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)},
	} {
		if err := s.SaveQuotaSnapshot(ctx, snap); err != nil {
			t.Fatalf("SaveQuotaSnapshot #%d: %v", i, err)
		}
	}

	got, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("providers = %d, want 2 (one row each)", len(got))
	}
	byProvider := map[string]QuotaState{}
	for _, g := range got {
		byProvider[g.Provider] = g.State
	}
	if byProvider["minimax"] != QuotaAvailable {
		t.Errorf("minimax = %s, want the newest %s", byProvider["minimax"], QuotaAvailable)
	}
	if byProvider["command-code"] != QuotaExhausted {
		t.Errorf("command-code = %s, want %s", byProvider["command-code"], QuotaExhausted)
	}
}

func TestQuotaWindowValuesStayUnreportedWhenAbsent(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// A provider that reports only "remaining" must not have used/limit invented
	// as zero: unreported is distinct from 0.
	if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "q", Confidence: Reported,
		ObservedAt: time.Now(),
		Windows:    []QuotaWindow{{Window: "5h", Remaining: floatPtr(0)}},
	}); err != nil {
		t.Fatalf("SaveQuotaSnapshot: %v", err)
	}

	got, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	w := got[0].Windows[0]
	if w.Limit != nil || w.Used != nil {
		t.Fatalf("limit/used = %v/%v, want nil (not reported)", w.Limit, w.Used)
	}
	if w.Remaining == nil || *w.Remaining != 0 {
		t.Fatalf("remaining = %v, want a reported 0", w.Remaining)
	}
}

func TestUsageIsSeparateAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	u := UsageRecord{
		Source: "test", RequestID: "req-1", Provider: "minimax", Model: "MiniMax-M3.1-Flash",
		InputTokens: intPtr(10), OutputTokens: intPtr(20),
		Provenance: Estimated, ObservedAt: time.Now(),
	}
	if err := s.SaveUsageRecord(ctx, u); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	// A retried report with the same ID must not double-count tokens.
	if err := s.SaveUsageRecord(ctx, u); err != nil {
		t.Fatalf("duplicate SaveUsageRecord: %v", err)
	}

	var n, tokens int
	if err := s.db.QueryRow("SELECT COUNT(*), SUM(input_tokens) FROM usage_records").Scan(&n, &tokens); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 || tokens != 10 {
		t.Fatalf("rows = %d, input tokens = %d; want 1 row and 10", n, tokens)
	}
	// Usage must never land in the quota tables: the two are separate signals.
	var snapshots int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapshots != 0 {
		t.Fatalf("quota snapshots = %d after a usage write, want 0", snapshots)
	}
}

func TestUsageWithoutRequestIDIsKept(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	u := UsageRecord{Source: "test", Provider: "minimax", Provenance: Estimated, ObservedAt: time.Now()}
	for i := 0; i < 2; i++ {
		if err := s.SaveUsageRecord(ctx, u); err != nil {
			t.Fatalf("SaveUsageRecord #%d: %v", i, err)
		}
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2: without an id duplicates cannot be detected", n)
	}
}

func TestHealthIsSeparateFromQuota(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// A healthy provider proves reachability only; it must not create a quota row
	// or imply AVAILABLE.
	if err := s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Healthy, ObservedAt: time.Now()}); err != nil {
		t.Fatalf("SaveProviderHealth: %v", err)
	}
	health, err := s.LatestProviderHealth(ctx)
	if err != nil {
		t.Fatalf("LatestProviderHealth: %v", err)
	}
	if len(health) != 1 || health[0].State != Healthy {
		t.Fatalf("health = %+v, want one HEALTHY row", health)
	}
	quota, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if len(quota) != 0 {
		t.Fatalf("quota rows = %d after a health write, want 0", len(quota))
	}
}

func TestControllerStateUpserts(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	if _, ok, err := s.ControllerState(ctx, "mode"); err != nil || ok {
		t.Fatalf("ControllerState before write = ok:%v err:%v, want unset", ok, err)
	}
	for _, v := range []string{"shadow", "active"} {
		if err := s.SaveControllerState(ctx, "mode", v, time.Now()); err != nil {
			t.Fatalf("SaveControllerState: %v", err)
		}
	}
	got, ok, err := s.ControllerState(ctx, "mode")
	if err != nil || !ok || got != "active" {
		t.Fatalf("ControllerState = %q ok:%v err:%v, want active", got, ok, err)
	}
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
		if c.err == nil {
			t.Errorf("%s: saved, want an error", c.name)
		}
	}
	// A rejected write must leave nothing behind.
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("quota snapshots = %d after rejected writes, want 0", n)
	}
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
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("schema accepted %s", q)
		}
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
		t.Fatalf("concurrent access: %v", err)
	}

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 100 {
		t.Fatalf("snapshots = %d, want 100", n)
	}
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
		if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: provider, State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: obs,
			Windows: []QuotaWindow{{Window: "5h"}},
		}); err != nil {
			t.Fatalf("SaveQuotaSnapshot: %v", err)
		}
	}
	if err := s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Healthy, ObservedAt: old}); err != nil {
		t.Fatalf("SaveProviderHealth: %v", err)
	}
	if err := s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "minimax", Provenance: Estimated, ObservedAt: old}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}

	deleted, err := s.Prune(ctx, Retention{
		QuotaSnapshots: 24 * time.Hour,
		ProviderHealth: 24 * time.Hour,
		UsageRecords:   24 * time.Hour,
	}, old.Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// minimax: 2 of 2 rows are older than newest -> 1 kept; command-code: its
	// only row is the newest for that provider -> kept. So 1 of 3 goes.
	if deleted["quota_snapshots"] != 1 {
		t.Fatalf("deleted quota_snapshots = %d, want 1", deleted["quota_snapshots"])
	}
	latest, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest snapshots after prune = %d, want one per provider", len(latest))
	}
	// The window rows of the deleted snapshot must go with it (cascade).
	var orphanWindows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM quota_windows w WHERE NOT EXISTS (SELECT 1 FROM quota_snapshots s WHERE s.id = w.snapshot_id)`).Scan(&orphanWindows); err != nil {
		t.Fatalf("orphan windows: %v", err)
	}
	if orphanWindows != 0 {
		t.Fatalf("orphan quota_windows = %d, want 0", orphanWindows)
	}
	// Health keeps its newest row per provider; usage has no such rule.
	if deleted["provider_health"] != 0 {
		t.Fatalf("deleted provider_health = %d, want 0 (the newest per provider is kept)", deleted["provider_health"])
	}
	if deleted["usage_records"] != 1 {
		t.Fatalf("deleted usage_records = %d, want 1", deleted["usage_records"])
	}
}

func TestPruneKeepsEverythingWhenRetentionIsZero(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: old}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	deleted, err := s.Prune(ctx, Retention{}, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing with zero retention", deleted)
	}
}

func TestPruneIsAtomicOnFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// A cancelled context fails the transaction; nothing may be deleted.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := s.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: old}); err != nil {
			t.Fatalf("SaveUsageRecord: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Prune(cancelled, Retention{UsageRecords: time.Hour}, time.Now()); err == nil {
		t.Fatal("Prune with a cancelled context succeeded")
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("usage_records = %d after a failed prune, want 3", n)
	}
}

func TestBackupIsConsistentUnderWAL(t *testing.T) {
	ctx := context.Background()
	s, path := openMigrated(t)

	// Rows still in the WAL are the ones a file copy misses.
	if err := s.SaveControllerState(ctx, "mode", "active", time.Now()); err != nil {
		t.Fatalf("SaveControllerState: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The backup is one file: no sidecars, so no WAL to forget on restore.
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(backup + sidecar); err == nil {
			t.Errorf("backup left %s behind; VACUUM INTO writes a single file", sidecar)
		}
	}
	bdb, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bdb.Close()
	var got string
	if err := bdb.QueryRow("SELECT value FROM controller_state WHERE key='mode'").Scan(&got); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if got != "active" {
		t.Fatalf("backup value = %q, want active: the WAL rows were not captured", got)
	}
	// A backup must not overwrite an existing file.
	if err := s.Backup(ctx, backup); err == nil {
		t.Fatal("Backup overwrote an existing file")
	}
	if path == backup {
		t.Fatal("test bug: backup path equals the live path")
	}
}

func TestConcurrentBackupsToOneTargetKeepTheWinner(t *testing.T) {
	// Review finding: a failed VACUUM INTO used to remove the destination
	// unconditionally, so a racing loser could delete the winner's good backup.
	ctx := context.Background()
	s, _ := openMigrated(t)
	if err := s.SaveControllerState(ctx, "mode", "active", time.Now()); err != nil {
		t.Fatalf("SaveControllerState: %v", err)
	}
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
		if !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "file exists") {
			t.Fatalf("racer %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers succeeded, want exactly 1", winners)
	}
	// The surviving file must be a complete backup, not a deleted or truncated one.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the winning backup is gone: %v", err)
	}
	bdb, err := sql.Open("sqlite", "file:"+target+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bdb.Close()
	var got string
	if err := bdb.QueryRow("SELECT value FROM controller_state WHERE key='mode'").Scan(&got); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if got != "active" {
		t.Fatalf("backup value = %q, want active", got)
	}
	// No private temporary file was left behind.
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") {
			t.Errorf("temporary file %q survived", e.Name())
		}
	}
}

func TestRelativePathWorks(t *testing.T) {
	// The default setting is a relative path ("quota-router.db"), so it must keep
	// working. A url.URL-based DSN rendered it as "file://quota-router.db" — an
	// authority, not a relative path — and the driver could not open anything.
	saved, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(saved)

	for _, path := range []string{"quota-router.db", filepath.Join("nested", "quota-router.db")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		s, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
		if err != nil {
			t.Fatalf("Open(%q): %v", path, err)
		}
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate(%q): %v", path, err)
		}
		if err := s.SaveControllerState(context.Background(), "k", "v", time.Now()); err != nil {
			t.Fatalf("write(%q): %v", path, err)
		}
		s.Close()
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatalf("relative path %q created no file: %v", path, err)
		}
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
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := s.Migrate(context.Background()); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if err := s.SaveControllerState(context.Background(), "k", "v", time.Now()); err != nil {
				t.Fatalf("write: %v", err)
			}
			s.Close()

			// The file must exist under the configured name, and no other file may
			// have been created from the truncated URI.
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not created at %q: %v", name, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			for _, e := range entries {
				base := e.Name()
				if base == name || base == name+"-wal" || base == name+"-shm" || base == name+".lock" {
					continue
				}
				t.Errorf("unexpected file %q: the path was cut at a URI delimiter", base)
			}
			// And the migration lock pointed at the same file, so serialization holds.
			if _, err := os.Stat(path + ".lock"); err != nil {
				t.Errorf("migration lock is not beside the database: %v", err)
			}
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
		if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "minimax", State: state, Source: "s", Confidence: Reported, ObservedAt: at,
		}); err != nil {
			t.Fatalf("SaveQuotaSnapshot: %v", err)
		}
	}
	for _, state := range []HealthState{Healthy, Unavailable} {
		if err := s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: state, ObservedAt: at}); err != nil {
			t.Fatalf("SaveProviderHealth: %v", err)
		}
	}

	for i := 0; i < 5; i++ { // the tie must resolve the same way every time
		snaps, err := s.LatestQuotaSnapshot(ctx)
		if err != nil {
			t.Fatalf("LatestQuotaSnapshot: %v", err)
		}
		if len(snaps) != 1 {
			t.Fatalf("snapshots = %d, want exactly 1 per provider", len(snaps))
		}
		health, err := s.LatestProviderHealth(ctx)
		if err != nil {
			t.Fatalf("LatestProviderHealth: %v", err)
		}
		if len(health) != 1 {
			t.Fatalf("health = %d, want exactly 1 per provider", len(health))
		}
	}

	// Retention must spare exactly the row the readers return, no more.
	before, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if _, err := s.Prune(ctx, Retention{QuotaSnapshots: time.Hour, ProviderHealth: time.Hour}, at.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	after, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot after prune: %v", err)
	}
	if len(after) != 1 || after[0].State != before[0].State {
		t.Fatalf("reader returned %+v before prune and %+v after: they disagree on the tie", before, after)
	}
	var snapshots, health int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM provider_health").Scan(&health); err != nil {
		t.Fatalf("count health: %v", err)
	}
	if snapshots != 1 || health != 1 {
		t.Fatalf("retention kept %d snapshots and %d health rows, want 1 each", snapshots, health)
	}
}

func TestBackupRefusesAndLeavesNoLitter(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	dir := t.TempDir()

	for _, c := range []struct{ name, path string }{
		{"empty path", ""},
		{"missing directory", filepath.Join(dir, "nodir", "x.db")},
	} {
		if err := s.Backup(ctx, c.path); err == nil {
			t.Errorf("%s: Backup succeeded", c.name)
		}
	}

	target := filepath.Join(dir, "ok.db")
	if err := s.Backup(ctx, target); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Refusing an existing target must not damage it either.
	if err := s.Backup(ctx, target); err == nil {
		t.Fatal("Backup overwrote an existing file")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the existing backup was removed: %v", err)
	}
	// No private temporary file may survive either outcome.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") {
			t.Errorf("temporary file %q survived", e.Name())
		}
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.SaveControllerState(ctx, "k", "v", time.Now()); err != nil {
		t.Fatalf("write: %v", err)
	}

	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	for _, p := range []string{path, backup} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := st.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600: the file holds provider observations", filepath.Base(p), got)
		}
	}
	s.Close()

	// An existing database created 0644 by an earlier build is tightened on open,
	// so an upgrade does not leave it exposed.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	again, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("existing database mode = %04o after open, want 0600", got)
	}
}

func TestSubMillisecondObservationsDoNotTie(t *testing.T) {
	// Review finding: millisecond truncation turned distinct observations into a
	// tie, and the id tie-break then let a LATE, OLDER write outrank the newer one.
	ctx := context.Background()
	s, _ := openMigrated(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := base.Add(900 * time.Microsecond)

	// The newer observation is written first, so the older one gets the higher id.
	if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: newer,
	}); err != nil {
		t.Fatalf("newer: %v", err)
	}
	if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaExhausted, Source: "s", Confidence: Reported, ObservedAt: base,
	}); err != nil {
		t.Fatalf("older: %v", err)
	}

	got, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(got))
	}
	if got[0].State != QuotaAvailable {
		t.Fatalf("state = %s, want %s: the late older write outranked the newer observation",
			got[0].State, QuotaAvailable)
	}
	if !got[0].ObservedAt.Equal(newer.Truncate(time.Nanosecond)) {
		t.Fatalf("observed_at = %s, want %s: precision was lost", got[0].ObservedAt, newer)
	}
	// Retention must agree: the same row is the one that survives.
	if _, err := s.Prune(ctx, Retention{QuotaSnapshots: time.Hour}, base.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	after, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot after prune: %v", err)
	}
	if len(after) != 1 || after[0].State != QuotaAvailable {
		t.Fatalf("after prune %+v, want the same AVAILABLE row", after)
	}
}

func TestMemoryPathStillPersists(t *testing.T) {
	// Review finding: a path STARTING with ":" is a SQLite URI form, so a
	// relative ":memory:" opened an in-memory database while the controller
	// believed it had a file — every observation was lost on restart, with no
	// startup error. It has to be a relative path to reproduce: an absolute
	// "/tmp/x/:memory:" is an ordinary file name.
	ctx := context.Background()
	saved, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(saved)

	s, err := Open(Config{Path: ":memory:", BusyTimeout: time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.SaveControllerState(ctx, "k", "v", time.Now()); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.Close()

	// A new process must see the row: that is the whole point of the setting.
	again, err := Open(Config{Path: ":memory:", BusyTimeout: time.Second}, testLog())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	v, ok, err := again.ControllerState(ctx, "k")
	if err != nil {
		t.Fatalf("ControllerState: %v", err)
	}
	if !ok || v != "v" {
		t.Fatal("state did not survive the reopen: the path opened an in-memory database")
	}
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
	if err := s.Backup(ctx, target); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Nothing private may be left behind, and the published file is owner-only.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") {
			t.Errorf("temporary backup %q survived", e.Name())
		}
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %04o, want 0600", got)
	}
}

func TestDirectoryPathKeepsItsMode(t *testing.T) {
	// Review finding: chmodDB could chmod a misconfigured directory path. It must
	// refuse before changing anything.
	dir := filepath.Join(t.TempDir(), "adir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Open(Config{Path: dir, BusyTimeout: time.Second}, testLog()); err == nil {
		t.Fatal("Open succeeded on a directory")
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %04o after a failed Open, want 0700 (unchanged)", got)
	}
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
		if err := s.SaveQuotaSnapshot(ctx, snap); err == nil {
			t.Errorf("quota snapshot stored error %q", e[:40])
		}
		if err := s.SaveProviderHealth(ctx, HealthRecord{
			Provider: "p", State: Unavailable, ObservedAt: time.Now(), Error: e,
		}); err == nil {
			t.Errorf("health stored error %q", e[:40])
		}
	}

	// A short reason that names no credential is still accepted.
	if err := s.SaveProviderHealth(ctx, HealthRecord{
		Provider: "p", State: Degraded, ObservedAt: time.Now(), Error: "http 503 from quota endpoint",
	}); err != nil {
		t.Fatalf("safe description rejected: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestSymlinkedDatabaseSharesOneMigrationLock(t *testing.T) {
	// Review finding: the lock name came from the configured path, so two
	// controllers naming one database through a symlink took different locks and
	// could migrate it concurrently.
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	alias := filepath.Join(dir, "alias.db")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	viaReal, err := Open(Config{Path: real, BusyTimeout: time.Second}, testLog())
	if err != nil {
		t.Fatalf("open real: %v", err)
	}
	defer viaReal.Close()
	viaAlias, err := Open(Config{Path: alias, BusyTimeout: time.Second}, testLog())
	if err != nil {
		t.Fatalf("open alias: %v", err)
	}
	defer viaAlias.Close()

	if viaReal.path != viaAlias.path {
		t.Fatalf("lock paths differ: %q vs %q — the aliases would migrate concurrently",
			viaReal.path, viaAlias.path)
	}
}

func TestDiskFullIsReportedAndLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	// Shrinking the page budget is how SQLite reports SQLITE_FULL without
	// filling a real disk.
	if _, err := s.db.Exec("PRAGMA max_page_count = 4"); err != nil {
		t.Fatalf("max_page_count: %v", err)
	}
	// A snapshot with windows exercises the multi-statement transaction: if the
	// window insert fails, the snapshot row must not survive alone.
	var sawFull bool
	for i := 0; i < 100 && !sawFull; i++ {
		err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "p", State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: time.Now(),
			Windows: []QuotaWindow{{Window: "5h", Limit: floatPtr(1), Used: floatPtr(0)}},
		})
		switch {
		case err == nil:
		case errors.Is(err, ErrFull):
			sawFull = true
		default:
			t.Fatalf("error = %v, want ErrFull", err)
		}
	}
	if !sawFull {
		t.Fatal("never hit SQLITE_FULL; the test could not exercise the failure")
	}
	// No half-written snapshot: the transaction rolled back as a unit.
	var snapshots, windows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots); err != nil {
		t.Fatalf("the database stopped being readable after ErrFull: %v", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_windows").Scan(&windows); err != nil {
		t.Fatalf("count windows: %v", err)
	}
	if snapshots != windows {
		t.Fatalf("%d snapshots but %d windows: a failed write left a partial record", snapshots, windows)
	}
}

func TestBusyIsClassifiedAndLosesNoWrite(t *testing.T) {
	// Another writer holding the lock past busy_timeout is the SQLITE_BUSY path
	// issue #12 names. A zero busy timeout makes the conflict happen at once
	// instead of waiting a second.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(Config{Path: path, BusyTimeout: time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	blocker, err := sql.Open("sqlite", "file:"+path+"?_txlock=exclusive&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	defer blocker.Close()
	held, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// An exclusive transaction with a real write, so the lock is genuinely held.
	if _, err := held.Exec("INSERT INTO controller_state (key, value, updated_at) VALUES ('k','v','2026-01-01T00:00:00.000Z')"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}

	// A second store with no patience: it must report the conflict as ErrBusy,
	// not as an unknown driver error.
	impatient, err := Open(Config{Path: path, BusyTimeout: 1 * time.Millisecond}, testLog())
	if err != nil {
		t.Fatalf("open impatient: %v", err)
	}
	defer impatient.Close()
	err = impatient.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: time.Now()})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want ErrBusy while another writer holds the lock", err)
	}

	if err := held.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// Once the lock is free the write lands, and exactly once.
	if err := impatient.SaveUsageRecord(ctx, UsageRecord{Source: "test", Provider: "p", Provenance: Estimated, ObservedAt: time.Now()}); err != nil {
		t.Fatalf("write after the lock was released: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("usage_records = %d, want 1 (the failed write must not have landed)", n)
	}
}

func TestMigrationLockIsExclusiveAcrossHandles(t *testing.T) {
	// Two controllers racing to migrate must serialize, not run concurrently.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	first, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer first.Close()
	second, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
		t.Fatalf("concurrent Migrate: %v", err)
	}
	// Both ran, the schema exists once, and the version is recorded once.
	v, err := first.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != 3 {
		t.Fatalf("version = %d, want 3", v)
	}
	var applied int
	if err := first.db.QueryRow("SELECT COUNT(*) FROM goose_db_version WHERE version_id = 1 AND is_applied = 1").Scan(&applied); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if applied != 1 {
		t.Fatalf("version 1 recorded %d times, want 1", applied)
	}
}

func TestPruneSparesTheSameRowTheReadersReturn(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A late write with an older observed_at: the row with the highest id is not
	// the newest observation, so pruning by id would delete what routing reads.
	if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "s", Confidence: Reported, ObservedAt: base.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("newest: %v", err)
	}
	if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaLow, Source: "s", Confidence: Reported, ObservedAt: base,
	}); err != nil {
		t.Fatalf("late stale write: %v", err)
	}
	if err := s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Healthy, ObservedAt: base.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("health newest: %v", err)
	}
	if err := s.SaveProviderHealth(ctx, HealthRecord{Provider: "minimax", State: Unavailable, ObservedAt: base}); err != nil {
		t.Fatalf("health late write: %v", err)
	}

	before, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	healthBefore, err := s.LatestProviderHealth(ctx)
	if err != nil {
		t.Fatalf("LatestProviderHealth: %v", err)
	}

	// Everything is old enough to expire, so only the spared rows may survive.
	if _, err := s.Prune(ctx, Retention{QuotaSnapshots: time.Hour, ProviderHealth: time.Hour}, base.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot after prune: %v", err)
	}
	if len(after) != 1 || after[0].State != before[0].State || !after[0].ObservedAt.Equal(before[0].ObservedAt) {
		t.Fatalf("routing read before %+v, after %+v: prune removed the row the reader returns", before, after)
	}
	healthAfter, err := s.LatestProviderHealth(ctx)
	if err != nil {
		t.Fatalf("LatestProviderHealth after prune: %v", err)
	}
	if len(healthAfter) != 1 || healthAfter[0].State != healthBefore[0].State {
		t.Fatalf("health before %+v, after %+v: prune removed the row the reader returns", healthBefore, healthAfter)
	}
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
	if err := s.SaveQuotaSnapshot(ctx, good); err != nil {
		t.Fatalf("SaveQuotaSnapshot: %v", err)
	}

	// Fail the next write at the database: a broken foreign key target cannot be
	// reached through the public API, so shrink the page budget and write large
	// records until SQLite reports it.
	if _, err := s.db.Exec("PRAGMA max_page_count = 8"); err != nil {
		t.Fatalf("max_page_count: %v", err)
	}
	committed, sawFull := 1, false
	for i := 0; i < 100 && !sawFull; i++ {
		err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
			Provider: "minimax", State: QuotaExhausted, Source: "s", Confidence: Reported,
			ObservedAt: time.Now().Add(time.Duration(i+1) * time.Second),
			Error:      strings.Repeat("x", maxError-1), // a full, accepted description
			Windows:    []QuotaWindow{{Window: "5h", Remaining: floatPtr(0)}},
		})
		switch {
		case err == nil:
			committed++
		case errors.Is(err, ErrFull):
			sawFull = true
		default:
			t.Fatalf("error = %v, want ErrFull", err)
		}
	}
	if !sawFull {
		t.Fatal("never hit SQLITE_FULL; the test could not exercise the failure")
	}

	// Exactly the committed writes are visible: a failed write left nothing.
	var snapshots int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM quota_snapshots").Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapshots != committed {
		t.Fatalf("%d snapshots visible, %d writes committed: a failed write left a partial record", snapshots, committed)
	}
	// Every readable snapshot pair is complete: no snapshot row without its window.
	var incomplete int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots q
		WHERE NOT EXISTS (SELECT 1 FROM quota_windows w WHERE w.snapshot_id = q.id)`).Scan(&incomplete); err != nil {
		t.Fatalf("count incomplete: %v", err)
	}
	if incomplete != 0 {
		t.Fatalf("%d snapshots have no windows: a failed write left a partial record", incomplete)
	}
	// And the reader still answers, so a failed write did not break routing state.
	latest, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot after a failed write: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("providers = %d, want 1", len(latest))
	}
	t.Logf("committed %d snapshots before the page budget ran out", committed)
}

func TestOpenFailsClearlyOnAnUnusablePath(t *testing.T) {
	// A directory that does not exist must fail at startup, not on first write.
	missing := filepath.Join(t.TempDir(), "no-such-dir", "test.db")
	if _, err := Open(Config{Path: missing, BusyTimeout: time.Second}, testLog()); err == nil {
		t.Fatal("Open succeeded on a path in a missing directory")
	}
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
	if !errors.Is(err, ErrReadOnly) && !errors.Is(err, ErrIO) && !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want a classified storage error", err)
	}
}

// Last attempt and last success are independent: a failure moves one and must
// leave the other alone, and "never succeeded" stays distinguishable from zero.
func TestPollStateTracksAttemptAndSuccessSeparately(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	attempt, success, err := s.PollState(ctx, "minimax")
	if err != nil {
		t.Fatalf("PollState on an unpolled provider: %v", err)
	}
	if !attempt.IsZero() || !success.IsZero() {
		t.Fatalf("PollState = %s/%s, want zero times before any poll", attempt, success)
	}

	first := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if err := s.SavePollState(ctx, "minimax", first, first); err != nil {
		t.Fatalf("SavePollState: %v", err)
	}
	later := first.Add(time.Minute)
	if err := s.SavePollState(ctx, "minimax", later, time.Time{}); err != nil {
		t.Fatalf("SavePollState after a failure: %v", err)
	}

	attempt, success, err = s.PollState(ctx, "minimax")
	if err != nil {
		t.Fatalf("PollState: %v", err)
	}
	if !attempt.Equal(later) {
		t.Fatalf("last attempt = %s, want the failed attempt's time %s", attempt, later)
	}
	if !success.Equal(first) {
		t.Fatalf("last success = %s, want the earlier success %s: a failure must not erase it", success, first)
	}

	// Providers do not share a row.
	if err := s.SavePollState(ctx, "other", later, time.Time{}); err != nil {
		t.Fatalf("SavePollState: %v", err)
	}
	if a, succ, err := s.PollState(ctx, "other"); err != nil || !a.Equal(later) || !succ.IsZero() {
		t.Fatalf("other = %s/%s (err %v), want its own attempt and no success", a, succ, err)
	}
	if a, _, _ := s.PollState(ctx, "minimax"); !a.Equal(later) {
		t.Fatalf("minimax attempt = %s, want it untouched by another provider", a)
	}

	if err := s.SavePollState(ctx, "", later, later); err == nil {
		t.Fatal("SavePollState accepted an empty provider")
	}
	if err := s.SavePollState(ctx, "minimax", time.Time{}, later); err == nil {
		t.Fatal("SavePollState accepted a zero attempt time")
	}
}
