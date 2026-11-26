package quota

import "time"

// UsageOrigin records where a usage figure came from. Only ProviderReported
// figures are a provider's own accounting; the other two are this installation's
// observations and are not comparable across machines.
type UsageOrigin string

const (
	// ProviderReported means the provider returned the figure itself.
	ProviderReported UsageOrigin = "PROVIDER_REPORTED"
	// LocallyObserved means this installation counted it from requests it saw,
	// for example LiteLLM callbacks or agent usage reports.
	LocallyObserved UsageOrigin = "LOCALLY_OBSERVED"
	// Estimated means the figure was derived, for example from token counting.
	Estimated UsageOrigin = "ESTIMATED"
)

// Usage is locally useful accounting and correlation data. It is never a
// substitute for subscription quota: no value here says how much quota remains,
// and converting one into the other is not defined.
type Usage struct {
	Provider   string
	Source     string
	ObservedAt time.Time
	Origin     UsageOrigin
	// Nil fields were not recorded; they are not zero.
	Requests     *int64
	InputTokens  *int64
	OutputTokens *int64
}
