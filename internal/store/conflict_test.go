package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Two sources that disagree about one attempt must not leave only one of the
// two reports behind: the stored event keeps the first observation, and the
// disagreement itself is kept so a consumer can see that it happened.
func TestConflictingReportIsKeptAsAConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{
		RequestID: "req-1", Attempt: 1, Provider: "minimax", Model: "mm-fast", Source: "agent",
		InputTokens: intPtr(100), InputProvenance: TokenProxyReported,
		OutputTokens: intPtr(20), OutputProvenance: TokenProxyReported,
		Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}
	if err := s.SaveUsageRecord(ctx, base); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	conflict := base
	conflict.OutputTokens = intPtr(25)
	if err := s.SaveUsageRecord(ctx, conflict); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("SaveUsageRecord conflict = %v, want ErrUsageConflict", err)
	}

	// The stored observation is the first one: a conflict never overwrites it.
	got, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].OutputTokens == nil || *got[0].OutputTokens != 20 {
		t.Fatalf("output_tokens = %v, want the first observation 20", got[0].OutputTokens)
	}

	// The conflict is queryable: both sides of the disagreement, with the
	// channel that produced the count.
	conflicts, err := s.QueryUsageConflicts(ctx, UsageConflictQuery{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("QueryUsageConflicts: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want the one disagreeing field", conflicts)
	}
	c := conflicts[0]
	if c.Field != "output_tokens" {
		t.Fatalf("field = %q, want output_tokens", c.Field)
	}
	if c.OriginalValue != "20" || c.IncomingValue != "25" {
		t.Fatalf("values = %q/%q, want 20/25", c.OriginalValue, c.IncomingValue)
	}
	if c.OriginalProvenance != TokenProxyReported || c.IncomingProvenance != TokenProxyReported {
		t.Fatalf("provenance = %q/%q, want proxy_reported on both sides",
			c.OriginalProvenance, c.IncomingProvenance)
	}
	if c.Attempt != 1 || c.Source != "agent" {
		t.Fatalf("conflict identity = %d/%q, want attempt 1 from agent", c.Attempt, c.Source)
	}
}

// A redelivery of the same event is not a conflict: it changes nothing and
// records no disagreement.
func TestIdenticalRetransmissionRecordsNoConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	rec := UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		InputTokens: intPtr(100), InputProvenance: TokenProxyReported,
		Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}
	for i := 0; i < 3; i++ {
		if err := s.SaveUsageRecord(ctx, rec); err != nil {
			t.Fatalf("SaveUsageRecord %d: %v", i, err)
		}
	}
	conflicts, err := s.QueryUsageConflicts(ctx, UsageConflictQuery{})
	if err != nil {
		t.Fatalf("QueryUsageConflicts: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none for an identical redelivery", conflicts)
	}
	events, err := s.QueryUsageRecords(ctx, UsageQuery{Provider: "minimax"})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

// The same conflict reported twice is one conflict: the record of a
// disagreement must not grow with every redelivery of it.
func TestRepeatedConflictIsRecordedOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}
	if err := s.SaveUsageRecord(ctx, base); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	conflict := base
	conflict.Outcome = OutcomeCancelled
	for i := 0; i < 3; i++ {
		if err := s.SaveUsageRecord(ctx, conflict); !errors.Is(err, ErrUsageConflict) {
			t.Fatalf("SaveUsageRecord conflict %d = %v, want ErrUsageConflict", i, err)
		}
	}
	conflicts, err := s.QueryUsageConflicts(ctx, UsageConflictQuery{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("QueryUsageConflicts: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one row for one disagreement", conflicts)
	}
	if conflicts[0].Field != "outcome" ||
		conflicts[0].OriginalValue != string(OutcomeSuccess) ||
		conflicts[0].IncomingValue != string(OutcomeCancelled) {
		t.Fatalf("conflict = %+v, want the outcome disagreement", conflicts[0])
	}
}

// A conflict keeps the disagreement about the provenance of a count too: the
// two channels are the fact, not the count.
func TestConflictingProvenanceIsRecorded(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	base := UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
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
	conflicts, err := s.QueryUsageConflicts(ctx, UsageConflictQuery{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("QueryUsageConflicts: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one row", conflicts)
	}
	if conflicts[0].Field != "input_provenance" ||
		conflicts[0].OriginalValue != string(TokenProviderReported) ||
		conflicts[0].IncomingValue != string(TokenEstimated) {
		t.Fatalf("conflict = %+v, want the provenance disagreement", conflicts[0])
	}
}

// The task class is a bounded vocabulary, and the store enforces it: a direct
// write cannot store a class the routing contract does not know, and the
// schema refuses one too.
func TestTaskClassIsConstrainedInTheStore(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	rec := UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		TaskClass: "not-a-class", Outcome: OutcomeSuccess, ObservedAt: time.Now(),
	}
	if err := s.SaveUsageRecord(ctx, rec); err == nil {
		t.Fatal("an unknown task class was stored")
	}

	// The schema refuses it as well, so a row that skipped the store's
	// validation cannot hold one either.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (request_id, attempt, event_id, schema_version, provider,
		    task_class, source, outcome, observed_at, observed_at_reported, received_at)
		 VALUES ('req-2', 1, 'e2', 1, 'minimax', 'not-a-class', 'agent', 'SUCCESS', ?, 0, ?)`,
		ts(time.Now()), ts(time.Now())); err == nil {
		t.Fatal("the schema accepted an unknown task class")
	}

	// The routing decision carries the same vocabulary.
	decision := RoutingDecisionRecord{
		DecisionID: "dec-1", PolicyVersion: "routing.v1", SessionID: "session-1",
		TaskClass: "not-a-class", Payload: `{}`, CreatedAt: time.Now(),
	}
	if err := s.SaveRoutingDecision(ctx, decision); err == nil {
		t.Fatal("a routing decision with an unknown task class was stored")
	}
	decision.TaskClass = "coding"
	if err := s.SaveRoutingDecision(ctx, decision); err != nil {
		t.Fatalf("SaveRoutingDecision: %v", err)
	}
}

// The conflicts belong to the events: cleanup removes them with the events
// they are about.
func TestConflictsArePrunedWithTheEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openMigrated(t)

	old := time.Now().UTC().Add(-48 * time.Hour)
	rec := UsageRecord{
		RequestID: "req-1", Provider: "minimax", Source: "agent",
		Outcome: OutcomeSuccess, ObservedAt: old,
	}
	if err := s.SaveUsageRecord(ctx, rec); err != nil {
		t.Fatalf("SaveUsageRecord: %v", err)
	}
	conflict := rec
	conflict.Outcome = OutcomeCancelled
	if err := s.SaveUsageRecord(ctx, conflict); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("SaveUsageRecord conflict = %v, want ErrUsageConflict", err)
	}
	deleted, err := s.Prune(ctx, Retention{UsageRecords: 24 * time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted["usage_records"] != 1 || deleted["usage_conflicts"] != 1 {
		t.Fatalf("deleted = %v, want the event and its conflict", deleted)
	}
	conflicts, err := s.QueryUsageConflicts(ctx, UsageConflictQuery{})
	if err != nil {
		t.Fatalf("QueryUsageConflicts: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none after cleanup", conflicts)
	}
}
