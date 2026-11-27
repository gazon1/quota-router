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

// RejectedCandidate records why a candidate was eliminated.
type RejectedCandidate struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Stage    string `json:"stage"`
	Reason   string `json:"reason"`
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
type ConfiguredPriority struct {
	Provider         string `json:"provider"`
	ProviderPriority int    `json:"provider_priority"`
	ModelAlias       string `json:"model_alias"`
	ModelPriority    int    `json:"model_priority"`
}

// Affinity records the current viable route for a session. It is honored only
// when the candidate still passes all hard constraints.
type Affinity struct {
	SessionID string
	Alias     string
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
// DefaultQuotaMaxAge is used when SelectState.QuotaMaxAge is not set. It keeps
// selection conservative without requiring every caller to plumb polling config.
// Snapshots marked stale or dated implausibly in the future are still rejected
// by quota.Snapshot.StateAt.
const DefaultQuotaMaxAge = 24 * time.Hour

func (s Selector) Select(req RouteRequest, state SelectState) Decision {
	state.Now = normalizeNow(state.Now)
	if state.QuotaMaxAge <= 0 {
		state.QuotaMaxAge = DefaultQuotaMaxAge
	}
	d := Decision{
		Request:           req,
		PolicyVersion:     PolicyVersion,
		OverrideRequested: req.Override != nil,
	}
	if err := req.Validate(); err != nil {
		d.Reasons = []string{fmt.Sprintf("request invalid: %v", err)}
		d.DecisionID = decisionID(req, state, d)
		return d
	}

	candidates, configElim := s.initialCandidates()
	d.addEliminations(&candidates, configElim)
	d.recordStage("configuration", candidates, configElim, "")

	candidates, elim := filterCapabilities(candidates, s.Config, req)
	d.addEliminations(&candidates, elim)
	d.recordStage("capabilities", candidates, elim, "")

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
			d.recordStage("manual_override", []candidate{selected}, overrideEliminations(candidates, target), "manual override applied after hard constraints")
			d.DecisionID = decisionID(req, state, d)
			return d
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf("manual override %q rejected: target is not eligible after hard constraints", target))
		d.recordStage("manual_override", candidates, nil, "override target ineligible; continuing with normal selection")
	}

	if selected, ok := selectAffinity(candidates, req, state); ok {
		d.SelectedAlias = selected.Alias
		d.SelectedProvider = selected.Provider
		d.AffinityInfluenced = true
		d.Reasons = append(d.Reasons, fmt.Sprintf("session affinity retained viable route %q", selected.Alias))
		if cp, ok := s.configuredPriority(selected.Alias); ok {
			d.ConfiguredPriority = &cp
		}
		d.recordStage("session_affinity", []candidate{selected}, affinityEliminations(candidates, selected.Alias), "affinity retained")
		d.DecisionID = decisionID(req, state, d)
		return d
	}
	d.recordStage("session_affinity", candidates, nil, "no viable affinity match")

	ranked, prefInfluenced, perfInfluenced, rankReasons := s.rankCandidates(candidates, req, state)
	if len(ranked) == 0 {
		d.Reasons = append(d.Reasons, "no eligible candidate after all decision stages")
		d.DecisionID = decisionID(req, state, d)
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
	d.DecisionID = decisionID(req, state, d)
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
	// fails closed: quota.StateAt treats non-positive max age as unknown.
	QuotaMaxAge time.Duration
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

func filterHealth(cs []candidate, cfg config.Config, state SelectState) ([]candidate, []RejectedCandidate) {
	var elim []RejectedCandidate
	for _, c := range cs {
		h, ok := state.Health[c.Provider]
		if !ok {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: "provider health unknown"})
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
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: reason})
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
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: "quota unknown: not observed"})
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
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "subscription_quota", Reason: r})
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

func selectAffinity(cs []candidate, req RouteRequest, state SelectState) (candidate, bool) {
	if state.Affinity == nil {
		return candidate{}, false
	}
	if state.Affinity.SessionID != "" && state.Affinity.SessionID != req.SessionID {
		return candidate{}, false
	}
	if state.Affinity.Alias == "" {
		return candidate{}, false
	}
	for _, c := range cs {
		if c.Alias == state.Affinity.Alias {
			return c, true
		}
	}
	return candidate{}, false
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
		if a.modelRank != b.modelRank {
			return a.modelRank < b.modelRank
		}
		// Request preference is a soft signal: it cannot override configured
		// priority and is evaluated only among equal-priority candidates.
		if a.prefRank != b.prefRank {
			return a.prefRank < b.prefRank
		}
		// Performance is only a tie-break among equal configured priority.
		if a.providerRank == b.providerRank && a.modelRank == b.modelRank && a.prefRank == b.prefRank {
			if a.hasPerf != b.hasPerf {
				return a.hasPerf
			}
			if a.hasPerf && b.hasPerf && a.perfScore != b.perfScore {
				return a.perfScore < b.perfScore
			}
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
		if a.providerRank == b.providerRank && a.modelRank == b.modelRank && a.hasPerf && (!b.hasPerf || a.perfScore < b.perfScore) {
			perfInfluenced = true
		}
	}

	reasons := make([]string, 0, 3)
	if prefInfluenced {
		reasons = append(reasons, fmt.Sprintf("configured priority selected provider %q (priority %d) model %q (priority %d)", rankedList[0].c.Provider, rankedList[0].providerRank, rankedList[0].c.Alias, rankedList[0].modelRank))
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
	if rec.ObservedAt.IsZero() || state.Now.Sub(rec.ObservedAt.UTC()) > state.MaxPerformanceAge {
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
	mp, ok := rp.Models[alias]
	if !ok {
		return ConfiguredPriority{}, false
	}
	return ConfiguredPriority{Provider: provider, ProviderPriority: rp.Priority, ModelAlias: alias, ModelPriority: mp}, true
}

func performanceStageNote(influenced bool) string {
	if influenced {
		return "fresh sufficient performance used as equal-priority tie-break"
	}
	return "performance did not override configured preference; stale/sparse data ignored"
}

func decisionID(req RouteRequest, state SelectState, d Decision) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write(PolicyVersion)
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
