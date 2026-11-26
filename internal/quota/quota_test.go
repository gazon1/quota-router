package quota

import (
	"encoding/json"
	"math"
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
	if state, reason := full.State(); state != Available || reason != "" || full.Confidence != ConfidenceHigh {
		t.Fatalf("Observed(full) = %s/%s %+v, want AVAILABLE/HIGH", state, reason, full)
	}

	// A window missing used/reset is usable but lowers confidence.
	sparse := Observed("minimax", "token_plan/remains", now, []Window{{Limit: f(100), Remaining: f(90)}}, 0.1)
	if state, _ := sparse.State(); state != Available || sparse.Confidence != ConfidenceLow {
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
	if state, reason := s.State(); state != QuotaUnknown || reason != UnsupportedNoQuotaAPI || s.Windows != nil || s.Confidence != ConfidenceNone {
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
		// A failure recorded without a timestamp keeps its own reason rather
		// than reporting NOT_YET_OBSERVED and losing the diagnosis.
		zero := Failed("p", "src", time.Time{}, reason)
		if got, gotReason := zero.StateAt(now, time.Hour); got != QuotaUnknown || gotReason != reason {
			t.Errorf("StateAt(zero timestamp) = %s/%s, want UNKNOWN/%s", got, gotReason, reason)
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
		{"never observed", Snapshot{}, time.Time{}, time.Hour},
		{"stale beyond max age", Observed("p", "src", now.Add(-2*time.Hour), fresh.Windows, 0.1), now, time.Hour},
		{"marked stale", stale, now, time.Hour},
		{"dated in the future beyond skew", Observed("p", "src", now.Add(10*time.Minute), fresh.Windows, 0.1), now, time.Hour},
		{"unset max age fails closed", fresh, now, 0},
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
	if got, _ := s.StateAt(now, time.Hour); got != Available {
		t.Fatalf("StateAt(skew 30s) = %s, want AVAILABLE", got)
	}
}

// A provider response can carry a NaN or an infinity through a numeric field an
// adapter failed to parse. NaN compares false against everything, so without an
// explicit check it would fall through to AVAILABLE.
func TestNonFiniteValuesNeverBecomeQuota(t *testing.T) {
	inf := math.Inf(1)
	tests := []struct {
		name string
		win  Window
	}{
		{"NaN remaining", Window{Limit: f(100), Remaining: f(math.NaN())}},
		{"Inf remaining", Window{Limit: f(100), Remaining: &inf}},
		{"NaN limit", Window{Limit: f(math.NaN()), Remaining: f(50)}},
		{"Inf limit", Window{Limit: &inf, Remaining: f(50)}},
	}
	for _, tc := range tests {
		if got, reason := Derive([]Window{tc.win}, 0.1); got != QuotaUnknown || reason != MalformedResponse {
			t.Errorf("%s: Derive() = %s/%s, want UNKNOWN/MALFORMED_RESPONSE", tc.name, got, reason)
		}
	}

	// Used never establishes quota, so a broken one must not claim a complete
	// observation either: it lowers confidence, not the state.
	broken := Observed("p", "src", now, []Window{{
		Limit: f(100), Remaining: f(90), Used: f(math.NaN()), ResetsAt: now.Add(time.Hour),
	}}, 0.1)
	if state, _ := broken.State(); state != Available {
		t.Fatalf("State() = %s, want AVAILABLE: Used does not establish quota", state)
	}
	if broken.Confidence != ConfidenceLow {
		t.Fatalf("Confidence = %s, want LOW: a NaN Used is not complete accounting", broken.Confidence)
	}
}

// The snapshot owns its values: mutating the caller's windows afterwards must
// not leave a State that disagrees with the Windows it reports.
func TestObservedIsImmuneToCallerMutation(t *testing.T) {
	limit, used, rem := 100.0, 10.0, 90.0
	win := Window{Limit: &limit, Used: &used, Remaining: &rem, ResetsAt: now.Add(time.Hour)}
	loc := time.FixedZone("MSK", 3*60*60)
	s := Observed("p", "src", now, []Window{win}, 0.1)
	s.Windows[0].ResetsAt = now.In(loc) // exercise the copy, not the caller's value

	rem = 0
	if state, _ := s.State(); state != Available {
		t.Fatalf("State() = %s, want AVAILABLE", state)
	}
	if *s.Windows[0].Remaining != 90 {
		t.Fatalf("Windows[0].Remaining = %v, want 90: the snapshot shares the caller's pointer", *s.Windows[0].Remaining)
	}
}

// State is derived on every read, so a snapshot edited after construction cannot
// keep claiming a state its windows no longer support. There is no stored copy
// to disagree with.
func TestStateFollowsWindowsAfterMutation(t *testing.T) {
	s := Observed("p", "src", now, []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	*s.Windows[0].Remaining = 0
	if state, _ := s.State(); state != Exhausted {
		t.Fatalf("State() = %s, want EXHAUSTED after the window was exhausted", state)
	}

	// A hand-built snapshot with no windows establishes nothing.
	if state, _ := (Snapshot{ObservedAt: now}).State(); state != QuotaUnknown {
		t.Fatalf("State() = %s for an empty snapshot, want UNKNOWN", state)
	}
	if state, _ := (Snapshot{ObservedAt: now, Windows: []Window{remaining(90, 100, now.Add(time.Hour))}}).StateAt(now, time.Hour); state != Available {
		t.Fatalf("StateAt() = %s, want AVAILABLE derived from the windows", state)
	}
}

func TestUnusableWindowMakesWholeSnapshotUnknown(t *testing.T) {
	// A known-low window next to an unparseable one must not report LOW_QUOTA:
	// the issue requires missing quota to become UNKNOWN, and a consumer cannot
	// otherwise tell a fully known snapshot from a partial one.
	windows := []Window{
		remaining(5, 100, now.Add(time.Hour)),
		{Name: "weekly", Limit: f(200)},
	}
	if got, reason := Derive(windows, 0.1); got != QuotaUnknown || reason != MalformedResponse {
		t.Fatalf("Derive(low + missing) = %s/%s, want UNKNOWN/MALFORMED_RESPONSE", got, reason)
	}

	exhausted := []Window{
		{Name: "5h", Limit: f(100), Remaining: f(0)},
		{Name: "weekly"}, // no remaining at all
	}
	if got, _ := Derive(exhausted, 0.1); got != QuotaUnknown {
		t.Fatalf("Derive(exhausted + missing) = %s, want UNKNOWN", got)
	}
}

func TestResetTimesAreNormalizedToUTC(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	s := Observed("p", "src", now, []Window{remaining(90, 100, now.In(loc))}, 0.1)
	if s.Windows[0].ResetsAt.Location() != time.UTC {
		t.Fatalf("ResetsAt location = %v, want UTC", s.Windows[0].ResetsAt.Location())
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

// A raw provider error converted to a reason string would carry a credential
// into the model. Both constructors refuse a value outside the declared set.
func TestUnknownReasonVocabularyIsEnforced(t *testing.T) {
	leak := UnknownReason("dial tcp https://user:secret@provider:443 failed")
	s := Failed("p", "src", now, leak)
	if state, reason := s.State(); state != QuotaUnknown || reason != MalformedResponse {
		t.Fatalf("State() = %s/%s, want UNKNOWN/MALFORMED_RESPONSE for an undeclared reason", state, reason)
	}
	if state, reason := s.StateAt(now, time.Hour); state != QuotaUnknown || reason == leak {
		t.Fatalf("StateAt() = %s/%s, leaked the raw error", state, reason)
	}

	// The declared set still passes through untouched.
	for _, reason := range []UnknownReason{UnsupportedNoQuotaAPI, TransientFetchFailure, MalformedResponse} {
		if got := Failed("p", "src", now, reason); func() UnknownReason { _, r := got.State(); return r }() != reason {
			t.Errorf("Failed(%s) did not keep its declared reason", reason)
		}
	}
}

func TestHealthReasonVocabularyIsEnforced(t *testing.T) {
	leak := HealthReason("401 https://user:secret@provider/v1: invalid key sk-cp-abc")
	st := NewProviderState("p", Degraded, leak, now)
	if st.Reason() != ReasonServerError {
		t.Fatalf("Reason() = %q, want PROVIDER_ERROR for an undeclared reason", st.Reason())
	}

	st = NewProviderState("p", Degraded, ReasonRateLimited, now)
	if st.Reason() != ReasonRateLimited {
		t.Fatalf("Reason() = %q, want RATE_LIMITED", st.Reason())
	}
}

func TestExplicitFailureReasonSurvivesAnyMaxAge(t *testing.T) {
	s := Failed("p", "src", now, AuthFailure)
	for _, age := range []time.Duration{time.Hour, 0, -time.Second} {
		if state, reason := s.StateAt(now, age); state != QuotaUnknown || reason != AuthFailure {
			t.Errorf("StateAt(maxAge=%s) = %s/%s, want UNKNOWN/AUTHENTICATION_OR_PERMISSION_FAILURE", age, state, reason)
		}
	}
}
