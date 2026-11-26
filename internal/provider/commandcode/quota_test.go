package commandcode

import (
	"context"
	"net/http"
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

// roundTripperFunc lets the test count any request the adapter might make.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestFetchMakesNoRequest guards the "do not ship a speculative /alpha client"
// criterion: any outbound poll of an undocumented route fails here instead of
// silently becoming production polling.
func TestFetchMakesNoRequest(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("Fetch requested %s, want no request at all", r.URL.Redacted())
		return nil, nil
	})

	Adapter{}.Fetch(context.Background())
}
