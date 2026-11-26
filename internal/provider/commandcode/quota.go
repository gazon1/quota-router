// Package commandcode reports Command Code subscription quota as UNKNOWN.
//
// Issue #4, the P0 compatibility gate, verified that Command Code documents
// inference endpoints only: /chat/completions, /responses, /messages and /models
// on https://api.commandcode.ai/provider/v1. It established no documented
// subscription-quota endpoint, so issue #11 requires UNKNOWN with
// UnsupportedNoQuotaAPI instead of a speculative client.
//
// The /alpha routes recorded in docs/litellm-provider-compatibility.md answered
// 200 when probed, but they are undocumented and were not confirmed as permitted
// for automated polling. This adapter therefore holds no URL, credential or HTTP
// client, so it cannot be aimed at them: using them is a decision for a new gate,
// not a configuration change.
package commandcode

import (
	"context"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
)

const (
	// Provider is the name these observations are reported under.
	Provider = "commandcode"
	// Source records that no quota source exists. It is never an inference
	// endpoint: a successful completion says nothing about subscription quota.
	Source = "none: no documented Command Code subscription quota source"
)

// Adapter reports Command Code subscription quota. The zero value is usable and
// has nothing to configure: there is no endpoint, key or timeout to get wrong.
type Adapter struct{}

// Fetch reports that Command Code subscription quota is not observable. It makes
// no request, so authentication, rate-limit, server and transport failures cannot
// occur and none of them can be misreported as exhausted quota.
//
// Health is Unknown/NotProbed rather than Unavailable or Healthy: nothing was
// probed, and the provider may be perfectly reachable. Kept independent of quota
// as the model requires.
func (Adapter) Fetch(context.Context) (quota.Snapshot, quota.ProviderState) {
	at := time.Now().UTC()
	return quota.Failed(Provider, Source, at, quota.UnsupportedNoQuotaAPI),
		quota.NewProviderState(Provider, quota.HealthUnknown, quota.ReasonNotProbed, at)
}
