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

// HealthReason is a closed set of non-secret explanations, the same discipline
// as UnknownReason: a raw provider error can carry a credential in a URL or a
// header, so it never enters the model. An adapter maps an error to one of
// these codes and keeps the text in its own logs.
type HealthReason string

const (
	// ReasonNone means the health state needs no explanation.
	ReasonNone HealthReason = ""
	// ReasonAuthFailed means the provider rejected authentication or permission.
	ReasonAuthFailed HealthReason = "AUTHENTICATION_OR_PERMISSION_FAILURE"
	// ReasonTimeout means the probe exceeded its deadline.
	ReasonTimeout HealthReason = "TIMEOUT"
	// ReasonRateLimited means the provider returned a rate or usage limit.
	ReasonRateLimited HealthReason = "RATE_LIMITED"
	// ReasonServerError means the provider answered 5xx or an unusable body.
	ReasonServerError HealthReason = "PROVIDER_ERROR"
	// ReasonUnreachable means the connection failed or was refused.
	ReasonUnreachable HealthReason = "UNREACHABLE"
	// ReasonNotProbed means this installation has not probed yet.
	ReasonNotProbed HealthReason = "NOT_PROBED"
)

// ProviderState is one observation of a provider's health, plus any temporary
// routing-policy exclusions. A cooldown is a policy decision about routing, not
// a quota state: a cooled-down provider keeps whatever quota it had.
type ProviderState struct {
	Provider   string
	Health     Health
	Reason     HealthReason
	ObservedAt time.Time
	// CooldownUntil excludes the provider from routing until then; zero means
	// no cooldown. It says nothing about quota or health.
	CooldownUntil time.Time
}

// CooldownActive reports whether the provider is excluded from routing at now.
func (s ProviderState) CooldownActive(now time.Time) bool {
	return s.CooldownUntil.After(now)
}
