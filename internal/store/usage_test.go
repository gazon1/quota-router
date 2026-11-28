package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		require.NoError(t, s.SaveUsageRecord(ctx, u), "SaveUsageRecord attempt %d", u.Attempt)
		// A repeated report of the same attempt must store nothing.
		require.NoError(t, s.SaveUsageRecord(ctx, u), "duplicate SaveUsageRecord attempt %d", u.Attempt)
	}

	var n, tokens int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(output_tokens), 0) FROM usage_records").Scan(&n, &tokens), "count")
	assert.Equal(t, 2, n, "want 2 attempts")
	assert.Equal(t, 7, tokens, "want the output tokens of the successful attempt")
	// The retry must not be stored as a second copy of the successful usage.
	var failed int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM usage_records WHERE request_id = 'req-1' AND outcome = 'FAILED_BEFORE_FIRST_TOKEN'`).Scan(&failed),
		"count failed attempts")
	assert.Equal(t, 1, failed, "want the one retry that never produced output")
}

// Missing token counts stay NULL: a report that supplies nothing must not read
// as zero tokens later.
func TestUsageMissingTokensStayNull(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-nil", Provider: "minimax", Source: "agent",
		Provenance: Estimated, Outcome: OutcomeCancelled, ObservedAt: time.Now(),
	}), "SaveUsageRecord")
	var in, out *int64
	require.NoError(t, s.db.QueryRow("SELECT input_tokens, output_tokens FROM usage_records WHERE request_id = 'req-nil'").Scan(&in, &out),
		"read")
	assert.Nil(t, in, "want NULL for the input tokens")
	assert.Nil(t, out, "want NULL for the output tokens")
}

// Provenance is stored as reported or estimated, never promoted: a locally
// estimated count must not become a provider-reported one.
func TestUsageProvenanceIsNotPromoted(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for _, p := range []Confidence{Reported, Estimated} {
		require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-" + string(p), Provider: "minimax", Source: "agent",
			InputTokens: intPtr(5), Provenance: p, Outcome: OutcomeSuccess, ObservedAt: time.Now(),
		}), "SaveUsageRecord %s", p)
	}
	rows, err := s.db.Query("SELECT request_id, provenance FROM usage_records ORDER BY request_id")
	require.NoError(t, err, "query")
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, prov string
		require.NoError(t, rows.Scan(&id, &prov), "scan")
		got[id] = prov
	}
	assert.Equal(t, "reported", got["req-reported"], "each row keeps its own provenance")
	assert.Equal(t, "estimated", got["req-estimated"], "each row keeps its own provenance")
}

// A redelivered event with identical content is a no-op; the same event id
// reused for different content is a conflict, not a silent overwrite.
func TestUsageConflictingEventReuseIsConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{RequestID: "req-1", Attempt: 1, Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now()}
	require.NoError(t, s.SaveUsageRecord(ctx, base), "SaveUsageRecord")

	conflict := base
	conflict.InputTokens = intPtr(999)
	err := s.SaveUsageRecord(ctx, conflict)
	assert.ErrorIs(t, err, ErrUsageConflict)

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 1, n, "a conflicting reuse must not overwrite")

	require.NoError(t, s.SaveUsageRecord(ctx, base), "identical redelivery")
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 1, n, "an identical redelivery stores nothing")
}

// A redelivered event whose reporter-supplied observation timestamp differs is
// a conflicting reuse, not an identical redelivery: the stored attempt must not
// accept a different observed_at as the same content.
func TestUsageConflictingEventReuseWithDifferentObservedAt(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{RequestID: "req-2", Attempt: 1, Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now(), ObservedAtReported: true}
	require.NoError(t, s.SaveUsageRecord(ctx, base), "SaveUsageRecord")

	conflict := base
	conflict.ObservedAt = base.ObservedAt.Add(time.Hour)
	err := s.SaveUsageRecord(ctx, conflict)
	assert.ErrorIs(t, err, ErrUsageConflict)

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 1, n, "a conflicting observed_at must not overwrite")
}

// Partial streams and cancellations are outcomes, not failures of the write,
// and an attempt can carry the routing decision it was routed by.
func TestUsageRecordsOutcomeAndDecision(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "litellm.callback",
		InputTokens: intPtr(30), OutputTokens: intPtr(4),
		Provenance: Reported, Outcome: OutcomePartialStream, DecisionID: "dec-42", ObservedAt: time.Now(),
	}), "SaveUsageRecord")
	var outcome, decision string
	require.NoError(t, s.db.QueryRow("SELECT outcome, decision_id FROM usage_records WHERE request_id = 'req-1'").Scan(&outcome, &decision),
		"read")
	assert.Equal(t, "PARTIAL_STREAM", outcome)
	assert.Equal(t, "dec-42", decision)
}

// An attempt with no routing decision keeps that as unknown rather than
// inventing an id.
func TestUsageDecisionStaysNullWhenUnreported(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}), "SaveUsageRecord")
	var decision *string
	require.NoError(t, s.db.QueryRow("SELECT decision_id FROM usage_records WHERE request_id = 'req-1'").Scan(&decision), "read")
	assert.Nil(t, decision, "want NULL")
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
		assert.Error(t, s.SaveUsageRecord(ctx, tc.rec), "%s: saved, want an error", tc.name)
	}
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 0, n, "no usage rows after rejected writes")
}

// An attempt of zero and an empty outcome are the two defaults the store sets,
// so a caller that omits them does not have to know the enum.
func TestUsageDefaultsAttemptOneAndUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "p", Source: "s", Provenance: Reported, ObservedAt: time.Now(),
	}), "SaveUsageRecord")
	var attempt int
	var outcome string
	require.NoError(t, s.db.QueryRow("SELECT attempt, outcome FROM usage_records WHERE request_id = 'req-1'").Scan(&attempt, &outcome),
		"read")
	assert.Equal(t, 1, attempt)
	assert.Equal(t, "UNKNOWN", outcome)
}

// TestUsageMigrationKeepsExistingRows covers the upgrade path: a version-2
// database has usage rows with no attempt, source or outcome, and migration
// 0003 must keep them as first attempts rather than drop them.
func TestUsageMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "old.db"), BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "Open")
	defer s.Close()

	// Bring the database to version 1 only, the shape that shipped before this
	// change, and write a row in it.
	upTo(t, ctx, s, "0001_persistence.sql")
	_, err = s.db.Exec(
		`INSERT INTO usage_records (request_id, provider, model, input_tokens, output_tokens, provenance, observed_at)
		 VALUES ('old-1', 'minimax', 'MiniMax-M2.7', 3, 4, 'reported', '2026-01-01T00:00:00.000000000Z')`)
	require.NoError(t, err, "insert pre-migration row")

	upTo(t, ctx, s, "0001_persistence.sql", "0002_poll_state.sql", "0003_usage_attempts.sql")

	var attempt int
	var source, outcome string
	var in, out int
	require.NoError(t, s.db.QueryRow(
		`SELECT attempt, source, outcome, input_tokens, output_tokens FROM usage_records WHERE request_id = 'old-1'`).
		Scan(&attempt, &source, &outcome, &in, &out), "the pre-migration row is gone")
	assert.Equal(t, 1, attempt)
	assert.Equal(t, "legacy", source)
	assert.Equal(t, "UNKNOWN", outcome)
	assert.Equal(t, 3, in, "want the reported input tokens kept")
	assert.Equal(t, 4, out, "want the reported output tokens kept")
}

// A downgrade must not lose the usage it can represent: the pre-attempt shape
// keeps the first attempt of each request and no invented columns.
func TestUsageMigrationDownKeepsFirstAttempts(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for attempt := 1; attempt <= 2; attempt++ {
		require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-1", Attempt: attempt, Provider: "p", Source: "s",
			Provenance: Reported, ObservedAt: time.Now(),
		}), "SaveUsageRecord attempt %d", attempt)
	}
	dir := t.TempDir()
	for _, f := range []string{
		"0001_persistence.sql",
		"0002_poll_state.sql",
		"0003_usage_attempts.sql",
		"0004_routing_decisions.sql",
		"0005_provider_cooldown.sql",
		"0006_session_affinity.sql",
	} {
		apply(t, dir, f, readMigration(t, f))
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, os.DirFS(dir),
		goose.WithDisableGlobalRegistry(true))
	require.NoError(t, err, "NewProvider")
	_, err = provider.DownTo(ctx, 2)
	require.NoError(t, err, "DownTo")
	var rows int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&rows), "read downgraded table")
	assert.Equal(t, 1, rows, "after the downgrade only the first attempt remains")
	var old int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('usage_records') WHERE name IN ('attempt', 'source', 'outcome', 'decision_id')`).Scan(&old),
		"column check")
	assert.Equal(t, 0, old, "no attempt-era columns may survive the downgrade")
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
	require.NoError(t, err, "NewProvider")
	_, err = provider.Up(ctx)
	require.NoError(t, err, "apply %v", files)
}
