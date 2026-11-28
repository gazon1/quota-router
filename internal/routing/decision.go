package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gazon1/quota-router/internal/settings"
)

func decisionID(req RouteRequest, state SelectState, d Decision, cfg settings.Catalogue) string {
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

func configFingerprint(cfg settings.Catalogue) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write("routing.low_quota_threshold", fmt.Sprintf("%.17g", cfg.Routing.LowQuotaThreshold))
	write("routing.reserve_fraction", fmt.Sprintf("%.17g", cfg.Routing.ReserveFraction))

	aliasKeys := make([]string, 0, len(cfg.ModelAliases))
	for k := range cfg.ModelAliases {
		aliasKeys = append(aliasKeys, k)
	}
	sort.Strings(aliasKeys)
	for _, alias := range aliasKeys {
		m := cfg.ModelAliases[alias]
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
