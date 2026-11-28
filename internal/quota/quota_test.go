package quota

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Derive(tc.windows, 0.1)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDeriveWorstWindowWins(t *testing.T) {
	windows := []Window{
		remaining(100, 100, now.Add(time.Hour)),
		{Name: "weekly", Limit: f(200), Remaining: f(0), ResetsAt: now.Add(7 * 24 * time.Hour)},
	}
	got, _ := Derive(windows, 0.1)
	assert.Equal(t, Exhausted, got, "one exhausted window must not be averaged away")

	low := []Window{remaining(100, 100, now.Add(time.Hour)), remaining(5, 100, now.Add(7*24*time.Hour))}
	got, _ = Derive(low, 0.1)
	assert.Equal(t, LowQuota, got)
}

func TestDeriveNeverInventsQuota(t *testing.T) {
	got, reason := Derive(nil, 0.1)
	assert.Equal(t, QuotaUnknown, got)
	assert.Equal(t, MalformedResponse, reason)

	// A positive remaining without a limit cannot be classified.
	got, _ = Derive([]Window{{Remaining: f(50)}}, 0.1)
	assert.Equal(t, QuotaUnknown, got)

	// Absent remaining must not read as zero, and not as available.
	got, _ = Derive([]Window{{Limit: f(100)}}, 0.1)
	assert.Equal(t, QuotaUnknown, got)

	// Zero-or-less limit is not a fraction basis.
	got, _ = Derive([]Window{Window{Limit: f(0), Remaining: f(1)}}, 0.1)
	assert.Equal(t, QuotaUnknown, got)
}

func TestObservedRecordsConfidenceAndProvenance(t *testing.T) {
	full := Observed("minimax", "token_plan/remains", now, []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	state, reason := full.State()
	assert.Equal(t, Available, state)
	assert.Empty(t, reason)
	assert.Equal(t, ConfidenceHigh, full.Confidence)

	// A window missing used/reset is usable but lowers confidence.
	sparse := Observed("minimax", "token_plan/remains", now, []Window{{Limit: f(100), Remaining: f(90)}}, 0.1)
	state, _ = sparse.State()
	assert.Equal(t, Available, state)
	assert.Equal(t, ConfidenceLow, sparse.Confidence)

	sparse.Provenance = map[string]string{"remaining": "weekly.remaining_credits"}
	encoded, err := json.Marshal(sparse)
	require.NoError(t, err)
	assert.True(t, json.Valid(encoded))
}

func TestFailedCarriesNoValues(t *testing.T) {
	s := Failed("commandcode", "none", now, UnsupportedNoQuotaAPI)
	state, reason := s.State()
	assert.Equal(t, QuotaUnknown, state)
	assert.Equal(t, UnsupportedNoQuotaAPI, reason)
	assert.Nil(t, s.Windows)
	assert.Equal(t, ConfidenceNone, s.Confidence)

	state, reason = s.StateAt(now, time.Hour)
	assert.Equal(t, QuotaUnknown, state)
	assert.Equal(t, UnsupportedNoQuotaAPI, reason)
}

func TestEachUnknownReasonSurvivesReport(t *testing.T) {
	for _, reason := range []UnknownReason{
		UnsupportedNoQuotaAPI, TransientFetchFailure, StaleObservation,
		MalformedResponse, AuthFailure, NotYetObserved,
	} {
		t.Run(string(reason), func(t *testing.T) {
			s := Failed("p", "src", now, reason)
			got, gotReason := s.StateAt(now, time.Hour)
			assert.Equal(t, QuotaUnknown, got)
			assert.Equal(t, reason, gotReason)

			// A failure recorded without a timestamp keeps its own reason rather
			// than reporting NOT_YET_OBSERVED and losing the diagnosis.
			zero := Failed("p", "src", time.Time{}, reason)
			got, gotReason = zero.StateAt(now, time.Hour)
			assert.Equal(t, QuotaUnknown, got)
			assert.Equal(t, reason, gotReason)
		})
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
		t.Run(tc.name, func(t *testing.T) {
			got, _ := tc.snap.StateAt(tc.at, tc.age)
			assert.Equal(t, QuotaUnknown, got)
		})
	}

	got, reason := fresh.StateAt(now, time.Hour)
	assert.Equal(t, Available, got)
	assert.Empty(t, reason)
}

func TestSmallClockSkewIsTolerated(t *testing.T) {
	s := Observed("p", "src", now.Add(30*time.Second), []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	got, _ := s.StateAt(now, time.Hour)
	assert.Equal(t, Available, got)
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
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Derive([]Window{tc.win}, 0.1)
			assert.Equal(t, QuotaUnknown, got)
			assert.Equal(t, MalformedResponse, reason)
		})
	}

	// Used never establishes quota, so a broken one must not claim a complete
	// observation either: it lowers confidence, not the state.
	broken := Observed("p", "src", now, []Window{{
		Limit: f(100), Remaining: f(90), Used: f(math.NaN()), ResetsAt: now.Add(time.Hour),
	}}, 0.1)
	state, _ := broken.State()
	assert.Equal(t, Available, state, "Used does not establish quota")
	assert.Equal(t, ConfidenceLow, broken.Confidence, "a NaN Used is not complete accounting")
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
	state, _ := s.State()
	assert.Equal(t, Available, state)
	assert.Equal(t, 90.0, *s.Windows[0].Remaining, "the snapshot shares the caller's pointer")
}

// State is derived on every read, so a snapshot edited after construction cannot
// keep claiming a state its windows no longer support. There is no stored copy
// to disagree with.
func TestStateFollowsWindowsAfterMutation(t *testing.T) {
	s := Observed("p", "src", now, []Window{remaining(90, 100, now.Add(time.Hour))}, 0.1)
	*s.Windows[0].Remaining = 0
	state, _ := s.State()
	assert.Equal(t, Exhausted, state, "after the window was exhausted")

	// A hand-built snapshot with no windows establishes nothing.
	state, _ = (Snapshot{ObservedAt: now}).State()
	assert.Equal(t, QuotaUnknown, state)
	state, _ = (Snapshot{ObservedAt: now, Windows: []Window{remaining(90, 100, now.Add(time.Hour))}}).StateAt(now, time.Hour)
	assert.Equal(t, Available, state, "derived from the windows")
}

func TestUnusableWindowMakesWholeSnapshotUnknown(t *testing.T) {
	// A known-low window next to an unparseable one must not report LOW_QUOTA:
	// the issue requires missing quota to become UNKNOWN, and a consumer cannot
	// otherwise tell a fully known snapshot from a partial one.
	windows := []Window{
		remaining(5, 100, now.Add(time.Hour)),
		{Name: "weekly", Limit: f(200)},
	}
	got, reason := Derive(windows, 0.1)
	assert.Equal(t, QuotaUnknown, got)
	assert.Equal(t, MalformedResponse, reason)

	exhausted := []Window{
		{Name: "5h", Limit: f(100), Remaining: f(0)},
		{Name: "weekly"}, // no remaining at all
	}
	got, _ = Derive(exhausted, 0.1)
	assert.Equal(t, QuotaUnknown, got)
}

func TestResetTimesAreNormalizedToUTC(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	s := Observed("p", "src", now, []Window{remaining(90, 100, now.In(loc))}, 0.1)
	assert.Equal(t, time.UTC, s.Windows[0].ResetsAt.Location())
}

func TestUsageAndHealthAreIndependentOfQuota(t *testing.T) {
	usage := Usage{Provider: "p", Origin: LocallyObserved, Requests: new(int64), InputTokens: nil}
	*usage.Requests = 42
	assert.Equal(t, LocallyObserved, usage.Origin)

	// A healthy provider with an active cooldown still carries no quota claim.
	state := ProviderState{Provider: "p", Health: Healthy, ObservedAt: now, CooldownUntil: now.Add(time.Minute)}
	assert.True(t, state.CooldownActive(now))
	assert.Equal(t, Healthy, state.Health, "cooldown must not change health")
	assert.False(t, (ProviderState{CooldownUntil: now}).CooldownActive(now))
}

// A raw provider error converted to a reason string would carry a credential
// into the model. Both constructors refuse a value outside the declared set.
func TestUnknownReasonVocabularyIsEnforced(t *testing.T) {
	leak := UnknownReason("dial tcp https://user:secret@provider:443 failed")
	s := Failed("p", "src", now, leak)
	state, reason := s.State()
	assert.Equal(t, QuotaUnknown, state)
	assert.Equal(t, MalformedResponse, reason, "for an undeclared reason")

	state, reason = s.StateAt(now, time.Hour)
	assert.Equal(t, QuotaUnknown, state)
	assert.NotEqual(t, leak, reason, "leaked the raw error")

	// The declared set still passes through untouched.
	for _, reason := range []UnknownReason{UnsupportedNoQuotaAPI, TransientFetchFailure, MalformedResponse} {
		got := Failed("p", "src", now, reason)
		_, r := got.State()
		assert.Equal(t, reason, r, "Failed(%s) did not keep its declared reason", reason)
	}
}

func TestHealthReasonVocabularyIsEnforced(t *testing.T) {
	leak := HealthReason("401 https://user:secret@provider/v1: invalid key sk-cp-abc")
	st := NewProviderState("p", Degraded, leak, now)
	assert.Equal(t, ReasonServerError, st.Reason(), "for an undeclared reason")

	st = NewProviderState("p", Degraded, ReasonRateLimited, now)
	assert.Equal(t, ReasonRateLimited, st.Reason())

	// Health is a string too: a value outside its vocabulary cannot become a
	// provider state either.
	st = NewProviderState("p", Health("AVAILABLE"), ReasonNone, now)
	assert.Equal(t, HealthUnknown, st.Health, "for an undeclared health state")
}

func TestExplicitFailureReasonSurvivesAnyMaxAge(t *testing.T) {
	s := Failed("p", "src", now, AuthFailure)
	for _, age := range []time.Duration{time.Hour, 0, -time.Second} {
		t.Run(age.String(), func(t *testing.T) {
			state, reason := s.StateAt(now, age)
			assert.Equal(t, QuotaUnknown, state)
			assert.Equal(t, AuthFailure, reason)
		})
	}
}
