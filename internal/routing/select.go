package routing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/settings"
)

// PolicyVersion identifies the selection policy. Decisions with identical
// inputs, state, and policy version must produce identical output.
const PolicyVersion = "routing.v1"

// Decision is the result of one routing selection. It is machine-readable and
// explains which stages eliminated candidates and what influenced the choice.
type Decision struct {
	// Request is the validated request that was evaluated.
	Request RouteRequest `json:"request"`
	// SelectedAlias is the chosen catalogue alias. Empty when no candidate is
	// eligible.
	SelectedAlias string `json:"selected_alias,omitempty"`
	// SelectedProvider is the provider of the selected alias. Empty when no
	// candidate is eligible.
	SelectedProvider string `json:"selected_provider,omitempty"`
	// DecisionID is a stable identifier derived from the decision inputs and
	// outcome. Identical inputs, state, and policy version produce the same ID.
	DecisionID string `json:"decision_id"`
	// PolicyVersion identifies the selection policy that produced this decision.
	PolicyVersion string `json:"policy_version"`
	// Reasons describes why the selected candidate was chosen.
	Reasons []string `json:"reasons,omitempty"`
	// Rejected lists candidates eliminated during selection, in evaluation order,
	// with the stage and reason for elimination.
	Rejected []RejectedCandidate `json:"rejected,omitempty"`
	// Stages records per-stage outcomes for explanation.
	Stages []StageOutcome `json:"stages,omitempty"`
	// Candidates is the evaluated candidate set in evaluation order: every
	// candidate the selector looked at, with the stable reason code that
	// decided its fate and the observations the hard checks read. It is empty
	// when the request was rejected before any candidate was evaluated.
	Candidates []CandidateEvaluation `json:"candidates,omitempty"`
	// AffinityInfluenced reports whether session affinity kept the selected
	// candidate.
	AffinityInfluenced bool `json:"affinity_influenced"`
	// ConfiguredPreferenceInfluenced reports whether configured provider/model
	// priority influenced the selected candidate.
	ConfiguredPreferenceInfluenced bool `json:"configured_preference_influenced"`
	// PerformanceInfluenced reports whether performance data influenced the
	// selected candidate among equal-priority candidates.
	PerformanceInfluenced bool `json:"performance_influenced"`
	// OverrideRequested reports whether the request carried a manual override.
	OverrideRequested bool `json:"override_requested"`
	// OverrideApplied reports whether the manual override selected the target.
	OverrideApplied bool `json:"override_applied"`
	// ConfiguredPriority identifies the configured provider/model priority that
	// influenced selection, when applicable.
	ConfiguredPriority *ConfiguredPriority `json:"configured_priority,omitempty"`
}

// RejectedCandidate records why a candidate was eliminated. Transient marks an
// elimination that carries no information about the route itself: an observation
// that is missing or stale, so the route's availability is unknown. A pinned
// session keeps its route through that instead of switching on an event that says
// nothing about the route (see Affinity).
//
// A provider cooldown is deliberately not transient. It is an explicit
// observation that the provider should receive no traffic, which is information
// about the route — holding a session on it would send the conversation's
// requests into the very rate limiting the cooldown exists to avoid.
type RejectedCandidate struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Stage    string `json:"stage"`
	// Code is the stable reason code of the elimination. The prose below is
	// for a human and may be reworded; the code is what a consumer branches on.
	Code      ReasonCode `json:"code"`
	Reason    string     `json:"reason"`
	Transient bool       `json:"transient,omitempty"`
}

// PrimaryReason is the one-line explanation of a decision: the first reason
// the selector recorded. The full list stays in the decision; this is the line
// an operator or a stored row reads first.
func (d Decision) PrimaryReason() string {
	if len(d.Reasons) == 0 {
		return ""
	}
	return d.Reasons[0]
}

// StageOutcome summarizes one decision stage.
type StageOutcome struct {
	Stage      string             `json:"stage"`
	Remaining  []string           `json:"remaining"`
	Eliminated []StageElimination `json:"eliminated,omitempty"`
	Note       string             `json:"note,omitempty"`
}

// StageElimination records one candidate eliminated at a stage.
type StageElimination struct {
	Alias  string `json:"alias"`
	Reason string `json:"reason"`
}

// ConfiguredPriority identifies the static priority that influenced selection.
// ModelPriority is omitted when only provider-level priority influenced selection.
type ConfiguredPriority struct {
	Provider         string `json:"provider"`
	ProviderPriority int    `json:"provider_priority"`
	ModelAlias       string `json:"model_alias,omitempty"`
	ModelPriority    *int   `json:"model_priority,omitempty"`
}

// Affinity records the current viable route for a session. It is honored only
// when the candidate still passes all hard constraints and the pin has not
// expired.
//
// ExpiresAt is the pin's lifetime: at or past it the affinity is not read at all
// and the decision is re-evaluated from the full candidate set, which is
// deterministic because it no longer depends on the pin. A zero ExpiresAt is an
// unbounded pin, which the selector refuses for the same reason a stored one
// cannot be unbounded: a route nobody renewed must stop pinning a session.
type Affinity struct {
	SessionID string
	Alias     string
	ExpiresAt time.Time
}

// PerformanceRecord is a measured performance observation for a candidate.
// Performance can only break ties among candidates with equal configured
// priority and only when fresh and statistically meaningful.
type PerformanceRecord struct {
	Alias string
	// Score is a comparable performance value where lower is better (for example,
	// latency in milliseconds). Only meaningful when Fresh and Sufficient are true.
	Score      float64
	Fresh      bool
	Sufficient bool
	ObservedAt time.Time
}

// Selector evaluates routing requests against configuration and runtime state.
type Selector struct {
	Catalogue settings.Catalogue
}

// Select chooses a viable model alias for req. The decision is deterministic
// for identical inputs, state, and policy version.
// DefaultQuotaMaxAge is used only when SelectState.QuotaMaxAge is not set.
// Callers should pass an explicit freshness bound derived from polling config;
// the fallback is conservative for direct selector use in tests/tools.
const DefaultQuotaMaxAge = 24 * time.Hour

func (s Selector) Select(req RouteRequest, state SelectState) Decision {
	state.Now = normalizeNow(state.Now)
	if state.QuotaMaxAge <= 0 {
		state.QuotaMaxAge = DefaultQuotaMaxAge
	}
	if state.HealthMaxAge <= 0 {
		state.HealthMaxAge = state.QuotaMaxAge
	}
	if err := req.Validate(); err != nil {
		// An invalid request is refused before any candidate is looked at, so
		// the decision carries no candidate set: nothing was considered.
		d := Decision{
			Request:           req,
			PolicyVersion:     PolicyVersion,
			OverrideRequested: req.Override != nil,
			Reasons:           []string{fmt.Sprintf("request invalid: %v", err)},
		}
		d.DecisionID = decisionID(req, state, d, s.Catalogue)
		return d
	}
	d := s.selectRoute(req, state)
	d.Candidates = s.evaluateCandidates(d, state)
	return d
}

// selectRoute runs the decision stages. Select wraps it, because the evaluated
// candidate set is assembled from what the stages decided and from the state
// they read.
func (s Selector) selectRoute(req RouteRequest, state SelectState) Decision {
	d := Decision{
		Request:           req,
		PolicyVersion:     PolicyVersion,
		OverrideRequested: req.Override != nil,
	}

	candidates, configElim := s.initialCandidates()
	d.addEliminations(&candidates, configElim)
	d.recordStage("configuration", candidates, configElim, "")

	candidates, elim := filterCapabilities(candidates, req)
	d.addEliminations(&candidates, elim)
	d.recordStage("capabilities", candidates, elim, "")

	// A configured local-time window is a hard constraint like a capability, so
	// it is applied here — before the pin is resolved and before any preference
	// ranking. Affinity and manual override both read this candidate set, so
	// neither can reach a model inside an active window.
	candidates, elim = filterAvailability(candidates, state)
	d.addEliminations(&candidates, elim)
	d.recordStage("model_availability", candidates, elim, "")

	// The pin is resolved here, after the durable stages and before the
	// transient ones. A route excluded only by a missing, stale or temporarily
	// cooled observation is still the session's route, and this is the point
	// where that distinction is still recoverable.
	affinityCandidate, affinityApplies := selectAffinity(candidates, req, state)

	candidates, elim = filterHealth(candidates, state)
	d.addEliminations(&candidates, elim)
	d.recordStage("provider_health", candidates, elim, "")

	candidates, elim = filterQuota(candidates, s.Catalogue, req, state)
	d.addEliminations(&candidates, elim)
	d.recordStage("subscription_quota", candidates, elim, "")

	candidates, elim = filterCooldown(candidates, state)
	d.addEliminations(&candidates, elim)
	d.recordStage("temporary_exclusions", candidates, elim, "")

	// Manual override is explicit and auditable, but bounded by hard
	// constraints. Evaluate after hard constraints so it cannot bypass them.
	if req.Override != nil {
		target := strings.TrimSpace(req.Override.ModelAlias)
		if containsAlias(candidates, target) {
			selected := candidateByAlias(candidates, target)
			d.SelectedAlias = selected.Alias
			d.SelectedProvider = selected.Provider
			d.OverrideApplied = true
			d.Reasons = append(d.Reasons, fmt.Sprintf("manual override selected %q", target))
			if cp, ok := s.configuredPriority(selected.Alias); ok {
				d.ConfiguredPriority = &cp
			}
			overrideRejected := overrideEliminations(candidates, target)
			d.recordStage("manual_override", []candidate{selected}, overrideRejected, "manual override applied after hard constraints")
			d.Rejected = append(d.Rejected, overrideRejected...)
			d.DecisionID = decisionID(req, state, d, s.Catalogue)
			return d
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf("manual override %q rejected: target is not eligible after hard constraints", target))
		d.recordStage("manual_override", candidates, nil, "override target ineligible; continuing with normal selection")
	}

	if affinityApplies {
		// The pin decides unless its route is gone for a durable reason: the
		// exclusion then says something about the route, not about the
		// observation, so the session has to move. A transient exclusion says
		// only that this installation cannot currently see the route.
		held, why := false, ""
		if !containsAlias(candidates, affinityCandidate.Alias) {
			held, why = heldThroughTransient(d.Rejected, affinityCandidate.Alias)
		}
		if containsAlias(candidates, affinityCandidate.Alias) || held {
			d.SelectedAlias = affinityCandidate.Alias
			d.SelectedProvider = affinityCandidate.Provider
			d.AffinityInfluenced = true
			note := "affinity retained"
			if held {
				// Switching here would rebuild the next model's context over an
				// event that carries no information about the route, which is
				// exactly the mid-turn switch this contract forbids. The pin's
				// expiry bounds how long that patience lasts; a durable exclusion
				// ends it immediately.
				note = "affinity held through transient exclusions"
				d.Reasons = append(d.Reasons, fmt.Sprintf("session affinity retained route %q held through transient exclusions: %s", affinityCandidate.Alias, why))
			} else {
				d.Reasons = append(d.Reasons, fmt.Sprintf("session affinity retained viable route %q", affinityCandidate.Alias))
			}
			if cp, ok := s.configuredPriority(affinityCandidate.Alias); ok {
				d.ConfiguredPriority = &cp
			}
			affinityRejected := affinityEliminations(candidates, affinityCandidate.Alias)
			d.recordStage("session_affinity", []candidate{affinityCandidate}, affinityRejected, note)
			d.Rejected = append(d.Rejected, affinityRejected...)
			d.DecisionID = decisionID(req, state, d, s.Catalogue)
			return d
		}
	}
	d.recordStage("session_affinity", candidates, nil, affinityStageNote(req, state, affinityApplies, d.Rejected))

	ranked, prefInfluenced, perfInfluenced, rankReasons := s.rankCandidates(candidates, req, state)
	if len(ranked) == 0 {
		d.Reasons = append(d.Reasons, "no eligible candidate after all decision stages")
		d.DecisionID = decisionID(req, state, d, s.Catalogue)
		return d
	}

	selected := ranked[0]
	d.SelectedAlias = selected.Alias
	d.SelectedProvider = selected.Provider
	d.ConfiguredPreferenceInfluenced = prefInfluenced
	d.PerformanceInfluenced = perfInfluenced
	d.Reasons = append(d.Reasons, rankReasons...)
	if cp, ok := s.configuredPriority(selected.Alias); ok {
		d.ConfiguredPriority = &cp
	}
	d.recordStage("configured_preference", ranked, nil, "ranked by configured preference")
	d.recordStage("performance_preference", ranked, nil, performanceStageNote(perfInfluenced))
	d.recordStage("tie_break", ranked, nil, "stable alias ordering")
	d.DecisionID = decisionID(req, state, d, s.Catalogue)
	return d
}

// SelectState carries runtime state needed for selection. It is caller-owned;
// the selector does not mutate it.
type SelectState struct {
	// Now is the evaluation time. Zero means time.Now().UTC().
	Now time.Time
	// Quota holds latest quota snapshots keyed by provider.
	Quota map[string]quota.Snapshot
	// Health holds latest provider health keyed by provider.
	Health map[string]quota.ProviderState
	// Affinity holds current session affinity, if any.
	Affinity *Affinity
	// Performance holds optional performance records keyed by alias.
	Performance map[string]PerformanceRecord
	// QuotaMaxAge bounds how old a quota snapshot may be to be acted on. Zero
	// fails closed: quota.StateAt treats non-positive max age as unknown. The
	// server sets this from polling interval (3*interval); direct callers should
	// set it explicitly.
	QuotaMaxAge time.Duration
	// HealthMaxAge bounds how old a provider health observation may be to be
	// acted on. Zero defaults to QuotaMaxAge.
	HealthMaxAge time.Duration
	// MaxPerformanceAge bounds performance freshness. Zero disables performance
	// influence.
	MaxPerformanceAge time.Duration
}

type candidate struct {
	Alias    string
	Provider string
	Model    settings.Model
}

func normalizeNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func (s Selector) initialCandidates() ([]candidate, []RejectedCandidate) {
	aliases := make([]string, 0, len(s.Catalogue.ModelAliases))
	for alias := range s.Catalogue.ModelAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	out := make([]candidate, 0, len(aliases))
	var elim []RejectedCandidate
	for _, alias := range aliases {
		m := s.Catalogue.ModelAliases[alias]
		if !m.IsEnabled() {
			elim = append(elim, RejectedCandidate{Alias: alias, Provider: m.Provider(), Stage: "configuration", Code: ReasonModelDisabled, Reason: "model disabled"})
			continue
		}
		out = append(out, candidate{Alias: alias, Provider: m.Provider(), Model: m})
	}
	return out, elim
}

func (d *Decision) addEliminations(candidates *[]candidate, elim []RejectedCandidate) {
	if len(elim) == 0 {
		return
	}
	d.Rejected = append(d.Rejected, elim...)
	kept := make([]candidate, 0, len(*candidates))
	rejected := map[string]bool{}
	for _, e := range elim {
		rejected[e.Alias] = true
	}
	for _, c := range *candidates {
		if !rejected[c.Alias] {
			kept = append(kept, c)
		}
	}
	*candidates = kept
}

func (d *Decision) recordStage(stage string, remaining []candidate, elim []RejectedCandidate, note string) {
	out := StageOutcome{Stage: stage, Note: note, Remaining: aliasesOf(remaining)}
	for _, e := range elim {
		out.Eliminated = append(out.Eliminated, StageElimination{Alias: e.Alias, Reason: e.Reason})
	}
	d.Stages = append(d.Stages, out)
}

func aliasesOf(cs []candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Alias)
	}
	return out
}
