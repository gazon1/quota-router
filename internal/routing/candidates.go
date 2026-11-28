package routing

import (
	"sort"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
)

// CandidateEvaluation is what the selector did with one candidate: the order it
// was evaluated in, whether it was eligible and whether it was chosen, the
// stable reason code that decided its fate, and the observations the hard
// checks read.
//
// It carries values, not provider payloads: the quota state, the health state
// and the cooldown are the numbers the decision was made on, copied so the
// decision can be reconstructed later without the observation rows still
// existing. A value that was not observed stays empty — it is never filled in
// with a zero or with a healthy-looking default.
type CandidateEvaluation struct {
	// Alias is the catalogue alias that was evaluated.
	Alias string `json:"alias"`
	// Provider is the provider the alias belongs to.
	Provider string `json:"provider"`
	// Ordinal is the evaluation order, 1-based, in the catalogue's stable alias
	// order — the order the selector considered the candidates in.
	Ordinal int `json:"ordinal"`
	// Selected reports whether this candidate is the route the decision chose.
	Selected bool `json:"selected"`
	// Eligible reports whether the candidate satisfied every hard constraint.
	// A candidate eliminated by a preference stage (a manual override, the
	// session pin) was eligible and merely not chosen.
	Eligible bool `json:"eligible"`
	// EliminatedStage is the stage that eliminated the candidate, empty when
	// it was not eliminated.
	EliminatedStage string `json:"eliminated_stage,omitempty"`
	// ReasonCode is the stable code of the decision about this candidate.
	ReasonCode ReasonCode `json:"reason_code"`
	// Reason is the human-readable explanation behind the code.
	Reason string `json:"reason,omitempty"`
	// Transient marks an elimination that says nothing about the route itself:
	// the observation was missing or stale.
	Transient bool `json:"transient,omitempty"`

	// QuotaState is the quota state the decision acted on, empty when the
	// provider carried no quota observation at all.
	QuotaState string `json:"quota_state,omitempty"`
	// QuotaUnknownReason explains a QuotaState of UNKNOWN, empty otherwise.
	QuotaUnknownReason string `json:"quota_unknown_reason,omitempty"`
	// QuotaObservedAt is when the quota observation was taken.
	QuotaObservedAt *time.Time `json:"quota_observed_at,omitempty"`
	// QuotaResetAt is the soonest reset among the quota windows the decision
	// read.
	QuotaResetAt *time.Time `json:"quota_reset_at,omitempty"`

	// HealthState is the actionable health state the decision acted on, empty
	// when the provider carried no health observation at all.
	HealthState string `json:"health_state,omitempty"`
	// HealthReason is the closed health reason behind the state.
	HealthReason string `json:"health_reason,omitempty"`
	// HealthObservedAt is when the health observation was taken.
	HealthObservedAt *time.Time `json:"health_observed_at,omitempty"`
	// CooldownUntil is when the provider's cooldown expires, when it has one.
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

// hardStages are the stages that decide eligibility. A candidate eliminated at
// one of them is not eligible; one eliminated at a preference stage was.
var hardStages = map[string]bool{
	"configuration":        true,
	"capabilities":         true,
	"model_availability":   true,
	"provider_health":      true,
	"subscription_quota":   true,
	"temporary_exclusions": true,
}

// evaluateCandidates assembles the evaluated candidate set of one decision.
//
// It covers every alias in the catalogue, because the selector looked at every
// one of them: an alias it dropped at the configuration stage was evaluated
// and rejected. Candidates the selector never reached do not appear — an
// invalid request evaluates nothing at all.
func (s Selector) evaluateCandidates(d Decision, state SelectState) []CandidateEvaluation {
	aliases := make([]string, 0, len(s.Catalogue.ModelAliases))
	for alias := range s.Catalogue.ModelAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	rejected := make(map[string]RejectedCandidate, len(d.Rejected))
	for _, r := range d.Rejected {
		rejected[r.Alias] = r
	}

	out := make([]CandidateEvaluation, 0, len(aliases))
	for i, alias := range aliases {
		m := s.Catalogue.ModelAliases[alias]
		ev := CandidateEvaluation{
			Alias:    alias,
			Provider: m.Provider(),
			Ordinal:  i + 1,
		}
		switch {
		case alias == d.SelectedAlias:
			ev.Selected = true
			ev.Eligible = true
			ev.ReasonCode = selectionCode(d)
			ev.Reason = d.PrimaryReason()
		default:
			if r, ok := rejected[alias]; ok {
				ev.EliminatedStage = r.Stage
				ev.ReasonCode = r.Code
				ev.Reason = r.Reason
				ev.Transient = r.Transient
				ev.Eligible = !hardStages[r.Stage]
			} else {
				ev.Eligible = true
				ev.ReasonCode = ReasonEligibleNotSelected
			}
		}
		fillObservations(&ev, state)
		out = append(out, ev)
	}
	return out
}

// selectionCode names the policy result that produced the chosen route.
func selectionCode(d Decision) ReasonCode {
	switch {
	case d.OverrideApplied:
		return ReasonSelectedByOverride
	case d.AffinityInfluenced:
		return ReasonSelectedByAffinity
	default:
		return ReasonSelectedByPreference
	}
}

// fillObservations copies the quota and health values the decision acted on.
// A provider with no observation keeps empty fields: absent is not zero and
// not healthy.
func fillObservations(ev *CandidateEvaluation, state SelectState) {
	if snap, ok := state.Quota[ev.Provider]; ok {
		st, reason := snap.StateAt(state.Now, quotaFreshness(state))
		ev.QuotaState = string(st)
		if reason != "" {
			ev.QuotaUnknownReason = string(reason)
		}
		if !snap.ObservedAt.IsZero() {
			t := snap.ObservedAt.UTC()
			ev.QuotaObservedAt = &t
		}
		if reset := soonestReset(snap.Windows); reset != nil {
			ev.QuotaResetAt = reset
		}
	}
	hs, ok := state.Health[ev.Provider]
	if !ok {
		return
	}
	health, reason := actionableHealth(hs, state.Now, state.HealthMaxAge)
	ev.HealthState = health
	ev.HealthReason = reason
	if !hs.ObservedAt.IsZero() {
		t := hs.ObservedAt.UTC()
		ev.HealthObservedAt = &t
	}
	if !hs.CooldownUntil.IsZero() {
		t := hs.CooldownUntil.UTC()
		ev.CooldownUntil = &t
	}
}

// actionableHealth is the health state a consumer may act on at now, with the
// reason behind it. A stale or unobserved observation is UNKNOWN, never the
// healthy-looking state it happened to carry.
func actionableHealth(hs quota.ProviderState, now time.Time, maxAge time.Duration) (string, string) {
	switch quota.ClassifyHealth(hs.ObservedAt, now, maxAge) {
	case quota.HealthNotObserved:
		return string(quota.HealthUnknown), string(quota.ReasonNotProbed)
	case quota.HealthStale:
		return string(quota.HealthUnknown), string(quota.StaleObservation)
	}
	return string(hs.Health), string(hs.Reason())
}

// soonestReset is the earliest reset among the quota windows, nil when no
// window reported one.
func soonestReset(ws []quota.Window) *time.Time {
	var out *time.Time
	for _, w := range ws {
		if w.ResetsAt.IsZero() {
			continue
		}
		t := w.ResetsAt.UTC()
		if out == nil || t.Before(*out) {
			out = &t
		}
	}
	return out
}
