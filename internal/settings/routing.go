package settings

// Routing configures routing thresholds and the static provider/model
// preference order. Preference is consulted only after hard constraints and
// viable session affinity.
type Routing struct {
	// LowQuotaThreshold is the remaining-quota fraction at or below which a
	// provider counts as scarce and loses to a healthier candidate.
	LowQuotaThreshold float64 `koanf:"low_quota_threshold"`
	// ReserveFraction is the remaining-quota fraction reserved for reasoning and
	// coding workloads. A provider at or below this fraction is ineligible for
	// those task classes while quota is not exhausted. Zero disables the reserve.
	ReserveFraction float64 `koanf:"reserve_fraction"`
	// Providers is the static preference order. Keys are provider names as they
	// appear in model ids (<provider>/<model>). When empty, no static preference
	// is configured. When set, every enabled catalogue alias must be listed under
	// its provider with an explicit priority.
	Providers map[string]ProviderRouting `koanf:"providers"`
}

// ProviderRouting orders one provider and the model aliases served by it.
// Lower numeric priority means higher preference.
type ProviderRouting struct {
	// Priority is this provider's static preference rank. Must be a positive
	// integer; lower values are preferred. Duplicate provider priorities are invalid.
	Priority int `koanf:"priority"`
	// Models orders catalogue aliases within this provider. Keys are catalogue
	// aliases; values are positive integer priorities where lower is preferred.
	// Model priority is scoped to this provider and never compared across providers.
	Models map[string]int `koanf:"models"`
}

// Provider is a quota provider the controller polls directly.
type Provider struct {
	BaseURL string `koanf:"base_url"`
	APIKey  Secret `koanf:"api_key"`
}
