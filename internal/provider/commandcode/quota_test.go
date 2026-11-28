package commandcode

import (
	"context"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/stretchr/testify/assert"
)

// TestFetchReportsUnsupportedNotZero is the acceptance criterion of #11: with no
// documented quota API the result is UNKNOWN with a clear reason and no values.
// Zero remaining, AVAILABLE and any invented window all fail here.
func TestFetchReportsUnsupportedNotZero(t *testing.T) {
	snap, state := Adapter{}.Fetch(context.Background())

	got, reason := snap.State()
	assert.Equal(t, quota.QuotaUnknown, got)
	assert.Equal(t, quota.UnsupportedNoQuotaAPI, reason)
	assert.Nil(t, snap.Windows)
	assert.Equal(t, quota.ConfidenceNone, snap.Confidence)
	assert.Equal(t, Provider, snap.Provider)
	assert.Equal(t, Source, snap.Source)
	// Health must not borrow the quota verdict: nothing was probed, so it is
	// Unknown/NotProbed, never Unavailable or Exhausted.
	assert.Equal(t, quota.HealthUnknown, state.Health)
	assert.Equal(t, quota.ReasonNotProbed, state.Reason())
	assert.False(t, state.CooldownActive(time.Now()), "want no routing exclusion")
	// A freshness bound must not turn the refusal into a different diagnosis.
	for _, maxAge := range []time.Duration{time.Hour, 0, -time.Second} {
		got, reason := snap.StateAt(time.Now(), maxAge)
		assert.Equal(t, quota.QuotaUnknown, got, "StateAt(%s)", maxAge)
		assert.Equal(t, quota.UnsupportedNoQuotaAPI, reason, "StateAt(%s)", maxAge)
	}
}

// The "do not ship a speculative /alpha client" criterion needs no test of its
// own: Adapter has no fields, so there is no endpoint to aim and no transport to
// replace. A request-intercepting test would prove a fact about http.DefaultTransport
// rather than about this adapter, and would stop catching anything the day a
// custom transport were introduced.
