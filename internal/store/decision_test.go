package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decisionRecord builds a decision with the given candidates.
func decisionRecord(now time.Time, candidates ...RoutingDecisionCandidate) RoutingDecisionRecord {
	return RoutingDecisionRecord{
		DecisionID:         "dec-1",
		PolicyVersion:      "routing.v1",
		SessionID:          "sess-1",
		TaskClass:          "coding",
		RequestedAlias:     "mm-general",
		RequestedProvider:  "minimax",
		SelectedAlias:      "cc-coding",
		SelectedProvider:   "commandcode",
		SelectedReasonCode: "selected_by_preference",
		SelectedReason:     "highest configured priority",
		Payload:            `{"selected_alias":"cc-coding"}`,
		Candidates:         candidates,
		CreatedAt:          now,
	}
}

func candidate(alias, provider string, ordinal int) RoutingDecisionCandidate {
	return RoutingDecisionCandidate{
		Ordinal:    ordinal,
		Alias:      alias,
		Provider:   provider,
		Selected:   alias == "cc-coding",
		Eligible:   true,
		ReasonCode: "eligible_not_selected",
	}
}

// A decision and its candidates are written in one transaction: a candidate
// write that fails must leave no decision behind, so history can never contain
// a decision whose candidate set is half written.
func TestDecisionAndCandidatesAreOneTransaction(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Now().UTC()

	// The duplicate ordinal violates the storage invariant the Go validation
	// does not pre-empt.
	rec := decisionRecord(now,
		candidate("cc-coding", "commandcode", 1),
		candidate("cc-fast", "commandcode", 1),
	)
	require.Error(t, s.SaveRoutingDecision(ctx, rec), "a duplicate ordinal must fail the write")

	var decisions, candidates int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decisions").Scan(&decisions), "count decisions")
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decision_candidates").Scan(&candidates), "count candidates")
	assert.Equal(t, 0, decisions, "the failed write left no decision row")
	assert.Equal(t, 0, candidates, "the failed write left no candidate row")
}

// The read path returns the candidates of a decision, in evaluation order.
func TestDecisionReadPathReturnsCandidates(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Now().UTC()

	rec := decisionRecord(now,
		candidate("cc-coding", "commandcode", 1),
		candidate("cc-fast", "commandcode", 2),
		candidate("mm-general", "minimax", 3),
	)
	require.NoError(t, s.SaveRoutingDecision(ctx, rec), "SaveRoutingDecision")

	recs, err := s.LatestRoutingDecisions(ctx, 10)
	require.NoError(t, err, "LatestRoutingDecisions")
	require.Len(t, recs, 1)
	assert.Equal(t, "dec-1", recs[0].DecisionID)
	assert.Equal(t, "coding", recs[0].TaskClass)
	assert.Equal(t, "selected_by_preference", recs[0].SelectedReasonCode)
	assert.Equal(t, "mm-general", recs[0].RequestedAlias)
	require.Len(t, recs[0].Candidates, 3)
	assert.Equal(t, "cc-coding", recs[0].Candidates[0].Alias)
	assert.Equal(t, 3, recs[0].Candidates[2].Ordinal)
	assert.True(t, recs[0].Candidates[0].Selected)
	assert.False(t, recs[0].Candidates[1].Selected)
}

// The candidate rows carry the observations the decision acted on, and a value
// that was not observed stays NULL instead of becoming zero or healthy.
func TestDecisionCandidatesKeepUnobservedValuesNull(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Now().UTC()

	c := candidate("mm-general", "minimax", 1)
	c.QuotaState = "UNKNOWN"
	c.QuotaUnknownReason = "TRANSIENT_FETCH_FAILURE"
	require.NoError(t, s.SaveRoutingDecision(ctx, decisionRecord(now, c)), "SaveRoutingDecision")

	var quotaObserved, quotaReset, healthObserved, cooldown *string
	require.NoError(t, s.db.QueryRow(
		`SELECT quota_observed_at, quota_reset_at, health_observed_at, cooldown_until
		 FROM routing_decision_candidates WHERE alias = 'mm-general'`).
		Scan(&quotaObserved, &quotaReset, &healthObserved, &cooldown), "read")
	assert.Nil(t, quotaObserved, "an unobserved quota time must stay NULL")
	assert.Nil(t, quotaReset)
	assert.Nil(t, healthObserved)
	assert.Nil(t, cooldown)

	var state, reason string
	require.NoError(t, s.db.QueryRow(
		`SELECT quota_state, quota_unknown_reason FROM routing_decision_candidates WHERE alias = 'mm-general'`).
		Scan(&state, &reason), "read")
	assert.Equal(t, "UNKNOWN", state)
	assert.Equal(t, "TRANSIENT_FETCH_FAILURE", reason)
}

// Retention prunes decisions, and the candidates go with them: a candidate row
// must never outlive the decision it belongs to.
func TestDecisionRetentionPrunesCandidatesToo(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Now().UTC()

	old := decisionRecord(now.Add(-48*time.Hour), candidate("cc-coding", "commandcode", 1))
	old.DecisionID = "dec-old"
	require.NoError(t, s.SaveRoutingDecision(ctx, old), "SaveRoutingDecision old")

	fresh := decisionRecord(now, candidate("cc-coding", "commandcode", 1))
	fresh.DecisionID = "dec-new"
	require.NoError(t, s.SaveRoutingDecision(ctx, fresh), "SaveRoutingDecision fresh")

	deleted, err := s.Prune(ctx, Retention{RoutingDecisions: 24 * time.Hour}, now)
	require.NoError(t, err, "Prune")
	assert.Equal(t, int64(1), deleted["routing_decisions"])

	var decisions, candidates int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decisions").Scan(&decisions), "count decisions")
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decision_candidates").Scan(&candidates), "count candidates")
	assert.Equal(t, 1, decisions, "the fresh decision survives")
	assert.Equal(t, 1, candidates, "the pruned decision's candidates went with it")
}

// The history survives a restart: a decision and its candidates are on disk, not
// in memory.
func TestDecisionSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, path := openMigrated(t)
	now := time.Now().UTC()

	require.NoError(t, s.SaveRoutingDecision(ctx, decisionRecord(now, candidate("cc-coding", "commandcode", 1))), "SaveRoutingDecision")
	s.Close()

	again, err := Open(Config{Path: path, BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "reopen")
	defer again.Close()
	require.NoError(t, again.Migrate(ctx), "Migrate after restart")

	recs, err := again.LatestRoutingDecisions(ctx, 10)
	require.NoError(t, err, "LatestRoutingDecisions")
	require.Len(t, recs, 1)
	require.Len(t, recs[0].Candidates, 1)
	assert.Equal(t, "cc-coding", recs[0].Candidates[0].Alias)
}

// The redaction boundary: a candidate reason that looks like a credential is
// refused, and nothing is written.
func TestDecisionRefusesCredentialLookingReason(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)
	now := time.Now().UTC()

	c := candidate("cc-coding", "commandcode", 1)
	c.Reason = "provider rejected Authorization: Bearer sk-secret"
	require.Error(t, s.SaveRoutingDecision(ctx, decisionRecord(now, c)), "a credential-looking reason must be refused")

	var decisions, candidates int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decisions").Scan(&decisions), "count decisions")
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decision_candidates").Scan(&candidates), "count candidates")
	assert.Equal(t, 0, decisions)
	assert.Equal(t, 0, candidates)
}

// The outcome of an attempt is recorded next to the decision it was routed by,
// with its latency and its normalized error class.
func TestUsageOutcomeCarriesLatencyAndErrorClass(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Attempt: 1, Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeTimeout, DecisionID: "dec-1",
		LatencyMS: intPtr(4200), ErrorClass: ErrorClassTimeout, ObservedAt: time.Now(),
	}), "SaveUsageRecord")

	var outcome, errClass string
	var latency *int64
	require.NoError(t, s.db.QueryRow(
		`SELECT outcome, latency_ms, error_class FROM usage_records WHERE request_id = 'req-1'`).
		Scan(&outcome, &latency, &errClass), "read")
	assert.Equal(t, "TIMEOUT", outcome)
	require.NotNil(t, latency)
	assert.Equal(t, int64(4200), *latency)
	assert.Equal(t, "TIMEOUT", errClass)
}

// A latency nobody measured stays NULL: a missing latency is not zero.
func TestUsageMissingLatencyStaysNull(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	require.NoError(t, s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}), "SaveUsageRecord")

	var latency *int64
	var errClass *string
	require.NoError(t, s.db.QueryRow(
		`SELECT latency_ms, error_class FROM usage_records WHERE request_id = 'req-1'`).
		Scan(&latency, &errClass), "read")
	assert.Nil(t, latency, "want NULL")
	assert.Nil(t, errClass, "want NULL for an attempt that did not fail")
}

// A redelivered outcome with identical content is a no-op; the same event id
// reused for different content is a conflict.
func TestUsageConflictOnChangedLatencyOrErrorClass(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{RequestID: "req-1", Attempt: 1, Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeTimeout, LatencyMS: intPtr(100), ErrorClass: ErrorClassTimeout, ObservedAt: time.Now()}
	require.NoError(t, s.SaveUsageRecord(ctx, base), "SaveUsageRecord")

	changedLatency := base
	changedLatency.LatencyMS = intPtr(200)
	assert.ErrorIs(t, s.SaveUsageRecord(ctx, changedLatency), ErrUsageConflict)

	changedClass := base
	changedClass.ErrorClass = ErrorClassRateLimited
	assert.ErrorIs(t, s.SaveUsageRecord(ctx, changedClass), ErrUsageConflict)

	require.NoError(t, s.SaveUsageRecord(ctx, base), "identical redelivery")
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count")
	assert.Equal(t, 1, n, "an identical redelivery stores nothing")
}

// The outcome enum is closed: a class outside it is refused instead of stored.
func TestUsageRejectsUnknownErrorClass(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Provenance: Reported, Outcome: OutcomeFailedBeforeFirstToken, ErrorClass: "MAYBE", ObservedAt: time.Now(),
	})
	assert.Error(t, err, "an unknown error class must be refused")
}

// The migration upgrades a database that already holds #12 data: the existing
// usage rows and decisions survive, and the new columns and table appear
// empty rather than fabricated.
func TestDecisionMigrationUpgradesExistingData(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "old.db"), BusyTimeout: 5 * time.Second}, testLog())
	require.NoError(t, err, "Open")
	defer s.Close()

	upTo(t, ctx, s,
		"0001_persistence.sql",
		"0002_poll_state.sql",
		"0003_usage_attempts.sql",
		"0004_routing_decisions.sql",
		"0005_provider_cooldown.sql",
		"0006_session_affinity.sql",
	)
	_, err = s.db.Exec(
		`INSERT INTO usage_records (request_id, attempt, provider, provenance, source, outcome, observed_at)
		 VALUES ('old-1', 1, 'minimax', 'reported', 'agent', 'SUCCESS', '2026-01-01T00:00:00.000000000Z')`)
	require.NoError(t, err, "insert pre-migration usage row")
	_, err = s.db.Exec(
		`INSERT INTO routing_decisions (decision_id, policy_version, session_id, selected_alias, selected_provider, payload, created_at)
		 VALUES ('dec-old', 'routing.v1', 's1', 'cc-coding', 'commandcode', '{}', '2026-01-01T00:00:00.000000000Z')`)
	require.NoError(t, err, "insert pre-migration decision row")

	upTo(t, ctx, s, "0007_routing_decision_candidates.sql")

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&n), "count usage rows")
	assert.Equal(t, 1, n, "the existing usage row survived the upgrade")

	var latency *int64
	var errorClass *string
	require.NoError(t, s.db.QueryRow(
		`SELECT latency_ms, error_class FROM usage_records WHERE request_id = 'old-1'`).Scan(&latency, &errorClass), "read")
	assert.Nil(t, latency, "an upgraded row has no measured latency")
	assert.Nil(t, errorClass, "an upgraded row has no error class")

	var candidates int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM routing_decision_candidates").Scan(&candidates), "count candidates")
	assert.Equal(t, 0, candidates, "an old decision has no candidate rows")

	// The new decision columns exist and are NULL for the old row.
	var code, taskClass *string
	require.NoError(t, s.db.QueryRow(
		`SELECT selected_reason_code, task_class FROM routing_decisions WHERE decision_id = 'dec-old'`).Scan(&code, &taskClass), "read")
	assert.Nil(t, code)
	assert.Nil(t, taskClass)
}
