package settings

// Routing configures routing thresholds and the static provider/model
// preference order. Preference is consulted only after hard constraints and
// viable session affinity.
type Routing struct {
	// LowQuotaThreshold is the remaining-quota fraction at or below which a
	// provider counts as scarce and loses to a healthier candidate.
	LowQuotaThreshold float64
	// ReserveFraction is the remaining-quota fraction reserved for reasoning and
	// coding workloads. A provider at or below this fraction is ineligible for
	// those task classes while quota is not exhausted. Zero disables the reserve.
	ReserveFraction float64
	// Providers is the static preference order. Keys are provider names as they
	// appear in model ids (<provider>/<model>). When empty, no static preference
	// is configured.
	Providers map[string]ProviderRouting
}

// ProviderRouting orders one provider and the model aliases served by it.
// Lower numeric priority means higher preference.
type ProviderRouting struct {
	// Priority is this provider's static preference rank. Lower values are
	// preferred.
	Priority int
	// Models orders catalogue aliases within this provider. Model priority is
	// scoped to this provider and never compared across providers.
	Models map[string]int
}
