package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Cleaning up the events must not touch the provider observations: subscription
// quota and health are a different measurement, kept in their own tables.
func TestEventRetentionLeavesProviderObservationsAlone(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	old := time.Now().UTC().Add(-48 * time.Hour)
	limit, used := 100.0, 20.0
	if err := s.SaveQuotaSnapshot(ctx, QuotaSnapshot{
		Provider: "minimax", State: QuotaAvailable, Source: "test", Confidence: Reported,
		ObservedAt: old,
		Windows:    []QuotaWindow{{Window: "5h", Limit: &limit, Used: &used}},
	}); err != nil {
		t.Fatalf("SaveQuotaSnapshot: %v", err)
	}
	if err := s.SaveProviderHealth(ctx, HealthRecord{
		Provider: "minimax", State: Healthy, ObservedAt: old,
	}); err != nil {
		t.Fatalf("SaveProviderHealth: %v", err)
	}
	if err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Outcome: OutcomeSuccess, ObservedAt: old,
	}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}

	deleted, err := s.Prune(ctx, Retention{UsageRecords: 24 * time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted["usage_records"] != 1 {
		t.Fatalf("deleted = %v, want the one stale event", deleted)
	}
	if deleted["quota_snapshots"] != 0 || deleted["provider_health"] != 0 {
		t.Fatalf("deleted = %v, want no provider observation removed", deleted)
	}
	snaps, err := s.LatestQuotaSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshot: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want the provider observation kept", len(snaps))
	}
	health, err := s.LatestProviderHealth(ctx)
	if err != nil {
		t.Fatalf("LatestProviderHealth: %v", err)
	}
	if len(health) != 1 {
		t.Fatalf("health rows = %d, want the provider observation kept", len(health))
	}
}

// The canonical request/outcome event (issue #23) must keep the outcome classes
// apart: a cancelled attempt, an interrupted stream and an ambiguous attempt
// are none of them a success, and none of them is the same fact as another.
func TestEventOutcomesStayDistinguishable(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for _, tc := range []struct {
		name        string
		outcome     Outcome
		errorClass  ErrorClass
		streamState StreamState
	}{
		{"completed response", OutcomeSuccess, "", StreamNotStreamed},
		{"completed stream", OutcomeSuccess, "", StreamComplete},
		{"failed before the first token", OutcomeFailedBeforeFirstToken, ErrorTimeout, StreamNotStreamed},
		{"failed after the first token", OutcomeFailedBeforeFirstToken, ErrorAuth, StreamNotStreamed},
		{"interrupted stream", OutcomePartialStream, ErrorNetwork, StreamInterrupted},
		{"cancelled", OutcomeCancelled, "", StreamNotStreamed},
		{"ambiguous", OutcomeAmbiguous, ErrorTimeout, StreamNotStreamed},
		{"not observed", OutcomeUnknown, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := UsageRecord{
				RequestID: "req-" + tc.name, Provider: "p-" + tc.name, Source: "agent",
				Outcome: tc.outcome, ErrorClass: tc.errorClass, StreamState: tc.streamState,
				ObservedAt: time.Now(),
			}
			if err := s.SaveUsageRecord(ctx, rec); err != nil {
				t.Fatalf("SaveUsageRecord: %v", err)
			}
			got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "p-" + tc.name})
			if err != nil {
				t.Fatalf("QueryUsageRecords: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("events = %d, want 1", len(got))
			}
			if got[0].Outcome != tc.outcome || got[0].ErrorClass != tc.errorClass || got[0].StreamState != tc.streamState {
				t.Fatalf("event = %s/%s/%s, want %s/%s/%s",
					got[0].Outcome, got[0].ErrorClass, got[0].StreamState,
					tc.outcome, tc.errorClass, tc.streamState)
			}
		})
	}
}

// A token count carries the channel that produced it. Two counts from two
// channels stay two counts: nothing merges them and nothing promotes the
// estimate to an observation.
func TestEventTokenProvenanceIsPerCount(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	if err := s.SaveUsageRecord(ctx, UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		InputTokens: intPtr(100), InputProvenance: TokenEstimated,
		OutputTokens: intPtr(20), OutputProvenance: TokenProviderReported,
		Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].InputProvenance != TokenEstimated || got[0].OutputProvenance != TokenProviderReported {
		t.Fatalf("provenance = %q/%q, want estimated/provider_reported",
			got[0].InputProvenance, got[0].OutputProvenance)
	}
	if got[0].InputTokens == nil || *got[0].InputTokens != 100 {
		t.Fatalf("input_tokens = %v, want the reported 100", got[0].InputTokens)
	}
}

// The event id is derived from the attempt identity, so the same attempt always
// has the same id and a retry is a different event. The schema version travels
// with the row, so a reader can tell which shape it sees.
func TestEventIdentityAndVersionAreStable(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-1", Attempt: attempt, Provider: "minimax", Source: "agent",
			Outcome: OutcomeSuccess, ObservedAt: time.Now(),
		}); err != nil {
			t.Fatalf("SaveUsageRecord attempt %d: %v", attempt, err)
		}
	}
	got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2 attempts", len(got))
	}
	if got[0].EventID == "" || got[0].EventID == got[1].EventID {
		t.Fatalf("event ids = %q/%q, want one distinct id per attempt", got[0].EventID, got[1].EventID)
	}
	for _, rec := range got {
		if rec.SchemaVersion != EventSchemaVersion {
			t.Fatalf("schema_version = %d, want %d", rec.SchemaVersion, EventSchemaVersion)
		}
	}
}

// A delayed report is still an observation of its own moment: it is stored,
// and it is read in observation order, not in arrival order. The source clock
// is never reconciled with the server clock.
func TestEventDelayedReportKeepsItsObservationTime(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	now := time.Now().UTC()
	late := now.Add(-2 * time.Hour)
	for _, rec := range []UsageRecord{
		{RequestID: "req-old", Provider: "minimax", Source: "agent", Outcome: OutcomeSuccess,
			ObservedAt: late, ObservedAtReported: true, ReceivedAt: now},
		{RequestID: "req-new", Provider: "minimax", Source: "agent", Outcome: OutcomeSuccess,
			ObservedAt: now, ObservedAtReported: true, ReceivedAt: now},
	} {
		if err := s.SaveUsageRecord(ctx, rec); err != nil {
			t.Fatalf("SaveUsageRecord %s: %v", rec.RequestID, err)
		}
	}
	got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(got) != 2 || got[0].RequestID != "req-new" {
		t.Fatalf("events = %+v, want the newer observation first", got)
	}
	old, err := s.QueryUsageRecords(ctx, UsageQuery{Since: late.Add(-time.Minute), Until: late.Add(time.Minute)})
	if err != nil {
		t.Fatalf("QueryUsageRecords window: %v", err)
	}
	if len(old) != 1 || old[0].RequestID != "req-old" {
		t.Fatalf("window events = %+v, want the delayed report in its own window", old)
	}
	if !old[0].ObservedAt.Equal(late) {
		t.Fatalf("observed_at = %s, want the source clock %s", old[0].ObservedAt, late)
	}
}

// The same attempt reported twice with different provenance is a conflict, not
// a merge: the two channels disagree about the count and the store keeps the
// first one instead of inventing a reconciled value.
func TestEventConflictingProvenanceIsAConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{
		RequestID: "req-1", Attempt: 1, Provider: "minimax", Source: "agent",
		InputTokens: intPtr(10), InputProvenance: TokenProviderReported,
		Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}
	if err := s.SaveUsageRecord(ctx, base); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	conflict := base
	conflict.InputProvenance = TokenEstimated
	if err := s.SaveUsageRecord(ctx, conflict); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("SaveUsageRecord conflict = %v, want ErrUsageConflict", err)
	}
	got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(got) != 1 || got[0].InputProvenance != TokenProviderReported {
		t.Fatalf("events = %+v, want the first observation kept", got)
	}
}

// The event carries bounded values only: a free-text error, a negative
// measurement or an unbounded identifier is refused rather than stored.
func TestEventRejectsUnrepresentableTelemetry(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	for _, tc := range []struct {
		name string
		rec  UsageRecord
	}{
		{"raw provider message as error class", UsageRecord{
			ErrorClass: ErrorClass("provider said: sk-secret-key")}},
		{"unknown error class", UsageRecord{ErrorClass: ErrorClass("teapot")}},
		{"unknown stream state", UsageRecord{StreamState: StreamState("half")}},
		{"unknown route source", UsageRecord{RouteSource: RouteSource("guess")}},
		{"provenance without a count", UsageRecord{InputProvenance: TokenEstimated}},
		{"count without provenance", UsageRecord{InputTokens: intPtr(3)}},
		{"negative latency", UsageRecord{LatencyMS: int64Ptr(-1)}},
		{"negative time to first token", UsageRecord{TTFTMS: int64Ptr(-1)}},
		{"unbounded identifier", UsageRecord{RequestID: strings.Repeat("x", maxKey+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.rec
			if rec.RequestID == "" {
				rec.RequestID = "req-" + tc.name
			}
			rec.Provider = "minimax"
			rec.Source = "agent"
			rec.ObservedAt = time.Now()
			if err := s.SaveUsageRecord(ctx, rec); err == nil {
				t.Fatal("an unrepresentable event was stored")
			}
		})
	}
}

// Retention is bounded twice: the configured age is clamped to the hard upper
// bound, and one run deletes at most a bounded number of rows, so a cleanup can
// never hold the write lock for the whole table.
func TestEventRetentionIsClampedAndBatched(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	now := time.Now().UTC()
	old := now.Add(-380 * 24 * time.Hour) // older than the hard upper bound
	recent := now.Add(-300 * 24 * time.Hour)
	for _, at := range []time.Time{old, recent} {
		if err := s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-" + at.Format("2006-01-02"), Provider: "minimax", Source: "agent",
			Outcome: OutcomeSuccess, ObservedAt: at,
		}); err != nil {
			t.Fatalf("SaveUsageRecord: %v", err)
		}
	}
	// A configured age above the bound is clamped to the bound, so the 380-day
	// row goes even though the configuration asked for 400 days.
	deleted, err := s.Prune(ctx, Retention{UsageRecords: 400 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted["usage_records"] != 1 {
		t.Fatalf("deleted = %d, want the row beyond the hard bound", deleted["usage_records"])
	}
	got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want the row inside the bound to survive", len(got))
	}

	// One run deletes at most pruneBatch*maxPruneBatches rows; the rest waits
	// for the next one.
	restore := pruneBatch
	pruneBatch = 2
	maxBatches := maxPruneBatches
	maxPruneBatches = 1
	t.Cleanup(func() { pruneBatch = restore; maxPruneBatches = maxBatches })

	for i := 0; i < 5; i++ {
		if err := s.SaveUsageRecord(ctx, UsageRecord{
			RequestID: "req-batch-" + string(rune('a'+i)), Provider: "minimax", Source: "agent",
			Outcome: OutcomeSuccess, ObservedAt: now.Add(-48 * time.Hour),
		}); err != nil {
			t.Fatalf("SaveUsageRecord: %v", err)
		}
	}
	deleted, err = s.Prune(ctx, Retention{UsageRecords: 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted["usage_records"] != 2 {
		t.Fatalf("deleted = %d, want one bounded batch of %d", deleted["usage_records"], pruneBatch)
	}
}
