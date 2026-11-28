package config

import "github.com/gazon1/quota-router/internal/settings"

// Routing and ProviderRouting are the configuration shape of the routing
// policy. The runtime shape lives in internal/settings, stripped of koanf tags
// and validation rules; Catalogue converts one to the other.

// Routing configures routing thresholds and the static provider/model
// preference order.
type Routing struct {
	// LowQuotaThreshold is the remaining-quota fraction at or below which a
	// provider counts as scarce and loses to a healthier candidate.
	LowQuotaThreshold float64 `koanf:"low_quota_threshold" validate:"gt=0,lte=1"`
	// ReserveFraction is the remaining-quota fraction reserved for reasoning and
	// coding workloads. Zero disables the reserve.
	ReserveFraction float64 `koanf:"reserve_fraction" validate:"gte=0,lte=1"`
	// Providers is the static preference order, keyed by provider name.
	Providers map[string]ProviderRouting `koanf:"providers" validate:"dive,keys,required,endkeys,required"`
}

// ProviderRouting orders one provider and the model aliases served by it.
type ProviderRouting struct {
	// Priority is this provider's static preference rank; lower is preferred.
	Priority int `koanf:"priority" validate:"gt=0"`
	// Models orders catalogue aliases within this provider; lower is preferred.
	Models map[string]int `koanf:"models" validate:"dive,keys,required,endkeys,gt=0"`
}

// runtime converts the policy to the runtime shape the selector reads.
func (r Routing) runtime() settings.Routing {
	out := settings.Routing{
		LowQuotaThreshold: r.LowQuotaThreshold,
		ReserveFraction:   r.ReserveFraction,
		Providers:         make(map[string]settings.ProviderRouting, len(r.Providers)),
	}
	for name, p := range r.Providers {
		out.Providers[name] = settings.ProviderRouting{Priority: p.Priority, Models: p.Models}
	}
	return out
}
