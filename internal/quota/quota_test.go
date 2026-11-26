package quota

import (
	"encoding/json"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }

func remaining(v float64, limit float64, reset time.Time) Window {
	return Window{Name: "5h", Limit: f(limit), Used: f(limit - v), Remaining: f(v), ResetsAt: reset}
}

func TestDeriveStateBoundaries(t *testing.T) {
	reset := now.Add(5 * time.Hour)
	tests := []struct {
		name    string
		windows []Window
		want    QuotaState
	}{
		{"well above threshold", []Window{remaining(90, 100, reset)}, Available},
		{"just above threshold", []Window{remaining(11, 100, reset)}, Available},
		{"at threshold is low", []Window{remaining(10, 100, reset)}, LowQuota},
		{"below threshold", []Window{remaining(1, 100, reset)}, LowQuota},
		{"zero remaining is exhausted", []Window{remaining(0, 100, reset)}, Exhausted},
		{"negative remaining is exhausted", []Window{remaining(-5, 100, reset)}, Exhausted},
		{"zero remaining without limit is still exhausted", []Window{Window{Remaining: f(0)}}, Exhausted},
	}
	for _, tc := range tests {
		if got, _ := Derive(tc.windows, 0.1); got != tc.want {
			t.Errorf("%s: Derive() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestDeriveWorstWindowWins(t *testing.T) {
	windows := []Window{
		remaining(100, 100, now.Add(time.Hour)),
		{Name: "weekly", Limit: f(200), Remaining: f(0), ResetsAt: now.Add(7 * 24 * time.Hour)},
	}
	if got, _ := Derive(windows, 0.1); got != Exhausted {
		t.Fatalf("Derive() = %s, want EXHAUSTED: one exhausted window must not be averaged away", got)
	}

	low := []Window{remaining(100, 100, now.Add(time.Hour)), remaining(5, 100, now.Add(7*24*time.Hour))}
	if got, _ := Derive(low, 0.1); got != LowQuota {
		t.Fatalf("Derive() = %s, want LOW_QUOTA", got)
	}
}

func TestDeriveNeverInventsQuota(t *testing.T) {
	if got, reason := Derive(nil, 0.1); got != QuotaUnknown || reason != MalformedResponse {
		t.Fatalf("Derive(nil) = %s/%s, want UNKNOWN/MALFORMED_RESPONSE", got, reason)
	}
	// A positive remaining without a limit cannot be classified.
	if got, _ := Derive([]Window{{Remaining: f(50)}}, 0.1); got != QuotaUnknown {
		t.Fatalf("Derive(remaining only) = %s, want UNKNOWN", got)
	}
	// Absent remaining must not read as zero, and not as available.
	if got, _ := Derive([]Window{{Limit: f(100)}}, 0.1); got != QuotaUnknown {
		t.Fatalf("Derive(missing remaining) = %s, want UNKNOWN", got)
	}
	// Zero-or-less limit is not a fraction basis.
	if got, _ := Derive([]Window{Window{Limit: f(0), Remaining: f(1)}}, 0.1); got != QuotaUnknown {
		t.Fatalf("Derive(zero limit) = %s, want UNKNOWN", got)
	}
}

func TestObservedRecordsConfidenceAndProvenance(t *testing.T) {
	full := Observed("minimax", "token_plan/remains", now, []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	if full.State != Available || full.Confidence != ConfidenceHigh || full.Unknown != "" {
		t.Fatalf("Observed(full) = %+v, want AVAILABLE/HIGH", full)
	}

	// A window missing used/reset is usable but lowers confidence.
	sparse := Observed("minimax", "token_plan/remains", now, []Window{{Limit: f(100), Remaining: f(90)}}, 0.1)
	if sparse.State != Available || sparse.Confidence != ConfidenceLow {
		t.Fatalf("Observed(sparse) = %+v, want AVAILABLE/LOW", sparse)
	}

	sparse.Provenance = map[string]string{"remaining": "weekly.remaining_credits"}
	encoded, err := json.Marshal(sparse)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("Marshal() produced invalid JSON: %s", encoded)
	}
}

func TestFailedCarriesNoValues(t *testing.T) {
	s := Failed("commandcode", "none", now, UnsupportedNoQuotaAPI)
	if s.State != QuotaUnknown || s.Unknown != UnsupportedNoQuotaAPI || s.Windows != nil || s.Confidence != ConfidenceNone {
		t.Fatalf("Failed() = %+v, want UNKNOWN with no windows", s)
	}
	if got, reason := s.StateAt(now, time.Hour); got != QuotaUnknown || reason != UnsupportedNoQuotaAPI {
		t.Fatalf("StateAt() = %s/%s, want UNKNOWN/%s", got, reason, UnsupportedNoQuotaAPI)
	}
}

func TestEachUnknownReasonSurvivesReport(t *testing.T) {
	for _, reason := range []UnknownReason{
		UnsupportedNoQuotaAPI, TransientFetchFailure, StaleObservation,
		MalformedResponse, AuthFailure, NotYetObserved,
	} {
		s := Failed("p", "src", now, reason)
		if got, gotReason := s.StateAt(now, time.Hour); got != QuotaUnknown || gotReason != reason {
			t.Errorf("StateAt() = %s/%s, want UNKNOWN/%s", got, gotReason, reason)
		}
	}
}

func TestStateAtRejectsUntrustworthyObservations(t *testing.T) {
	fresh := Observed("p", "src", now, []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	stale := fresh
	stale.Stale = true

	tests := []struct {
		name string
		snap Snapshot
		at   time.Time
		age  time.Duration
	}{
		{"never observed", Snapshot{State: Available}, time.Time{}, 0},
		{"stale beyond max age", Observed("p", "src", now.Add(-2*time.Hour), fresh.Windows, 0.1), now, time.Hour},
		{"marked stale", stale, now, 0},
		{"dated in the future beyond skew", Observed("p", "src", now.Add(10*time.Minute), fresh.Windows, 0.1), now, 0},
	}
	for _, tc := range tests {
		if got, _ := tc.snap.StateAt(tc.at, tc.age); got != QuotaUnknown {
			t.Errorf("%s: StateAt() = %s, want UNKNOWN", tc.name, got)
		}
	}

	if got, reason := fresh.StateAt(now, time.Hour); got != Available || reason != "" {
		t.Fatalf("StateAt(fresh) = %s/%s, want AVAILABLE", got, reason)
	}
}

func TestSmallClockSkewIsTolerated(t *testing.T) {
	s := Observed("p", "src", now.Add(30*time.Second), []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	if got, _ := s.StateAt(now, 0); got != Available {
		t.Fatalf("StateAt(skew 30s) = %s, want AVAILABLE", got)
	}
	// maxAge zero disables the freshness check, but skew never does.
	stale := Observed("p", "src", now.Add(-48*time.Hour), []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	if got, _ := stale.StateAt(now, 0); got != Available {
		t.Fatalf("StateAt(maxAge 0) = %s, want AVAILABLE", got)
	}
}

func TestUsageAndHealthAreIndependentOfQuota(t *testing.T) {
	usage := Usage{Provider: "p", Origin: LocallyObserved, Requests: new(int64), InputTokens: nil}
	*usage.Requests = 42
	if usage.Origin != LocallyObserved {
		t.Fatalf("Origin = %s", usage.Origin)
	}

	// A healthy provider with an active cooldown still carries no quota claim.
	state := ProviderState{Provider: "p", Health: Healthy, ObservedAt: now, CooldownUntil: now.Add(time.Minute)}
	if !state.CooldownActive(now) {
		t.Fatal("CooldownActive() = false, want true")
	}
	if state.Health != Healthy {
		t.Fatal("cooldown must not change health")
	}
	if (ProviderState{CooldownUntil: now}).CooldownActive(now) {
		t.Fatal("CooldownActive() = true for zero CooldownUntil")
	}
}
