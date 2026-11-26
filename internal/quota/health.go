package quota

import "time"

// Health is the operational condition of a provider. HTTP success proves
// reachability at most; it never proves subscription quota is available.
type Health string

const (
	Healthy     Health = "HEALTHY"
	Degraded    Health = "DEGRADED"
	Unavailable Health = "UNAVAILABLE"
	// HealthUnknown means this installation has no usable observation.
	HealthUnknown Health = "UNKNOWN"
)

// ProviderState is one observation of a provider's health, plus any temporary
// routing-policy exclusions. A cooldown is a policy decision about routing, not
// a quota state: a cooled-down provider keeps whatever quota it had.
type ProviderState struct {
	Provider   string
	Health     Health
	Reason     string // short non-secret explanation, empty when healthy
	ObservedAt time.Time
	// CooldownUntil excludes the provider from routing until then; zero means
	// no cooldown. It says nothing about quota or health.
	CooldownUntil time.Time
}

// CooldownActive reports whether the provider is excluded from routing at now.
func (s ProviderState) CooldownActive(now time.Time) bool {
	return s.CooldownUntil.After(now)
}
