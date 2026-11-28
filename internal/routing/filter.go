package routing

import (
	"fmt"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/settings"
)

func filterCapabilities(cs []candidate, req RouteRequest) ([]candidate, []RejectedCandidate) {
	required := requirementsFromRequest(req)
	var elim []RejectedCandidate
	for _, c := range cs {
		if !required.SatisfiedBy(c.Model) {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "capabilities", Reason: capabilityReason(required, c.Model)})
		}
	}
	return cs, elim
}

func requirementsFromRequest(req RouteRequest) settings.Requirements {
	r := settings.Requirements{}
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

func capabilityReason(req settings.Requirements, m settings.Model) string {
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

func filterHealth(cs []candidate, state SelectState) ([]candidate, []RejectedCandidate) {
	var elim []RejectedCandidate
	for _, c := range cs {
		h, ok := state.Health[c.Provider]
		if !ok {
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: "provider health unknown", Transient: true})
			continue
		}
		switch quota.ClassifyHealth(h.ObservedAt, state.Now, state.HealthMaxAge) {
		case quota.HealthNotObserved:
			elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "provider_health", Reason: "provider health not observed", Transient: true})
			continue
		case quota.HealthStale:
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
	}
	return cs, elim
}

func filterQuota(cs []candidate, cfg settings.Catalogue, req RouteRequest, state SelectState) ([]candidate, []RejectedCandidate) {
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

func reserveApplies(req RouteRequest, cfg settings.Catalogue) bool {
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
func filterCooldown(cs []candidate, state SelectState) ([]candidate, []RejectedCandidate) {
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
