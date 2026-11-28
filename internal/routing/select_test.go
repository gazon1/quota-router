package routing

import (
	"strings"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var selNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func defaultCatalogue() settings.Catalogue {
	return settings.Catalogue{
		ModelAliases: map[string]settings.Model{},
		Routing: settings.Routing{
			LowQuotaThreshold: 0.1,
			ReserveFraction:   0,
			Providers:         map[string]settings.ProviderRouting{},
		},
	}
}
func boolPtr(v bool) *bool { return &v }

func testConfig() settings.Catalogue {
	cfg := defaultCatalogue()
	cfg.ModelAliases = map[string]settings.Model{
		"cc-coding": {
			Model:   "commandcode/glyph:free",
			Enabled: boolPtr(true),
			Capabilities: settings.Capabilities{
				Streaming:        settings.CapabilitySupported,
				ToolCalling:      settings.CapabilitySupported,
				StructuredOutput: settings.CapabilitySupported,
			},
			Tasks: []string{"coding", "reasoning"},
		},
		"cc-fast": {
			Model:   "commandcode/fast:free",
			Enabled: boolPtr(true),
			Capabilities: settings.Capabilities{
				Streaming:   settings.CapabilitySupported,
				ToolCalling: settings.CapabilitySupported,
			},
			Tasks: []string{"routine", "coding"},
		},
		"mm-general": {
			Model:   "minimax/MiniMax-M2.7",
			Enabled: boolPtr(true),
			Capabilities: settings.Capabilities{
				Streaming:        settings.CapabilitySupported,
				ToolCalling:      settings.CapabilitySupported,
				StructuredOutput: settings.CapabilitySupported,
			},
			Tasks: []string{"routine", "coding", "reasoning"},
		},
	}
	cfg.Routing.Providers = map[string]settings.ProviderRouting{
		"commandcode": {Priority: 1, Models: map[string]int{"cc-coding": 1, "cc-fast": 2}},
		"minimax":     {Priority: 2, Models: map[string]int{"mm-general": 1}},
	}
	cfg.Routing.ReserveFraction = 0.2
	return cfg
}

func healthyProviders(now time.Time, names ...string) map[string]quota.ProviderState {
	out := map[string]quota.ProviderState{}
	for _, n := range names {
		out[n] = quota.NewProviderState(n, quota.Healthy, quota.ReasonNone, now)
	}
	return out
}

func availableQuota(now time.Time, providers ...string) map[string]quota.Snapshot {
	out := map[string]quota.Snapshot{}
	for _, p := range providers {
		limit, rem, used := 100.0, 80.0, 20.0
		out[p] = quota.Observed(p, "test", now, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: now.Add(time.Hour)}}, 0.1)
	}
	return out
}

func baseRequest() RouteRequest {
	return RouteRequest{Version: RouteRequestVersionV1, SessionID: "s1", Capabilities: &RequiredCapabilities{}}
}

func findRejected(d Decision, alias string) (RejectedCandidate, bool) {
	for _, r := range d.Rejected {
		if r.Alias == alias {
			return r, true
		}
	}
	return RejectedCandidate{}, false
}

func hasStage(d Decision, stage string) bool {
	for _, s := range d.Stages {
		if s.Stage == stage {
			return true
		}
	}
	return false
}

func TestSelectDeterministic(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	d1 := sel.Select(req, state)
	d2 := sel.Select(req, state)
	assert.Equal(t, "cc-coding", d1.SelectedAlias)
	assert.NotEmpty(t, d1.DecisionID)
	assert.Equal(t, d1.DecisionID, d2.DecisionID, "decision id not deterministic")
	assert.Equal(t, PolicyVersion, d1.PolicyVersion)
	assert.True(t, d1.ConfiguredPreferenceInfluenced, "expected configured preference influence")
	require.NotNil(t, d1.ConfiguredPriority)
	assert.Equal(t, "cc-coding", d1.ConfiguredPriority.ModelAlias)
}

func TestDecisionIDChangesWhenConfigChanges(t *testing.T) {
	cfg1 := testConfig()
	cfg2 := testConfig()
	// Change a configured priority that does not alter the selected alias.
	cfg2.Routing.Providers["minimax"] = settings.ProviderRouting{Priority: 5, Models: map[string]int{"mm-general": 1}}
	req := baseRequest()
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}
	d1 := (Selector{Catalogue: settings.Catalogue{ModelAliases: cfg1.ModelAliases, Routing: cfg1.Routing}}).Select(req, state)
	d2 := (Selector{Catalogue: settings.Catalogue{ModelAliases: cfg2.ModelAliases, Routing: cfg2.Routing}}).Select(req, state)
	assert.Equal(t, "cc-coding", d1.SelectedAlias)
	assert.Equal(t, "cc-coding", d2.SelectedAlias)
	assert.NotEqual(t, d1.DecisionID, d2.DecisionID, "decision id should change when routing configuration changes")
}

func TestSelectNeverSelectsIneligible(t *testing.T) {
	cfg := testConfig()
	cfg.ModelAliases["cc-coding"] = settings.Model{Model: "commandcode/glyph:free", Enabled: boolPtr(false), Capabilities: cfg.ModelAliases["cc-coding"].Capabilities, Tasks: cfg.ModelAliases["cc-coding"].Tasks}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	// disabled excluded by initial candidates
	d := sel.Select(baseRequest(), state)
	assert.NotEqual(t, "cc-coding", d.SelectedAlias, "disabled alias selected")

	// incompatible
	req := baseRequest()
	req.Capabilities = &RequiredCapabilities{StructuredOutput: true}
	d = sel.Select(req, state)
	assert.NotEqual(t, "cc-fast", d.SelectedAlias, "incompatible alias selected")
	r, ok := findRejected(d, "cc-fast")
	require.True(t, ok, "want a cc-fast rejection")
	assert.Equal(t, "capabilities", r.Stage)

	// exhausted
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "exhausted provider selected")

	// unavailable
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Unavailable, quota.ReasonServerError, selNow)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "unavailable provider selected")

	// cooling down
	h = healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.ProviderState{Provider: "commandcode", Health: quota.Healthy, ObservedAt: selNow, CooldownUntil: selNow.Add(5 * time.Minute)}
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "cooldown provider selected")
}

func TestSelectReserveForReasoningCoding(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.ReserveFraction = 0.2
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 15.0, 85.0 // LOW_QUOTA and below reserve
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	state := SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}

	coding := baseRequest()
	coding.TaskClass = TaskCoding
	d := sel.Select(coding, state)
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "commandcode selected despite reserve")
	r, ok := findRejected(d, "cc-coding")
	require.True(t, ok, "want a cc-coding rejection")
	assert.Equal(t, "subscription_quota", r.Stage)
	assert.Contains(t, r.Reason, "reserved")

	routine := baseRequest()
	routine.TaskClass = TaskRoutine
	d = sel.Select(routine, state)
	assert.Equal(t, "commandcode", d.SelectedProvider, "routine task should still use low-quota provider")
}

func TestSelectHealthFreshness(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow.Add(-2*time.Hour))
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour, HealthMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "stale healthy provider selected")
	r, ok := findRejected(d, "cc-coding")
	require.True(t, ok, "want a cc-coding rejection")
	assert.Equal(t, "provider_health", r.Stage)
	assert.Contains(t, r.Reason, "stale")

	h["commandcode"] = quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow.Add(2*time.Minute))
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour, HealthMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "future-dated healthy provider selected")

	// A current observation is eligible: the freshness rule rejects only what
	// it cannot vouch for.
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow.Add(-time.Minute))
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour, HealthMaxAge: time.Hour})
	assert.Equal(t, "commandcode", d.SelectedProvider, "current healthy provider not selected")

	// A missing observation stays unknown and transient.
	delete(h, "commandcode")
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour, HealthMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "provider without a health observation selected")
	r, ok = findRejected(d, "cc-coding")
	require.True(t, ok, "want a cc-coding rejection")
	assert.Equal(t, "provider_health", r.Stage)
	assert.True(t, r.Transient)
}

func TestSelectUnknownQuotaAndHealth(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	// Unknown quota is conservative - not eligible
	q := availableQuota(selNow, "commandcode", "minimax")
	q["commandcode"] = quota.Failed("commandcode", "test", selNow, quota.TransientFetchFailure)
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "unknown quota provider selected")
	r, ok := findRejected(d, "cc-coding")
	require.True(t, ok, "want a cc-coding rejection")
	assert.Contains(t, r.Reason, "quota unknown")

	// Missing quota observation
	q = availableQuota(selNow, "minimax")
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "unobserved quota provider selected")

	// Unknown health is conservative - not eligible
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.HealthUnknown, quota.ReasonNotProbed, selNow)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	assert.NotEqual(t, "commandcode", d.SelectedProvider, "unknown health provider selected")
	r, ok = findRejected(d, "cc-coding")
	require.True(t, ok, "want a cc-coding rejection")
	assert.Equal(t, "provider_health", r.Stage)

	// Degraded health is allowed by explicit policy
	h = healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Degraded, quota.ReasonRateLimited, selNow)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	assert.Equal(t, "commandcode", d.SelectedProvider, "degraded provider should be eligible")
}

func TestSelectAffinityOnlyAmongViable(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "session-1"

	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		Affinity:    &Affinity{SessionID: "session-1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)},
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(req, state)
	assert.Equal(t, "mm-general", d.SelectedAlias)
	assert.True(t, d.AffinityInfluenced, "expected affinity influence")

	// Affinity must not override hard constraints
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["minimax"] = quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonServerError, selNow)
	state.Health = h
	d = sel.Select(req, state)
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "affinity overrode unavailable provider")
	assert.False(t, d.AffinityInfluenced, "affinity should not be marked influenced")

	// Affinity for different session ignored
	state.Health = healthyProviders(selNow, "commandcode", "minimax")
	state.Affinity = &Affinity{SessionID: "other", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}
	d = sel.Select(req, state)
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "affinity from other session applied")
}

func TestSelectConfiguredPriorityAndPerformance(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "want cc-coding by configured priority")

	// Performance cannot override configured provider priority
	state.Performance = map[string]PerformanceRecord{
		"mm-general": {Alias: "mm-general", Score: 1, Fresh: true, Sufficient: true, ObservedAt: selNow},
		"cc-coding":  {Alias: "cc-coding", Score: 1000, Fresh: true, Sufficient: true, ObservedAt: selNow},
	}
	state.MaxPerformanceAge = time.Hour
	d = sel.Select(baseRequest(), state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "performance overrode configured priority")
	assert.False(t, d.PerformanceInfluenced, "performance should not influence across configured priority boundary")

	// Performance can break ties among equal-priority candidates.
	cfg2 := testConfig()
	cfg2.Routing.Providers["commandcode"] = settings.ProviderRouting{Priority: 1, Models: map[string]int{"cc-coding": 1, "cc-fast": 1}}
	sel2 := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg2.ModelAliases, Routing: cfg2.Routing}}
	state.Performance = map[string]PerformanceRecord{
		"cc-coding": {Alias: "cc-coding", Score: 100, Fresh: true, Sufficient: true, ObservedAt: selNow},
		"cc-fast":   {Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow},
	}
	d = sel2.Select(baseRequest(), state)
	assert.Equal(t, "cc-fast", d.SelectedAlias, "want cc-fast by performance tie-break")
	assert.True(t, d.PerformanceInfluenced, "expected performance influence")

	// Stale performance ignored
	state.Performance["cc-fast"] = PerformanceRecord{Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow.Add(-2 * time.Hour)}
	d = sel2.Select(baseRequest(), state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "stale performance influenced selection")

	// Future-dated performance ignored
	state.Performance["cc-fast"] = PerformanceRecord{Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow.Add(2 * time.Minute)}
	d = sel2.Select(baseRequest(), state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "future performance influenced selection")

	// Sparse performance ignored
	state.Performance["cc-fast"] = PerformanceRecord{Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: false, ObservedAt: selNow}
	d = sel2.Select(baseRequest(), state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "sparse performance influenced selection")
}

func TestSelectManualOverride(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "mm-general"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}

	d := sel.Select(req, state)
	assert.Equal(t, "mm-general", d.SelectedAlias)
	assert.True(t, d.OverrideRequested)
	assert.True(t, d.OverrideApplied)

	// Override cannot bypass hard constraints
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["minimax"] = quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonServerError, selNow)
	state.Health = h
	d = sel.Select(req, state)
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "override bypassed unavailable provider")
	assert.False(t, d.OverrideApplied, "override should not be marked applied")
	assert.Contains(t, strings.Join(d.Reasons, " "), "rejected")

	// Override cannot bypass capabilities
	req2 := baseRequest()
	req2.Capabilities = &RequiredCapabilities{StructuredOutput: true}
	req2.Override = &ManualOverride{ModelAlias: "cc-fast"}
	state.Health = healthyProviders(selNow, "commandcode", "minimax")
	d = sel.Select(req2, state)
	assert.NotEqual(t, "cc-fast", d.SelectedAlias, "override bypassed capability constraint")

	// Unconfigured override target
	req3 := baseRequest()
	req3.Override = &ManualOverride{ModelAlias: "missing"}
	d = sel.Select(req3, state)
	assert.False(t, d.OverrideApplied, "missing override applied")
	assert.NotEqual(t, "missing", d.SelectedAlias, "missing alias selected")
}

func TestSelectDecisionExplanation(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	state := SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}

	d := sel.Select(baseRequest(), state)
	assert.True(t, hasStage(d, "capabilities"), "missing stages: %+v", d.Stages)
	assert.True(t, hasStage(d, "provider_health"), "missing stages: %+v", d.Stages)
	assert.True(t, hasStage(d, "subscription_quota"), "missing stages: %+v", d.Stages)
	assert.True(t, hasStage(d, "temporary_exclusions"), "missing stages: %+v", d.Stages)
	assert.True(t, hasStage(d, "session_affinity"), "missing stages: %+v", d.Stages)
	assert.NotEmpty(t, d.Rejected, "expected rejected candidates")
	found := false
	for _, r := range d.Rejected {
		if r.Alias == "cc-coding" && r.Stage == "subscription_quota" {
			found = true
		}
	}
	assert.True(t, found, "missing quota rejection: %+v", d.Rejected)
	assert.Equal(t, "mm-general", d.SelectedAlias)
	assert.NotNil(t, d.ConfiguredPriority, "expected configured priority")
}

func TestSelectNoEligible(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: map[string]quota.Snapshot{}, Health: map[string]quota.ProviderState{}, QuotaMaxAge: time.Hour})
	assert.Equal(t, "", d.SelectedAlias)
	assert.NotEmpty(t, d.DecisionID, "decision id should be set")
	assert.NotEmpty(t, d.Reasons, "expected reasons")
}

func TestSelectDeterministicTieBreak(t *testing.T) {
	cfg := defaultCatalogue()
	cfg.ModelAliases = map[string]settings.Model{
		"a": {Model: "p1/m1", Enabled: boolPtr(true), Capabilities: settings.Capabilities{Streaming: settings.CapabilitySupported}},
		"b": {Model: "p1/m2", Enabled: boolPtr(true), Capabilities: settings.Capabilities{Streaming: settings.CapabilitySupported}},
	}
	// No configured routing priorities: stable alias tie-break should apply.
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Capabilities = &RequiredCapabilities{Streaming: true}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "p1"), Health: healthyProviders(selNow, "p1"), QuotaMaxAge: time.Hour}
	d1 := sel.Select(req, state)
	d2 := sel.Select(req, state)
	assert.Equal(t, "a", d1.SelectedAlias)
	assert.Equal(t, d1.DecisionID, d2.DecisionID, "deterministic tie-break produced different decision IDs")
}

func TestSelectModelPriorityNotComparedAcrossProviders(t *testing.T) {
	cfg := testConfig()
	cfg.ModelAliases = map[string]settings.Model{
		"a1": {Model: "provider-a/model-1", Enabled: boolPtr(true), Capabilities: settings.Capabilities{Streaming: settings.CapabilitySupported}},
		"b1": {Model: "provider-b/model-1", Enabled: boolPtr(true), Capabilities: settings.Capabilities{Streaming: settings.CapabilitySupported}},
	}
	cfg.Routing.Providers = map[string]settings.ProviderRouting{
		"provider-a": {Priority: 1, Models: map[string]int{"a1": 10}},
		"provider-b": {Priority: 1, Models: map[string]int{"b1": 1}},
	}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := RouteRequest{Version: 1, SessionID: "s1", Capabilities: &RequiredCapabilities{}}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "provider-a", "provider-b"), Health: healthyProviders(selNow, "provider-a", "provider-b"), QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.Equal(t, "provider-a", d.SelectedProvider, "want provider-a (provider name tie-break, model ranks not cross-provider comparable)")
	assert.Equal(t, "a1", d.SelectedAlias)
}

func TestSelectProviderOnlyPriorityReported(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Providers = map[string]settings.ProviderRouting{
		"commandcode": {Priority: 1},
		"minimax":     {Priority: 2},
	}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.True(t, d.ConfiguredPreferenceInfluenced, "provider-only configured priority should influence selection")
	require.NotNil(t, d.ConfiguredPriority, "ConfiguredPriority should be reported for provider-only priority")
	assert.Equal(t, "commandcode", d.ConfiguredPriority.Provider)
	assert.Equal(t, 1, d.ConfiguredPriority.ProviderPriority)
	assert.Nil(t, d.ConfiguredPriority.ModelPriority, "want nil for provider-only priority")
}

func TestSelectAffinityRejectionsInDecisionRejected(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: req.SessionID, Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.True(t, d.AffinityInfluenced, "expected affinity influence")
	r, ok := findRejected(d, "cc-coding")
	require.True(t, ok, "affinity elimination missing from Decision.Rejected: %+v", d.Rejected)
	assert.Equal(t, "session_affinity", r.Stage)
}

func TestSelectOverrideRejectionsInDecisionRejected(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "mm-general"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.True(t, d.OverrideApplied, "override should apply")
	r, ok := findRejected(d, "cc-coding")
	require.True(t, ok, "override elimination missing from Decision.Rejected: %+v", d.Rejected)
	assert.Equal(t, "manual_override", r.Stage)
}

func TestSelectRequestPreferenceDoesNotOverrideConfiguredPriority(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Preference = &RoutePreference{ModelAlias: "mm-general"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "request preference should not override configured priority")
	found := false
	for _, r := range d.Reasons {
		if strings.Contains(r, "request model preference") && strings.Contains(r, "not selected") {
			found = true
		}
	}
	assert.True(t, found, "reasons missing preference explanation: %v", d.Reasons)
}

func TestSelectRequestPreferenceBreaksEqualPriority(t *testing.T) {
	cfg := defaultCatalogue()
	cfg.ModelAliases = map[string]settings.Model{
		"a": {Model: "p1/m1", Enabled: boolPtr(true)},
		"b": {Model: "p1/m2", Enabled: boolPtr(true)},
	}
	cfg.Routing.Providers = map[string]settings.ProviderRouting{"p1": {Priority: 1, Models: map[string]int{"a": 1, "b": 1}}}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Preference = &RoutePreference{ModelAlias: "b"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "p1"), Health: healthyProviders(selNow, "p1"), QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.Equal(t, "b", d.SelectedAlias)
}

func TestSelectPerformanceNotInfluencedWhenPreferenceDecides(t *testing.T) {
	cfg := defaultCatalogue()
	cfg.ModelAliases = map[string]settings.Model{
		"a": {Model: "p1/m1", Enabled: boolPtr(true)},
		"b": {Model: "p1/m2", Enabled: boolPtr(true)},
	}
	cfg.Routing.Providers = map[string]settings.ProviderRouting{"p1": {Priority: 1, Models: map[string]int{"a": 1, "b": 1}}}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Preference = &RoutePreference{ModelAlias: "a"}
	state := SelectState{
		Now:               selNow,
		Quota:             availableQuota(selNow, "p1"),
		Health:            healthyProviders(selNow, "p1"),
		QuotaMaxAge:       time.Hour,
		MaxPerformanceAge: time.Hour,
		Performance: map[string]PerformanceRecord{
			"a": {Alias: "a", Score: 100, Fresh: true, Sufficient: true, ObservedAt: selNow},
			"b": {Alias: "b", Score: 1, Fresh: true, Sufficient: true, ObservedAt: selNow},
		},
	}
	d := sel.Select(req, state)
	assert.Equal(t, "a", d.SelectedAlias)
	assert.False(t, d.PerformanceInfluenced, "performance should not be reported as influence when request preference decided")
}

func TestSelectAffinityDifferentSessionNoInfluence(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s2"
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.False(t, d.AffinityInfluenced, "affinity influenced for different session")
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "wrong-session affinity selected")
}

func TestSelectAffinityEmptySessionIDIgnored(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s1"
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: "", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	assert.False(t, d.AffinityInfluenced, "empty affinity session id should not influence selection")
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "empty affinity session id acted as wildcard")
}

func TestSelectInvalidRequest(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(RouteRequest{}, SelectState{Now: selNow})
	assert.Equal(t, "", d.SelectedAlias)
	assert.NotEmpty(t, d.DecisionID, "decision id should be set")
	require.NotEmpty(t, d.Reasons)
	assert.Contains(t, d.Reasons[0], "request invalid")
}

func TestSelectPreferredCandidateIneligibleFallsThrough(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.Equal(t, "mm-general", d.SelectedAlias, "want mm-general fallback")
	require.NotNil(t, d.ConfiguredPriority)
	assert.Equal(t, "minimax", d.ConfiguredPriority.Provider)
}

func TestSelectStageCoverage(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.TaskClass = TaskCoding
	req.Capabilities = &RequiredCapabilities{ToolCalling: true}
	d := sel.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	stages := []string{"configuration", "capabilities", "model_availability", "provider_health", "subscription_quota", "temporary_exclusions", "session_affinity", "configured_preference", "performance_preference", "tie_break"}
	for _, s := range stages {
		assert.True(t, hasStage(d, s), "missing stage %q in %+v", s, d.Stages)
	}
}

func TestSelectSkipsExhaustedProviderButKeepsOtherProviderPriority(t *testing.T) {
	cfg := testConfig()
	// Add second model in minimax with lower priority than mm-general.
	cfg.ModelAliases["mm-cheap"] = settings.Model{Model: "minimax/cheap", Enabled: boolPtr(true), Capabilities: settings.Capabilities{Streaming: settings.CapabilitySupported}}
	cfg.Routing.Providers["minimax"] = settings.ProviderRouting{Priority: 2, Models: map[string]int{"mm-general": 1, "mm-cheap": 2}}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.Equal(t, "mm-general", d.SelectedAlias)
}

func TestSelectOverrideCannotBypassQuotaOrCooldown(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	// exhausted
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["minimax"] = quota.Observed("minimax", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "mm-general"}
	d := sel.Select(req, SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.False(t, d.OverrideApplied, "override bypassed exhaustion")
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "override bypassed exhaustion")

	// cooldown
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["minimax"] = quota.ProviderState{Provider: "minimax", Health: quota.Healthy, ObservedAt: selNow, CooldownUntil: selNow.Add(time.Minute)}
	d = sel.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	assert.False(t, d.OverrideApplied, "override bypassed cooldown")
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "override bypassed cooldown")

	// disabled
	cfg2 := testConfig()
	m := cfg2.ModelAliases["mm-general"]
	m.Enabled = boolPtr(false)
	cfg2.ModelAliases["mm-general"] = m
	d = Selector{Catalogue: settings.Catalogue{ModelAliases: cfg2.ModelAliases, Routing: cfg2.Routing}}.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	assert.False(t, d.OverrideApplied, "override selected disabled")
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "override selected disabled")
}

func TestSelectTableDrivenStages(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	limit100b, rem0, used100 := 100.0, 0.0, 100.0
	exhaustedWin := []quota.Window{{Name: "5h", Limit: &limit100b, Remaining: &rem0, Used: &used100, ResetsAt: selNow.Add(time.Hour)}}

	// Configuration/disabled
	{
		c := testConfig()
		m := c.ModelAliases["cc-coding"]
		off := false
		m.Enabled = &off
		c.ModelAliases["cc-coding"] = m
		d := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		assert.NotEqual(t, "cc-coding", d.SelectedAlias, "disabled selected")
		r, ok := findRejected(d, "cc-coding")
		require.True(t, ok, "disabled rejection missing")
		assert.Equal(t, "configuration", r.Stage)
	}

	// Capabilities
	{
		req := baseRequest()
		req.Capabilities = &RequiredCapabilities{StructuredOutput: true}
		d := sel.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		r, ok := findRejected(d, "cc-fast")
		require.True(t, ok, "cap rejection missing")
		assert.Equal(t, "capabilities", r.Stage)
	}

	// Provider health unavailable
	{
		h := healthyProviders(selNow, "commandcode", "minimax")
		h["commandcode"] = quota.NewProviderState("commandcode", quota.Unavailable, quota.ReasonServerError, selNow)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
		r, ok := findRejected(d, "cc-coding")
		require.True(t, ok, "health rejection missing")
		assert.Equal(t, "provider_health", r.Stage)
	}

	// Unknown health
	{
		h := healthyProviders(selNow, "commandcode", "minimax")
		h["commandcode"] = quota.NewProviderState("commandcode", quota.HealthUnknown, quota.ReasonNotProbed, selNow)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
		r, ok := findRejected(d, "cc-coding")
		require.True(t, ok, "unknown health rejection missing")
		assert.Equal(t, "provider_health", r.Stage)
	}

	// Exhausted quota
	{
		q := availableQuota(selNow, "commandcode", "minimax")
		q["commandcode"] = quota.Observed("commandcode", "test", selNow, exhaustedWin, 0.1)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		r, ok := findRejected(d, "cc-coding")
		require.True(t, ok, "exhausted rejection missing")
		assert.Equal(t, "subscription_quota", r.Stage)
	}

	// Unknown quota
	{
		q := availableQuota(selNow, "commandcode", "minimax")
		q["commandcode"] = quota.Failed("commandcode", "test", selNow, quota.MalformedResponse)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		r, ok := findRejected(d, "cc-coding")
		require.True(t, ok, "unknown quota rejection missing")
		assert.Equal(t, "subscription_quota", r.Stage)
	}

	// Cooldown
	{
		h := healthyProviders(selNow, "commandcode", "minimax")
		hs := quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow)
		hs.CooldownUntil = selNow.Add(10 * time.Minute)
		h["commandcode"] = hs
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
		r, ok := findRejected(d, "cc-coding")
		require.True(t, ok, "cooldown rejection missing")
		assert.Equal(t, "temporary_exclusions", r.Stage)
	}

	// Affinity
	{
		d := sel.Select(RouteRequest{Version: RouteRequestVersionV1, SessionID: "s1", Capabilities: &RequiredCapabilities{}}, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour})
		assert.True(t, d.AffinityInfluenced, "affinity: %+v", d)
		assert.Equal(t, "mm-general", d.SelectedAlias)
	}

	// Configured preference
	{
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		assert.True(t, d.ConfiguredPreferenceInfluenced, "preference: %+v", d)
		assert.Equal(t, "cc-coding", d.SelectedAlias)
	}

	// Performance tie-break
	{
		c := testConfig()
		c.Routing.Providers["commandcode"] = settings.ProviderRouting{Priority: 1, Models: map[string]int{"cc-coding": 1, "cc-fast": 1}}
		d := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour, MaxPerformanceAge: time.Hour, Performance: map[string]PerformanceRecord{"cc-fast": {Alias: "cc-fast", Score: 1, Fresh: true, Sufficient: true, ObservedAt: selNow}, "cc-coding": {Alias: "cc-coding", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow}}})
		assert.True(t, d.PerformanceInfluenced, "perf tie break: %+v", d)
		assert.Equal(t, "cc-fast", d.SelectedAlias)
	}

	// Deterministic tie-break
	{
		c := defaultCatalogue()
		c.ModelAliases = map[string]settings.Model{"b": {Model: "p1/b", Enabled: boolPtr(true)}, "a": {Model: "p1/a", Enabled: boolPtr(true)}}
		q := availableQuota(selNow, "p1")
		d1 := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "p1"), QuotaMaxAge: time.Hour})
		d2 := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "p1"), QuotaMaxAge: time.Hour})
		assert.Equal(t, "a", d1.SelectedAlias)
		assert.Equal(t, d1.DecisionID, d2.DecisionID)
	}

}

// A pin that has expired decides nothing: the decision is re-evaluated from the
// full candidate set, and the explanation says so rather than "no match".
func TestSelectExpiredAffinityReevaluates(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s1"
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		Affinity:    &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow},
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(req, state)
	assert.False(t, d.AffinityInfluenced, "expired affinity influenced selection")
	assert.Equal(t, "cc-coding", d.SelectedAlias, "want cc-coding from a fresh evaluation")
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	assert.Contains(t, note, "expired", "want it to say the pin expired")

	// The same inputs with an unexpired pin choose the other route, so expiry is
	// what decided — not a different candidate set.
	state.Affinity.ExpiresAt = selNow.Add(time.Minute)
	d = sel.Select(req, state)
	assert.Equal(t, "mm-general", d.SelectedAlias, "live pin")
	assert.True(t, d.AffinityInfluenced, "live pin")
}

// A pin without an expiry is not affinity: an unbounded pin would outlive the
// route it names and silently override every later decision.
func TestSelectUnboundedAffinityIgnored(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		Affinity:    &Affinity{SessionID: req.SessionID, Alias: "mm-general"},
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(req, state)
	assert.False(t, d.AffinityInfluenced, "pin without expiry decided the route: %+v", d)
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "pin without expiry decided the route: %+v", d)
}

// A transient exclusion — a missing or stale observation — must not move a pinned
// session: it says nothing about the route. A durable one must, because the route
// itself is gone. A cooldown is durable in this sense: it is an explicit decision
// that the provider should receive no traffic, so a new request must respect it
// even when the session would rather stay.
func TestSelectAffinityHeldThroughTransientExclusions(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s1"

	cases := []struct {
		name   string
		quota  func() map[string]quota.Snapshot
		health func() map[string]quota.ProviderState
		// want is the selected alias: the pinned mm-general when the exclusion is
		// transient, the next ranked route when it is durable.
		want string
	}{
		{name: "healthy", want: "mm-general"},
		{name: "stale health", want: "mm-general", health: func() map[string]quota.ProviderState {
			h := healthyProviders(selNow, "commandcode")
			h["minimax"] = quota.NewProviderState("minimax", quota.Healthy, quota.ReasonNone, selNow.Add(-2*time.Hour))
			return h
		}},
		{name: "unobserved quota", want: "mm-general", quota: func() map[string]quota.Snapshot {
			return availableQuota(selNow, "commandcode")
		}},
		{name: "unknown quota", want: "mm-general", quota: func() map[string]quota.Snapshot {
			q := availableQuota(selNow, "commandcode")
			q["minimax"] = quota.Failed("minimax", "test", selNow, quota.TransientFetchFailure)
			return q
		}},
		// A cooldown is an explicit "no traffic" statement, not missing
		// information, so the pinned session moves off the cooled provider.
		{name: "cooldown", want: "cc-coding", health: func() map[string]quota.ProviderState {
			h := healthyProviders(selNow, "commandcode", "minimax")
			hs := quota.NewProviderState("minimax", quota.Healthy, quota.ReasonNone, selNow)
			hs.CooldownUntil = selNow.Add(5 * time.Minute)
			h["minimax"] = hs
			return h
		}},
		{name: "unavailable", want: "cc-coding", health: func() map[string]quota.ProviderState {
			h := healthyProviders(selNow, "commandcode")
			h["minimax"] = quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonServerError, selNow)
			return h
		}},
		{name: "exhausted", want: "cc-coding", quota: func() map[string]quota.Snapshot {
			q := availableQuota(selNow, "commandcode")
			limit, rem, used := 100.0, 0.0, 100.0
			q["minimax"] = quota.Observed("minimax", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
			return q
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, h := availableQuota(selNow, "commandcode", "minimax"), healthyProviders(selNow, "commandcode", "minimax")
			if tc.quota != nil {
				q = tc.quota()
			}
			if tc.health != nil {
				h = tc.health()
			}
			state := SelectState{
				Now:         selNow,
				Quota:       q,
				Health:      h,
				Affinity:    &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)},
				QuotaMaxAge: time.Hour,
			}
			d := sel.Select(req, state)
			assert.Equal(t, tc.want, d.SelectedAlias)
			held := tc.want == "mm-general"
			assert.Equal(t, held, d.AffinityInfluenced)
		})
	}

	// A disabled alias is a durable exclusion too: the route is not routable.
	off := false
	cfg2 := testConfig()
	m := cfg2.ModelAliases["mm-general"]
	m.Enabled = &off
	cfg2.ModelAliases["mm-general"] = m
	d := (Selector{Catalogue: settings.Catalogue{ModelAliases: cfg2.ModelAliases, Routing: cfg2.Routing}}).Select(req, SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		Affinity:    &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)},
		QuotaMaxAge: time.Hour,
	})
	assert.Equal(t, "cc-coding", d.SelectedAlias, "pin survived a disabled alias")
	assert.False(t, d.AffinityInfluenced, "pin survived a disabled alias")
}

// A cooldown must move a pinned session — the controller is choosing where to
// send a new request, and "send it nowhere for five minutes" is what a cooldown
// says. Once it expires, normal selection policy decides again, which means the
// session does not automatically return to the old route: it is re-pinned to
// whatever is selected then.
func TestSelectCooldownMovesPinnedSessionAndExpiryAllowsReturn(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s1"

	cooled := healthyProviders(selNow, "commandcode", "minimax")
	hs := quota.NewProviderState("minimax", quota.Healthy, quota.ReasonNone, selNow)
	hs.CooldownUntil = selNow.Add(5 * time.Minute)
	cooled["minimax"] = hs

	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      cooled,
		Affinity:    &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)},
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(req, state)
	assert.NotEqual(t, "mm-general", d.SelectedAlias, "pinned session held on a cooled provider")
	assert.False(t, d.AffinityInfluenced, "pinned session held on a cooled provider")
	assert.Equal(t, "cc-coding", d.SelectedAlias, "want cc-coding while minimax is cooled")
	r, ok := rejectedCandidate(d.Rejected, "mm-general")
	require.True(t, ok, "cooldown rejection missing")
	assert.False(t, r.Transient, "want a durable rejection")
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	assert.Contains(t, note, "durable", "want it to name the durable exclusion")

	// After the cooldown expires, the session is pinned to whatever the policy
	// now selects. Here that is cc-coding, because the pin it used to hold is
	// gone from the stored route; selection is ordinary again.
	state.Health = healthyProviders(selNow, "commandcode", "minimax")
	state.Affinity = &Affinity{SessionID: "s1", Alias: "cc-coding", ExpiresAt: selNow.Add(time.Hour)}
	d = sel.Select(req, state)
	assert.Equal(t, "cc-coding", d.SelectedAlias, "after cooldown")
	assert.True(t, d.AffinityInfluenced, "after cooldown")
}

// A pin excluded before the affinity stage is reached — a disabled alias or one
// that no longer satisfies the request's capabilities — must be named in the
// explanation, not reported as a vague "no viable affinity match".
func TestSelectAffinityNoteNamesEarlyExclusion(t *testing.T) {
	cfg := testConfig()
	off := false
	m := cfg.ModelAliases["mm-general"]
	m.Enabled = &off
	cfg.ModelAliases["mm-general"] = m
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		Affinity:    &Affinity{SessionID: req.SessionID, Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)},
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(req, state)
	assert.False(t, d.AffinityInfluenced, "disabled pin influenced selection")
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	assert.Contains(t, note, "configuration", "want it to name the configuration exclusion")
	assert.Contains(t, note, "disabled", "want it to name the configuration exclusion")
}
