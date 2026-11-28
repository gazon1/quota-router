package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/config"
	"github.com/gazon1/quota-router/internal/quota"
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
	Alias     string `json:"alias"`
	Provider  string `json:"provider"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
	Transient bool   `json:"transient,omitempty"`
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
	Config config.Config
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
	d := Decision{
		Request:           req,
		PolicyVersion:     PolicyVersion,
		OverrideRequested: req.Override != nil,
	}
	if err := req.Validate(); err != nil {
		d.Reasons = []string{fmt.Sprintf("request invalid: %v", err)}
		d.DecisionID = decisionID(req, state, d, s.Config)
		return d
	}

	candidates, configElim := s.initialCandidates()
	d.addEliminations(&candidates, configElim)
	d.recordStage("configuration", candidates, configElim, "")

	candidates, elim := filterCapabilities(candidates, s.Config, req)
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

	candidates, elim = filterHealth(candidates, s.Config, state)
	d.addEliminations(&candidates, elim)
	d.recordStage("provider_health", candidates, elim, "")

	candidates, elim = filterQuota(candidates, s.Config, req, state)
	d.addEliminations(&candidates, elim)
	d.recordStage("subscription_quota", candidates, elim, "")

	candidates, elim = filterCooldown(candidates, s.Config, state)
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
			d.DecisionID = decisionID(req, state, d, s.Config)
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
			d.DecisionID = decisionID(req, state, d, s.Config)
			return d
		}
	}
	d.recordStage("session_affinity", candidates, nil, affinityStageNote(req, state, affinityApplies, d.Rejected))

	ranked, prefInfluenced, perfInfluenced, rankReasons := s.rankCandidates(candidates, req, state)
	if len(ranked) == 0 {
		d.Reasons = append(d.Reasons, "no eligible candidate after all decision stages")
		d.DecisionID = decisionID(req, state, d, s.Config)
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
	d.DecisionID = decisionID(req, state, d, s.Config)
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
	Model    config.Model
}

func normalizeNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func (s Selector) initialCandidates() ([]candidate, []RejectedCandidate) {
	aliases := make([]string, 0, len(s.Config.LiteLLM.ModelAliases))
	for alias := range s.Config.LiteLLM.ModelAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	out := make([]candidate, 0, len(aliases))
	var elim []RejectedCandidate
	for _, alias := range aliases {
		m := s.Config.LiteLLM.ModelAliases[alias]
		if !m.IsEnabled() {
			elim = append(elim, RejectedCandidate{Alias: alias, Provider: m.Provider(), Stage: "configuration", Reason: "model disabled"})
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

func filterCapabilities(cs []candidate, cfg config.Config, req RouteRequest) ([]candidate, []RejectedCandidate) {
	required := requirementsFromRequest(req)
	var elim []RejectedCandidate
	for _, c := range cs {
		if !required.SatisfiedBy(c.Model) {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "capabilities", Reason: capabilityReason(required, c.Model)})
		}
	}
	return cs, elim
}

func requirementsFromRequest(req RouteRequest) config.Requirements {
	r := config.Requirements{}
	if req.Capabilities != nil {
		r.Streaming = req.Capabilities.Streaming
		r.ToolCalling = req.Capabilities.ToolCalling
		r.StructuredOutput = req.Capabilities.StructuredOutput
	}
	if req.TaskClass != "" {
		r.Tasks = []string{string(req.TaskClass)}
	}
	return r
}

func capabilityReason(req config.Requirements, m config.Model) string {
	var missing []string
	if req.Streaming && !m.Capabilities.Streaming.Supported() {
		missing = append(missing, "streaming")
	}
	if req.ToolCalling && !m.Capabilities.ToolCalling.Supported() {
		missing = append(missing, "tool_calling")
	}
	if req.StructuredOutput && !m.Capabilities.StructuredOutput.Supported() {
		missing = append(missing, "structured_output")
	}
	for _, want := range req.Tasks {
		ok := false
		for _, have := range m.Tasks {
			if have == want {
				ok = true
				break
			}
		}
		if !ok {
			missing = append(missing, "task:"+want)
		}
	}
	if len(missing) == 0 {
		return "capability requirements not satisfied"
	}
	return "missing required capabilities: " + strings.Join(missing, ", ")
}

// filterAvailability excludes a model inside its configured recurring
// unavailable window. The window is the operator's explicit "do not send traffic
// here now", evaluated at the decision's own instant, so a model is ineligible
// for exactly as long as it holds and needs no background job to come back: the
// next decision reads the clock again.
//
// The rejection is durable for affinity's purposes. Holding a session on a model
// that policy says must not receive traffic would send the whole conversation
// into the very window the schedule exists to avoid.
func filterAvailability(cs []candidate, state SelectState) ([]candidate, []RejectedCandidate) {
	var elim []RejectedCandidate
	for _, c := range cs {
		w, ok := c.Model.Unavailable.WindowAt(state.Now)
		if !ok {
			continue
		}
		elim = append(elim, RejectedCandidate{
			Alias:    c.Alias,
			Provider: c.Provider,
			Stage:    "model_availability",
			Reason:   fmt.Sprintf("model unavailable by configured schedule (%s %s): %s", c.Model.Unavailable.Timezone, w, state.Now.UTC().Format(time.RFC3339)),
		})
	}
	return cs, elim
}

func filterHealth(cs []candidate, cfg config.Config, state SelectState) ([]candidate, []RejectedCandidate) {
	var elim []RejectedCandidate
	for _, c := range cs {
		h, ok := state.Health[c.Provider]
		if !ok {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: "provider health unknown", Transient: true})
			continue
		}
		if h.ObservedAt.IsZero() {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: "provider health not observed", Transient: true})
			continue
		}
		age := state.Now.Sub(h.ObservedAt.UTC())
		if age < 0 || age > state.HealthMaxAge {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: "provider health stale", Transient: true})
			continue
		}
		if h.Health == quota.Unavailable {
			reason := "provider unavailable"
			if r := h.Reason(); r != "" {
				reason += ": " + string(r)
			}
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: reason})
			continue
		}
		if h.Health == quota.HealthUnknown {
			reason := "provider health unknown"
			if r := h.Reason(); r != "" {
				reason += ": " + string(r)
			}
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: reason, Transient: true})
			continue
		}
		// Degraded is allowed by explicit policy: it is not unavailable. The
		// reason is kept in health state for explanation, but it does not
		// eliminate the candidate.
		_ = cfg
	}
	return cs, elim
}

func filterQuota(cs []candidate, cfg config.Config, req RouteRequest, state SelectState) ([]candidate, []RejectedCandidate) {
	var elim []RejectedCandidate
	for _, c := range cs {
		snap, ok := state.Quota[c.Provider]
		if !ok {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: "quota unknown: not observed", Transient: true})
			continue
		}
		st, reason := snap.StateAt(state.Now, quotaFreshness(state))
		switch st {
		case quota.Exhausted:
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: "subscription quota exhausted"})
		case quota.QuotaUnknown:
			r := "quota unknown"
			if reason != "" {
				r += ": " + string(reason)
			}
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: r, Transient: true})
		case quota.LowQuota, quota.Available:
			if reserveApplies(req, cfg) && belowReserve(snap, cfg.Routing.ReserveFraction) {
				elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: fmt.Sprintf("remaining quota reserved for %s workload (reserve %.0f%%)", req.TaskClass, cfg.Routing.ReserveFraction*100)})
			}
		default:
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: fmt.Sprintf("quota state %q not eligible", st)})
		}
	}
	return cs, elim
}

func quotaFreshness(state SelectState) time.Duration {
	// Zero fails closed via quota.StateAt: an unset freshness bound must not
	// accept stale quota as available.
	return state.QuotaMaxAge
}

func reserveApplies(req RouteRequest, cfg config.Config) bool {
	if cfg.Routing.ReserveFraction <= 0 {
		return false
	}
	switch req.TaskClass {
	case TaskCoding, TaskReasoning:
		return true
	default:
		return false
	}
}

func belowReserve(snap quota.Snapshot, reserve float64) bool {
	if reserve <= 0 {
		return false
	}
	for _, w := range snap.Windows {
		if w.Remaining == nil || w.Limit == nil || *w.Limit <= 0 {
			continue
		}
		if *w.Remaining <= *w.Limit*reserve {
			return true
		}
	}
	return false
}

// filterCooldown excludes a provider that routing policy has cooled down. The
// rejection is durable for affinity's purposes even though the cooldown itself
// expires: a cooldown is an explicit decision that this provider should receive
// no traffic, not an absence of information about it. Holding a session on a
// cooled provider would send a whole conversation's requests into the rate
// limiting the cooldown exists to avoid.
func filterCooldown(cs []candidate, _ config.Config, state SelectState) ([]candidate, []RejectedCandidate) {
	var elim []RejectedCandidate
	for _, c := range cs {
		h, ok := state.Health[c.Provider]
		if !ok {
			continue
		}
		if h.CooldownActive(state.Now) {
			until := h.CooldownUntil.UTC().Format(time.RFC3339)
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "temporary_exclusions", Reason: "provider in cooldown until " + until})
		}
	}
	return cs, elim
}

func containsAlias(cs []candidate, alias string) bool {
	for _, c := range cs {
		if c.Alias == alias {
			return true
		}
	}
	return false
}

func candidateByAlias(cs []candidate, alias string) candidate {
	for _, c := range cs {
		if c.Alias == alias {
			return c
		}
	}
	return candidate{}
}

func overrideEliminations(cs []candidate, selected string) []RejectedCandidate {
	var elim []RejectedCandidate
	for _, c := range cs {
		if c.Alias == selected {
			continue
		}
		elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "manual_override", Reason: "not selected by manual override"})
	}
	return elim
}

// selectAffinity reports the candidate a session's pin names, if the pin applies
// at all. It answers "is this the session's route", not "is it viable": the
// caller decides that, because a route excluded for a transient reason is still
// the session's route and switching away from it is the thing this issue forbids.
//
// A pin is refused when it names another session, has no alias, carries no
// expiry (an unbounded pin outlives the route it names), or has expired.
func selectAffinity(cs []candidate, req RouteRequest, state SelectState) (candidate, bool) {
	if state.Affinity == nil {
		return candidate{}, false
	}
	if state.Affinity.SessionID == "" || state.Affinity.SessionID != req.SessionID {
		return candidate{}, false
	}
	if state.Affinity.Alias == "" {
		return candidate{}, false
	}
	if !state.Affinity.ExpiresAt.After(state.Now) {
		return candidate{}, false
	}
	for _, c := range cs {
		if c.Alias == state.Affinity.Alias {
			return c, true
		}
	}
	return candidate{}, false
}

// heldThroughTransient reports whether the route the session is pinned to was
// eliminated only by transient stages. It reads the eliminations already
// recorded, so a stage added later is treated as durable until it says
// otherwise: switching on an unexplained exclusion would leave this decision
// copying the same kind of mistake the transient flag exists to prevent.
func heldThroughTransient(rejected []RejectedCandidate, alias string) (bool, string) {
	var why []string
	for _, r := range rejected {
		if r.Alias != alias {
			continue
		}
		if !r.Transient {
			return false, ""
		}
		why = append(why, r.Reason)
	}
	if len(why) == 0 {
		return false, ""
	}
	return true, strings.Join(why, "; ")
}

// affinityStageNote explains why the pin did not decide this decision, so the
// audit record names the actual reason instead of an ambiguous "no match". A pin
// excluded before it was even resolved — a disabled alias, a capability the
// catalogue no longer claims — is named from its own rejection, because falling
// back to "no viable affinity match" would hide the exclusion an operator needs
// to see.
func affinityStageNote(req RouteRequest, state SelectState, applies bool, rejected []RejectedCandidate) string {
	switch {
	case state.Affinity == nil:
		return "no affinity stored for this session"
	case state.Affinity.SessionID != req.SessionID:
		return "stored affinity belongs to another session"
	case state.Affinity.Alias == "":
		return "stored affinity names no route"
	case !state.Affinity.ExpiresAt.After(state.Now):
		return fmt.Sprintf("affinity for %q expired at %s; re-evaluated from the full candidate set", state.Affinity.Alias, state.Affinity.ExpiresAt.UTC().Format(time.RFC3339))
	}
	// A pin excluded before resolution never becomes an affinity candidate, so
	// its elimination is the only place the reason exists.
	if !applies {
		if r, ok := rejectedCandidate(rejected, state.Affinity.Alias); ok {
			return fmt.Sprintf("affinity route %q excluded at %s: %s; session must move", r.Alias, r.Stage, r.Reason)
		}
		return "no viable affinity match"
	}
	return fmt.Sprintf("affinity route %q eliminated by a durable exclusion; session must move", state.Affinity.Alias)
}

func rejectedCandidate(cs []RejectedCandidate, alias string) (RejectedCandidate, bool) {
	for _, r := range cs {
		if r.Alias == alias {
			return r, true
		}
	}
	return RejectedCandidate{}, false
}

func affinityEliminations(cs []candidate, selected string) []RejectedCandidate {
	var elim []RejectedCandidate
	for _, c := range cs {
		if c.Alias == selected {
			continue
		}
		elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "session_affinity", Reason: "session affinity retained another viable route"})
	}
	return elim
}

func (s Selector) rankCandidates(cs []candidate, req RouteRequest, state SelectState) ([]candidate, bool, bool, []string) {
	if len(cs) == 0 {
		return nil, false, false, nil
	}
	type ranked struct {
		c               candidate
		providerRank    int
		modelRank       int
		hasProviderPref bool
		hasModelPref    bool
		prefRank        int
		perfScore       float64
		hasPerf         bool
	}
	rankedList := make([]ranked, 0, len(cs))
	hasConfiguredPref := len(s.Config.Routing.Providers) > 0
	for _, c := range cs {
		r := ranked{c: c, providerRank: int(^uint(0) >> 1), modelRank: int(^uint(0) >> 1)}
		if rp, ok := s.Config.Routing.Providers[c.Provider]; ok {
			r.providerRank = rp.Priority
			r.hasProviderPref = true
			if mp, ok := rp.Models[c.Alias]; ok {
				r.modelRank = mp
				r.hasModelPref = true
			}
		}
		if rec, ok := usablePerformance(state, c.Alias); ok {
			r.perfScore = rec.Score
			r.hasPerf = true
		}
		r.prefRank = preferenceRank(req.Preference, c)
		rankedList = append(rankedList, r)
	}

	sort.SliceStable(rankedList, func(i, j int) bool {
		a, b := rankedList[i], rankedList[j]
		if a.providerRank != b.providerRank {
			return a.providerRank < b.providerRank
		}
		// Provider priorities tie: break ties by provider name deterministically,
		// then apply model priority only within the same provider. Model priorities
		// are scoped to a provider and must not be compared across providers (#53).
		if a.c.Provider != b.c.Provider {
			return a.c.Provider < b.c.Provider
		}
		if a.modelRank != b.modelRank {
			return a.modelRank < b.modelRank
		}
		// Request preference is a soft signal: it cannot override configured
		// priority and is evaluated only among equal-priority candidates.
		if a.prefRank != b.prefRank {
			return a.prefRank < b.prefRank
		}
		// Performance is only a tie-break among candidates with equal configured
		// priority (same provider, same model priority scope) and equal request
		// preference rank.
		if a.hasPerf != b.hasPerf {
			return a.hasPerf
		}
		if a.hasPerf && b.hasPerf && a.perfScore != b.perfScore {
			return a.perfScore < b.perfScore
		}
		return a.c.Alias < b.c.Alias
	})

	out := make([]candidate, 0, len(rankedList))
	for _, r := range rankedList {
		out = append(out, r.c)
	}

	prefInfluenced := hasConfiguredPref && (rankedList[0].hasProviderPref || rankedList[0].hasModelPref)
	perfInfluenced := false
	if len(rankedList) > 1 {
		a, b := rankedList[0], rankedList[1]
		sameConfiguredScope := a.providerRank == b.providerRank && a.c.Provider == b.c.Provider && a.modelRank == b.modelRank
		if sameConfiguredScope && a.prefRank == b.prefRank && a.hasPerf && (!b.hasPerf || a.perfScore < b.perfScore) {
			perfInfluenced = true
		}
	}

	reasons := make([]string, 0, 3)
	if prefInfluenced {
		if rankedList[0].hasModelPref {
			reasons = append(reasons, fmt.Sprintf("configured priority selected provider %q (priority %d) model %q (priority %d)", rankedList[0].c.Provider, rankedList[0].providerRank, rankedList[0].c.Alias, rankedList[0].modelRank))
		} else {
			reasons = append(reasons, fmt.Sprintf("configured priority selected provider %q (priority %d)", rankedList[0].c.Provider, rankedList[0].providerRank))
		}
	} else {
		reasons = append(reasons, "selected by deterministic stable ordering")
	}
	if perfInfluenced {
		reasons = append(reasons, fmt.Sprintf("performance broke tie among equal-priority candidates for %q", rankedList[0].c.Alias))
	}
	if req.Preference != nil {
		reasons = append(reasons, preferenceReason(req.Preference, rankedList[0].c))
	}
	return out, prefInfluenced, perfInfluenced, reasons
}

func usablePerformance(state SelectState, alias string) (PerformanceRecord, bool) {
	if state.MaxPerformanceAge <= 0 {
		return PerformanceRecord{}, false
	}
	rec, ok := state.Performance[alias]
	if !ok || !rec.Fresh || !rec.Sufficient {
		return PerformanceRecord{}, false
	}
	if rec.ObservedAt.IsZero() {
		return PerformanceRecord{}, false
	}
	age := state.Now.Sub(rec.ObservedAt.UTC())
	if age < 0 || age > state.MaxPerformanceAge {
		return PerformanceRecord{}, false
	}
	return rec, true
}

func preferenceReason(pref *RoutePreference, selected candidate) string {
	if pref == nil {
		return "no request preference"
	}
	if p := strings.TrimSpace(pref.ModelAlias); p != "" {
		if p == selected.Alias {
			return fmt.Sprintf("request model preference %q satisfied", p)
		}
		return fmt.Sprintf("request model preference %q not selected; selected %q", p, selected.Alias)
	}
	if p := strings.TrimSpace(pref.Provider); p != "" {
		if p == selected.Provider {
			return fmt.Sprintf("request provider preference %q satisfied", p)
		}
		return fmt.Sprintf("request provider preference %q not selected; selected provider %q", p, selected.Provider)
	}
	return "request preference present"
}

func preferenceRank(pref *RoutePreference, c candidate) int {
	if pref == nil {
		return 2
	}
	if p := strings.TrimSpace(pref.ModelAlias); p != "" && p == c.Alias {
		return 0
	}
	if p := strings.TrimSpace(pref.Provider); p != "" && p == c.Provider {
		return 1
	}
	return 2
}

func (s Selector) configuredPriority(alias string) (ConfiguredPriority, bool) {
	m, ok := s.Config.LiteLLM.ModelAliases[alias]
	if !ok {
		return ConfiguredPriority{}, false
	}
	provider := m.Provider()
	rp, ok := s.Config.Routing.Providers[provider]
	if !ok {
		return ConfiguredPriority{}, false
	}
	cp := ConfiguredPriority{Provider: provider, ProviderPriority: rp.Priority}
	if mp, ok := rp.Models[alias]; ok {
		cp.ModelAlias = alias
		mp := mp
		cp.ModelPriority = &mp
	}
	return cp, true
}

func performanceStageNote(influenced bool) string {
	if influenced {
		return "fresh sufficient performance used as equal-priority tie-break"
	}
	return "performance did not override configured preference; stale/sparse data ignored"
}

func decisionID(req RouteRequest, state SelectState, d Decision, cfg config.Config) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write(PolicyVersion)
	write(configFingerprint(cfg))
	write(fmt.Sprintf("%d", req.Version), req.SessionID, string(req.TaskClass))
	if req.Capabilities != nil {
		write(fmt.Sprintf("%t", req.Capabilities.Streaming), fmt.Sprintf("%t", req.Capabilities.ToolCalling), fmt.Sprintf("%t", req.Capabilities.StructuredOutput))
	} else {
		write("no-capabilities")
	}
	if req.Preference != nil {
		write(req.Preference.Provider, req.Preference.ModelAlias)
	} else {
		write("no-preference")
	}
	if req.Override != nil {
		write(req.Override.ModelAlias)
	} else {
		write("no-override")
	}
	if state.Affinity != nil {
		write(state.Affinity.SessionID, state.Affinity.Alias)
	} else {
		write("no-affinity")
	}
	write("now", state.Now.UTC().Format(time.RFC3339Nano))
	write("quota_max_age", state.QuotaMaxAge.String())
	write("health_max_age", state.HealthMaxAge.String())
	write("max_perf_age", state.MaxPerformanceAge.String())
	quotaKeys := make([]string, 0, len(state.Quota))
	for k := range state.Quota {
		quotaKeys = append(quotaKeys, k)
	}
	sort.Strings(quotaKeys)
	for _, k := range quotaKeys {
		snap := state.Quota[k]
		st, reason := snap.StateAt(state.Now, quotaFreshness(state))
		write("quota", k, string(st), string(reason))
	}
	healthKeys := make([]string, 0, len(state.Health))
	for k := range state.Health {
		healthKeys = append(healthKeys, k)
	}
	sort.Strings(healthKeys)
	for _, k := range healthKeys {
		hs := state.Health[k]
		write("health", k, string(hs.Health), string(hs.Reason()), hs.CooldownUntil.UTC().Format(time.RFC3339Nano))
	}
	perfKeys := make([]string, 0, len(state.Performance))
	for k := range state.Performance {
		perfKeys = append(perfKeys, k)
	}
	sort.Strings(perfKeys)
	for _, k := range perfKeys {
		p := state.Performance[k]
		write("perf", k, fmt.Sprintf("%t", p.Fresh), fmt.Sprintf("%t", p.Sufficient), fmt.Sprintf("%.6f", p.Score), p.ObservedAt.UTC().Format(time.RFC3339Nano))
	}
	write("selected", d.SelectedAlias, d.SelectedProvider)
	for _, r := range d.Rejected {
		write("rej", r.Alias, r.Provider, r.Stage, r.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func configFingerprint(cfg config.Config) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write("routing.low_quota_threshold", fmt.Sprintf("%.17g", cfg.Routing.LowQuotaThreshold))
	write("routing.reserve_fraction", fmt.Sprintf("%.17g", cfg.Routing.ReserveFraction))

	aliasKeys := make([]string, 0, len(cfg.LiteLLM.ModelAliases))
	for k := range cfg.LiteLLM.ModelAliases {
		aliasKeys = append(aliasKeys, k)
	}
	sort.Strings(aliasKeys)
	for _, alias := range aliasKeys {
		m := cfg.LiteLLM.ModelAliases[alias]
		write("model", alias, m.Model, m.Provider(), fmt.Sprintf("%t", m.IsEnabled()), fmt.Sprintf("%d", m.ContextLimit))
		write("cap", string(m.Capabilities.Streaming), string(m.Capabilities.ToolCalling), string(m.Capabilities.StructuredOutput))
		tasks := append([]string(nil), m.Tasks...)
		sort.Strings(tasks)
		write("tasks", strings.Join(tasks, ","))
		if u := m.Unavailable; u != nil {
			write("unavailable", u.Timezone, fmt.Sprintf("%d", len(u.Windows)))
			for _, w := range u.Windows {
				days := append([]string(nil), w.Days...)
				sort.Strings(days)
				write("window", strings.Join(days, ","), w.Start, w.End)
			}
		}
	}

	providerKeys := make([]string, 0, len(cfg.Routing.Providers))
	for k := range cfg.Routing.Providers {
		providerKeys = append(providerKeys, k)
	}
	sort.Strings(providerKeys)
	for _, p := range providerKeys {
		rp := cfg.Routing.Providers[p]
		write("provider", p, fmt.Sprintf("%d", rp.Priority))
		modelKeys := make([]string, 0, len(rp.Models))
		for k := range rp.Models {
			modelKeys = append(modelKeys, k)
		}
		sort.Strings(modelKeys)
		for _, a := range modelKeys {
			write("provider-model", p, a, fmt.Sprintf("%d", rp.Models[a]))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
