package routing

// ReasonCode is the stable, machine-readable half of an explanation. The prose
// in RejectedCandidate.Reason is written for a human and may be reworded; the
// code is the contract a consumer branches on, so it changes only together with
// the policy version.
//
// The vocabulary is closed. A code that is not listed here is a bug, not an
// extension point: a consumer must be able to treat an unknown code as "some
// other reason" without guessing what it means.
type ReasonCode string

const (
	// ReasonSelectedByPreference means the candidate won the configured and
	// request preference ranking.
	ReasonSelectedByPreference ReasonCode = "selected_by_preference"
	// ReasonSelectedByOverride means a manual override chose the candidate.
	ReasonSelectedByOverride ReasonCode = "selected_by_override"
	// ReasonSelectedByAffinity means the session's route pin kept the candidate.
	ReasonSelectedByAffinity ReasonCode = "selected_by_affinity"
	// ReasonEligibleNotSelected means the candidate satisfied every hard
	// constraint but lost the preference ranking.
	ReasonEligibleNotSelected ReasonCode = "eligible_not_selected"
	// ReasonNotSelectedByOverride means a manual override chose another route.
	ReasonNotSelectedByOverride ReasonCode = "not_selected_by_override"
	// ReasonNotSelectedByAffinity means the session's route pin chose another
	// route.
	ReasonNotSelectedByAffinity ReasonCode = "not_selected_by_affinity"

	// ReasonModelDisabled means the alias is disabled in the catalogue.
	ReasonModelDisabled ReasonCode = "model_disabled"
	// ReasonCapabilityMismatch means the model lacks a required capability or
	// task class.
	ReasonCapabilityMismatch ReasonCode = "capability_mismatch"
	// ReasonModelUnavailableWindow means the model is inside its configured
	// recurring unavailable window.
	ReasonModelUnavailableWindow ReasonCode = "model_unavailable_window"

	// ReasonHealthNotObserved means the provider carries no health observation.
	ReasonHealthNotObserved ReasonCode = "health_not_observed"
	// ReasonHealthStale means the health observation is older than the
	// freshness bound and cannot be acted on.
	ReasonHealthStale ReasonCode = "health_stale"
	// ReasonProviderUnavailable means the provider reported itself unavailable.
	ReasonProviderUnavailable ReasonCode = "provider_unavailable"
	// ReasonHealthUnknown means the provider's health state is UNKNOWN.
	ReasonHealthUnknown ReasonCode = "health_unknown"

	// ReasonQuotaNotObserved means the provider carries no quota observation.
	ReasonQuotaNotObserved ReasonCode = "quota_not_observed"
	// ReasonQuotaUnknown means quota could not be established; the snapshot
	// carries the reason.
	ReasonQuotaUnknown ReasonCode = "quota_unknown"
	// ReasonQuotaExhausted means the subscription quota is exhausted.
	ReasonQuotaExhausted ReasonCode = "quota_exhausted"
	// ReasonQuotaReserved means the remaining quota is reserved for another
	// workload.
	ReasonQuotaReserved ReasonCode = "quota_reserved"
	// ReasonQuotaIneligible means the quota state is not an eligible one.
	ReasonQuotaIneligible ReasonCode = "quota_ineligible"

	// ReasonProviderCooldown means routing policy cooled the provider down.
	ReasonProviderCooldown ReasonCode = "provider_cooldown"
)
