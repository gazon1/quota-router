package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// A retry is a separate attempt, never a duplicate of the usage it replaced,
// and a repeated report of one attempt is not counted twice.
func TestUsageRetriesAreSeparateAttempts(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	attempts := []UsageRecord{
		{RequestID: "req-1", Attempt: 1, Provider: "minimax", Source: "agent",
			Provenance: Reported, Outcome: OutcomeFailedBeforeFirstToken, ObservedAt: time.Now()},
		{RequestID: "req-1", Attempt: 2, Provider: "minimax", Source: "agent",
			InputTokens: intPtr(11), OutputTokens: intPtr(7),
			Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now()},
	}
	for _, u := range attempts {
		if err := s.SaveUsageRecord(ctx, u); err != nil {
			t.Fatalf("SaveUsageRecord attempt %d: %v", u.Attempt, err)
		}
		// A repeated report of the same attempt must store nothing.
		if err := s.SaveUsageRecord(ctx, u); err != nil {
			t.Fatalf("duplicate SaveUsageRecord attempt %d: %v", u.Attempt, err)
		}
	}

	var n, tokens int
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(output_tokens), 0) FROM usage_records").Scan(&n, &tokens); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 || tokens != 7 {
		t.Fatalf("rows = %d, output tokens = %d; want 2 attempts and 7", n, tokens)
	}
	// The retry must not be stored as a second copy of the successful usage.
	var failed int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM usage_records WHERE request_id = 'req-1' AND outcome = 'FAILED_BEFORE_FIRST_TOKEN'`).Scan(&failed); err != nil {
		t.Fatalf("count failed attempts: %v", err)
	}
	if failed != 1 {
		t.Fatalf("failed attempts = %d, want the one retry that never produced output", failed)
	}
}

// Missing token counts stay NULL: a report that supplies nothing must not read
// as zero tokens later.
func TestUsageMissingTokensStayNull(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	if err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-nil", Provider: "minimax", Source: "agent",
		Provenance: Estimated, Outcome: OutcomeCancelled, ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	var in, out *int64
	if err := s.db.QueryRow("SELECT input_tokens, output_tokens FROM usage_records WHERE request_id = 'req-nil'").Scan(&in, &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if in != nil || out != nil {
		t.Fatalf("tokens = %v/%v, want NULL for both", in, out)
	}
}

// Provenance is stored as reported or estimated, never promoted: a locally
// estimated count must not become a provider-reported one.
func TestUsageProvenanceIsNotPromoted(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for _, p := range []Confidence{Reported, Estimated} {
		if err := s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-" + string(p), Provider: "minimax", Source: "agent",
			InputTokens: intPtr(5), Provenance: p, Outcome: OutcomeSuccess, ObservedAt: time.Now(),
		}); err != nil {
			t.Fatalf("SaveUsageRecord %s: %v", p, err)
		}
	}
	rows, err := s.db.Query("SELECT request_id, provenance FROM usage_records ORDER BY request_id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, prov string
		if err := rows.Scan(&id, &prov); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = prov
	}
	if got["req-reported"] != "reported" || got["req-estimated"] != "estimated" {
		t.Fatalf("provenance = %v, want each row to keep its own", got)
	}
}

// A redelivered event with identical content is a no-op; the same event id
// reused for different content is a conflict, not a silent overwrite.
func TestUsageConflictingEventReuseIsConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{RequestID: "req-1", Attempt: 1, Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now()}
	if err := s.SaveUsageRecord(ctx, base); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}

	conflict := base
	conflict.InputTokens = intPtr(999)
	if err := s.SaveUsageRecord(ctx, conflict); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("SaveUsageRecord conflict = %v, want ErrUsageConflict", err)
	}

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1: a conflicting reuse must not overwrite", n)
	}

	if err := s.SaveUsageRecord(ctx, base); err != nil {
		t.Fatalf("identical redelivery: %v", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 after an identical redelivery", n)
	}
}

// A redelivered event whose reporter-supplied observation timestamp differs is
// a conflicting reuse, not an identical redelivery: the stored attempt must not
// accept a different observed_at as the same content.
func TestUsageConflictingEventReuseWithDifferentObservedAt(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{RequestID: "req-2", Attempt: 1, Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now(), ObservedAtReported: true}
	if err := s.SaveUsageRecord(ctx, base); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}

	conflict := base
	conflict.ObservedAt = base.ObservedAt.Add(time.Hour)
	if err := s.SaveUsageRecord(ctx, conflict); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("SaveUsageRecord conflicting observed_at = %v, want ErrUsageConflict", err)
	}

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1: a conflicting observed_at must not overwrite", n)
	}
}

// Partial streams and cancellations are outcomes, not failures of the write,
// and an attempt can carry the routing decision it was routed by.
func TestUsageRecordsOutcomeAndDecision(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	if err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "litellm.callback",
		InputTokens: intPtr(30), OutputTokens: intPtr(4),
		Provenance: Reported, Outcome: OutcomePartialStream, DecisionID: "dec-42", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	var outcome, decision string
	if err := s.db.QueryRow("SELECT outcome, decision_id FROM usage_records WHERE request_id = 'req-1'").Scan(&outcome, &decision); err != nil {
		t.Fatalf("read: %v", err)
	}
	if outcome != "PARTIAL_STREAM" || decision != "dec-42" {
		t.Fatalf("outcome/decision = %q/%q, want PARTIAL_STREAM/dec-42", outcome, decision)
	}
}

// An attempt with no routing decision keeps that as unknown rather than
// inventing an id.
func TestUsageDecisionStaysNullWhenUnreported(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	if err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	var decision *string
	if err := s.db.QueryRow("SELECT decision_id FROM usage_records WHERE request_id = 'req-1'").Scan(&decision); err != nil {
		t.Fatalf("read: %v", err)
	}
	if decision != nil {
		t.Fatalf("decision_id = %q, want NULL", *decision)
	}
}

// The store rejects what the schema cannot represent, so a broken reporter gets
// a named field instead of a constraint failure or a fabricated value.
func TestUsageRejectsUnrepresentableValues(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	now := time.Now()
	for _, tc := range []struct {
		name string
		rec  UsageRecord
	}{
		{"negative attempt", UsageRecord{Attempt: -1, Provider: "p", Source: "s", Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: now}},
		{"unknown outcome", UsageRecord{Provider: "p", Source: "s", Provenance: Reported, Outcome: "TIMEOUT", ObservedAt: now}},
		{"missing source", UsageRecord{Provider: "p", Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: now}},
		{"negative tokens", UsageRecord{Provider: "p", Source: "s", InputTokens: intPtr(-1), Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: now}},
		{"oversized request id", UsageRecord{RequestID: string(make([]byte, maxKey+1)), Provider: "p", Source: "s", Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: now}},
	} {
		if err := s.SaveUsageRecord(ctx, tc.rec); err == nil {
			t.Errorf("%s: saved, want an error", tc.name)
		}
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("usage rows = %d after rejected writes, want 0", n)
	}
}

// An attempt of zero and an empty outcome are the two defaults the store sets,
// so a caller that omits them does not have to know the enum.
func TestUsageDefaultsAttemptOneAndUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	if err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "p", Source: "s", Provenance: Reported, ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	var attempt int
	var outcome string
	if err := s.db.QueryRow("SELECT attempt, outcome FROM usage_records WHERE request_id = 'req-1'").Scan(&attempt, &outcome); err != nil {
		t.Fatalf("read: %v", err)
	}
	if attempt != 1 || outcome != "UNKNOWN" {
		t.Fatalf("attempt/outcome = %d/%q, want 1/UNKNOWN", attempt, outcome)
	}
}

// TestUsageMigrationKeepsExistingRows covers the upgrade path: a version-2
// database has usage rows with no attempt, source or outcome, and migration
// 0003 must keep them as first attempts rather than drop them.
func TestUsageMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "old.db"), BusyTimeout: 5 * time.Second}, testLog())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Bring the database to version 1 only, the shape that shipped before this
	// change, and write a row in it.
	upTo(t, ctx, s, "0001_persistence.sql")
	if _, err := s.db.Exec(
		`INSERT INTO usage_records (request_id, provider, model, input_tokens, output_tokens, provenance, observed_at)
		 VALUES ('old-1', 'minimax', 'MiniMax-M2.7', 3, 4, 'reported', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	upTo(t, ctx, s, "0001_persistence.sql", "0002_poll_state.sql", "0003_usage_attempts.sql")

	var attempt int
	var source, outcome string
	var in, out int
	if err := s.db.QueryRow(
		`SELECT attempt, source, outcome, input_tokens, output_tokens FROM usage_records WHERE request_id = 'old-1'`).
		Scan(&attempt, &source, &outcome, &in, &out); err != nil {
		t.Fatalf("the pre-migration row is gone: %v", err)
	}
	if attempt != 1 || source != "legacy" || outcome != "UNKNOWN" {
		t.Fatalf("migrated row = attempt %d source %q outcome %q, want 1/legacy/UNKNOWN", attempt, source, outcome)
	}
	if in != 3 || out != 4 {
		t.Fatalf("tokens = %d/%d, want the reported 3/4 kept", in, out)
	}
}

// A downgrade must not lose the usage it can represent: the pre-attempt shape
// keeps the first attempt of each request and no invented columns.
func TestUsageMigrationDownKeepsFirstAttempts(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-1", Attempt: attempt, Provider: "p", Source: "s",
			Provenance: Reported, ObservedAt: time.Now(),
		}); err != nil {
			t.Fatalf("SaveUsageRecord attempt %d: %v", attempt, err)
		}
	}
	dir := t.TempDir()
	for _, f := range []string{
		"0001_persistence.sql",
		"0002_poll_state.sql",
		"0003_usage_attempts.sql",
		"0004_routing_decisions.sql",
		"0005_provider_cooldown.sql",
	} {
		apply(t, dir, f, readMigration(t, f))
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, os.DirFS(dir),
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 2); err != nil {
		t.Fatalf("DownTo: %v", err)
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&rows); err != nil {
		t.Fatalf("read downgraded table: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d after the downgrade, want only the first attempt", rows)
	}
	var old int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('usage_records') WHERE name IN ('attempt', 'source', 'outcome', 'decision_id')`).Scan(&old); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if old != 0 {
		t.Fatalf("%d attempt-era columns survived the downgrade, want none", old)
	}
}

// upTo applies the named embedded migrations, in order, to a database that has
// none of them yet. It is how a test reproduces an older on-disk version.
func upTo(t *testing.T, ctx context.Context, s *Store, files ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		apply(t, dir, f, readMigration(t, f))
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, os.DirFS(dir),
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply %v: %v", files, err)
	}
}
