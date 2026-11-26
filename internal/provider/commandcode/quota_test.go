package commandcode

import (
	"context"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
)

// TestFetchReportsUnsupportedNotZero is the acceptance criterion of #11: with no
// documented quota API the result is UNKNOWN with a clear reason and no values.
// Zero remaining, AVAILABLE and any invented window all fail here.
func TestFetchReportsUnsupportedNotZero(t *testing.T) {
	snap, state := Adapter{}.Fetch(context.Background())

	if got, reason := snap.State(); got != quota.QuotaUnknown || reason != quota.UnsupportedNoQuotaAPI {
		t.Fatalf("State() = %s/%s, want UNKNOWN/%s", got, reason, quota.UnsupportedNoQuotaAPI)
	}
	if snap.Windows != nil || snap.Confidence != quota.ConfidenceNone {
		t.Fatalf("Fetch() = %+v, want no windows and no confidence", snap)
	}
	if snap.Provider != Provider || snap.Source != Source {
		t.Fatalf("Provider/Source = %s/%s, want %s/%s", snap.Provider, snap.Source, Provider, Source)
	}
	// Health must not borrow the quota verdict: nothing was probed, so it is
	// Unknown/NotProbed, never Unavailable or Exhausted.
	if state.Health != quota.HealthUnknown || state.Reason() != quota.ReasonNotProbed {
		t.Fatalf("health = %s/%s, want UNKNOWN/%s", state.Health, state.Reason(), quota.ReasonNotProbed)
	}
	if state.CooldownActive(time.Now()) {
		t.Error("CooldownActive = true, want no routing exclusion")
	}
	// A freshness bound must not turn the refusal into a different diagnosis.
	for _, maxAge := range []time.Duration{time.Hour, 0, -time.Second} {
		if got, reason := snap.StateAt(time.Now(), maxAge); got != quota.QuotaUnknown || reason != quota.UnsupportedNoQuotaAPI {
			t.Errorf("StateAt(%s) = %s/%s, want UNKNOWN/%s", maxAge, got, reason, quota.UnsupportedNoQuotaAPI)
		}
	}
}

// The "do not ship a speculative /alpha client" criterion needs no test of its
// own: Adapter has no fields, so there is no endpoint to aim and no transport to
// replace. A request-intercepting test would prove a fact about http.DefaultTransport
// rather than about this adapter, and would stop catching anything the day a
// custom transport were introduced.
