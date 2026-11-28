package routing

import (
	"fmt"
	"sort"
	"strings"
)

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
	hasConfiguredPref := len(s.Catalogue.Routing.Providers) > 0
	for _, c := range cs {
		r := ranked{c: c, providerRank: int(^uint(0) >> 1), modelRank: int(^uint(0) >> 1)}
		if rp, ok := s.Catalogue.Routing.Providers[c.Provider]; ok {
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
	m, ok := s.Catalogue.ModelAliases[alias]
	if !ok {
		return ConfiguredPriority{}, false
	}
	provider := m.Provider()
	rp, ok := s.Catalogue.Routing.Providers[provider]
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
