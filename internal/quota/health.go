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

// known reports whether r is one of the declared reasons.
func (r HealthReason) known() bool {
	switch r {
	case ReasonNone, ReasonAuthFailed, ReasonTimeout, ReasonRateLimited,
		ReasonServerError, ReasonUnreachable, ReasonNotProbed:
		return true
	}
	return false
}

// ProviderState is one observation of a provider's health, plus any temporary
// routing-policy exclusions. A cooldown is a policy decision about routing, not
// a quota state: a cooled-down provider keeps whatever quota it had. Reason is
// unexported so an adapter cannot put a raw provider error there; use
// NewProviderState.
type ProviderState struct {
	Provider   string
	Health     Health
	ObservedAt time.Time
	// CooldownUntil excludes the provider from routing until then; zero means
	// no cooldown. It says nothing about quota or health.
	CooldownUntil time.Time

	reason HealthReason
}

// known reports whether h is one of the declared health states.
func (h Health) known() bool {
	switch h {
	case Healthy, Degraded, Unavailable, HealthUnknown:
		return true
	}
	return false
}

// NewProviderState builds a health observation. A value outside a declared
// vocabulary becomes the safe fallback: the types are strings, so a caller
// could pass a raw provider error, which may carry a credential.
func NewProviderState(provider string, health Health, reason HealthReason, at time.Time) ProviderState {
	if !health.known() {
		health = HealthUnknown
		reason = ReasonServerError
	}
	if !reason.known() {
		reason = ReasonServerError
	}
	return ProviderState{Provider: provider, Health: health, ObservedAt: at, reason: reason}
}

// Reason reports the closed reason code for this health observation.
func (s ProviderState) Reason() HealthReason {
	return s.reason
}

// CooldownActive reports whether the provider is excluded from routing at now.
func (s ProviderState) CooldownActive(now time.Time) bool {
	return s.CooldownUntil.After(now)
}
