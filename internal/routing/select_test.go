package routing

import (
	"strings"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/settings"
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
	if d1.SelectedAlias != "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want cc-coding", d1.SelectedAlias)
	}
	if d1.DecisionID == "" || d1.DecisionID != d2.DecisionID {
		t.Fatalf("DecisionID not deterministic: %q vs %q", d1.DecisionID, d2.DecisionID)
	}
	if d1.PolicyVersion != PolicyVersion {
		t.Fatalf("PolicyVersion = %q", d1.PolicyVersion)
	}
	if !d1.ConfiguredPreferenceInfluenced {
		t.Fatal("expected configured preference influence")
	}
	if d1.ConfiguredPriority == nil || d1.ConfiguredPriority.ModelAlias != "cc-coding" {
		t.Fatalf("ConfiguredPriority = %+v", d1.ConfiguredPriority)
	}
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
	if d1.SelectedAlias != "cc-coding" || d2.SelectedAlias != "cc-coding" {
		t.Fatalf("selected aliases = %q, %q; want cc-coding, cc-coding", d1.SelectedAlias, d2.SelectedAlias)
	}
	if d1.DecisionID == d2.DecisionID {
		t.Fatal("decision id should change when routing configuration changes")
	}
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
	if d.SelectedAlias == "cc-coding" {
		t.Fatal("disabled alias selected")
	}

	// incompatible
	req := baseRequest()
	req.Capabilities = &RequiredCapabilities{StructuredOutput: true}
	d = sel.Select(req, state)
	if d.SelectedAlias == "cc-fast" {
		t.Fatal("incompatible alias selected")
	}
	if r, ok := findRejected(d, "cc-fast"); !ok || r.Stage != "capabilities" {
		t.Fatalf("cc-fast rejection = %+v, ok=%v", r, ok)
	}

	// exhausted
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatalf("exhausted provider selected: %+v", d)
	}

	// unavailable
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Unavailable, quota.ReasonServerError, selNow)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("unavailable provider selected")
	}

	// cooling down
	h = healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.ProviderState{Provider: "commandcode", Health: quota.Healthy, ObservedAt: selNow, CooldownUntil: selNow.Add(5 * time.Minute)}
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("cooldown provider selected")
	}
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
	if d.SelectedProvider == "commandcode" {
		t.Fatalf("commandcode selected despite reserve: %+v", d)
	}
	if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "subscription_quota" || !strings.Contains(r.Reason, "reserved") {
		t.Fatalf("cc-coding rejection = %+v, ok=%v", r, ok)
	}

	routine := baseRequest()
	routine.TaskClass = TaskRoutine
	d = sel.Select(routine, state)
	if d.SelectedProvider != "commandcode" {
		t.Fatalf("routine task should still use low-quota provider, got %+v", d)
	}
}

func TestSelectHealthFreshness(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow.Add(-2*time.Hour))
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour, HealthMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("stale healthy provider selected")
	}
	if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "provider_health" || !strings.Contains(r.Reason, "stale") {
		t.Fatalf("rejection=%+v ok=%v", r, ok)
	}

	h["commandcode"] = quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow.Add(2*time.Minute))
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour, HealthMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("future-dated healthy provider selected")
	}
}

func TestSelectUnknownQuotaAndHealth(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	// Unknown quota is conservative - not eligible
	q := availableQuota(selNow, "commandcode", "minimax")
	q["commandcode"] = quota.Failed("commandcode", "test", selNow, quota.TransientFetchFailure)
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("unknown quota provider selected")
	}
	if r, ok := findRejected(d, "cc-coding"); !ok || !strings.Contains(r.Reason, "quota unknown") {
		t.Fatalf("rejection = %+v ok=%v", r, ok)
	}

	// Missing quota observation
	q = availableQuota(selNow, "minimax")
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("unobserved quota provider selected")
	}

	// Unknown health is conservative - not eligible
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.HealthUnknown, quota.ReasonNotProbed, selNow)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	if d.SelectedProvider == "commandcode" {
		t.Fatal("unknown health provider selected")
	}
	if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "provider_health" {
		t.Fatalf("rejection = %+v ok=%v", r, ok)
	}

	// Degraded health is allowed by explicit policy
	h = healthyProviders(selNow, "commandcode", "minimax")
	h["commandcode"] = quota.NewProviderState("commandcode", quota.Degraded, quota.ReasonRateLimited, selNow)
	d = sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	if d.SelectedProvider != "commandcode" {
		t.Fatalf("degraded provider should be eligible, got %+v", d)
	}
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
	if d.SelectedAlias != "mm-general" {
		t.Fatalf("SelectedAlias = %q, want mm-general", d.SelectedAlias)
	}
	if !d.AffinityInfluenced {
		t.Fatal("expected affinity influence")
	}

	// Affinity must not override hard constraints
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["minimax"] = quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonServerError, selNow)
	state.Health = h
	d = sel.Select(req, state)
	if d.SelectedAlias == "mm-general" {
		t.Fatal("affinity overrode unavailable provider")
	}
	if d.AffinityInfluenced {
		t.Fatal("affinity should not be marked influenced")
	}

	// Affinity for different session ignored
	state.Health = healthyProviders(selNow, "commandcode", "minimax")
	state.Affinity = &Affinity{SessionID: "other", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}
	d = sel.Select(req, state)
	if d.SelectedAlias == "mm-general" {
		t.Fatal("affinity from other session applied")
	}
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
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want cc-coding by configured priority", d.SelectedAlias)
	}

	// Performance cannot override configured provider priority
	state.Performance = map[string]PerformanceRecord{
		"mm-general": {Alias: "mm-general", Score: 1, Fresh: true, Sufficient: true, ObservedAt: selNow},
		"cc-coding":  {Alias: "cc-coding", Score: 1000, Fresh: true, Sufficient: true, ObservedAt: selNow},
	}
	state.MaxPerformanceAge = time.Hour
	d = sel.Select(baseRequest(), state)
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("performance overrode configured priority: %q", d.SelectedAlias)
	}
	if d.PerformanceInfluenced {
		t.Fatal("performance should not influence across configured priority boundary")
	}

	// Performance can break ties among equal-priority candidates.
	cfg2 := testConfig()
	cfg2.Routing.Providers["commandcode"] = settings.ProviderRouting{Priority: 1, Models: map[string]int{"cc-coding": 1, "cc-fast": 1}}
	sel2 := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg2.ModelAliases, Routing: cfg2.Routing}}
	state.Performance = map[string]PerformanceRecord{
		"cc-coding": {Alias: "cc-coding", Score: 100, Fresh: true, Sufficient: true, ObservedAt: selNow},
		"cc-fast":   {Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow},
	}
	d = sel2.Select(baseRequest(), state)
	if d.SelectedAlias != "cc-fast" {
		t.Fatalf("SelectedAlias = %q, want cc-fast by performance tie-break", d.SelectedAlias)
	}
	if !d.PerformanceInfluenced {
		t.Fatal("expected performance influence")
	}

	// Stale performance ignored
	state.Performance["cc-fast"] = PerformanceRecord{Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow.Add(-2 * time.Hour)}
	d = sel2.Select(baseRequest(), state)
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("stale performance influenced selection: %q", d.SelectedAlias)
	}

	// Future-dated performance ignored
	state.Performance["cc-fast"] = PerformanceRecord{Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow.Add(2 * time.Minute)}
	d = sel2.Select(baseRequest(), state)
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("future performance influenced selection: %q", d.SelectedAlias)
	}

	// Sparse performance ignored
	state.Performance["cc-fast"] = PerformanceRecord{Alias: "cc-fast", Score: 10, Fresh: true, Sufficient: false, ObservedAt: selNow}
	d = sel2.Select(baseRequest(), state)
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("sparse performance influenced selection: %q", d.SelectedAlias)
	}
}

func TestSelectManualOverride(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "mm-general"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}

	d := sel.Select(req, state)
	if d.SelectedAlias != "mm-general" {
		t.Fatalf("SelectedAlias = %q, want mm-general", d.SelectedAlias)
	}
	if !d.OverrideRequested || !d.OverrideApplied {
		t.Fatalf("override flags = requested:%v applied:%v", d.OverrideRequested, d.OverrideApplied)
	}

	// Override cannot bypass hard constraints
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["minimax"] = quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonServerError, selNow)
	state.Health = h
	d = sel.Select(req, state)
	if d.SelectedAlias == "mm-general" {
		t.Fatal("override bypassed unavailable provider")
	}
	if d.OverrideApplied {
		t.Fatal("override should not be marked applied")
	}
	if !strings.Contains(strings.Join(d.Reasons, " "), "rejected") {
		t.Fatalf("reasons = %v", d.Reasons)
	}

	// Override cannot bypass capabilities
	req2 := baseRequest()
	req2.Capabilities = &RequiredCapabilities{StructuredOutput: true}
	req2.Override = &ManualOverride{ModelAlias: "cc-fast"}
	state.Health = healthyProviders(selNow, "commandcode", "minimax")
	d = sel.Select(req2, state)
	if d.SelectedAlias == "cc-fast" {
		t.Fatal("override bypassed capability constraint")
	}

	// Unconfigured override target
	req3 := baseRequest()
	req3.Override = &ManualOverride{ModelAlias: "missing"}
	d = sel.Select(req3, state)
	if d.OverrideApplied {
		t.Fatal("missing override applied")
	}
	if d.SelectedAlias == "missing" {
		t.Fatal("missing alias selected")
	}
}

func TestSelectDecisionExplanation(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	state := SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}

	d := sel.Select(baseRequest(), state)
	if !hasStage(d, "capabilities") || !hasStage(d, "provider_health") || !hasStage(d, "subscription_quota") || !hasStage(d, "temporary_exclusions") || !hasStage(d, "session_affinity") {
		t.Fatalf("missing stages: %+v", d.Stages)
	}
	if len(d.Rejected) == 0 {
		t.Fatal("expected rejected candidates")
	}
	found := false
	for _, r := range d.Rejected {
		if r.Alias == "cc-coding" && r.Stage == "subscription_quota" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing quota rejection: %+v", d.Rejected)
	}
	if d.SelectedAlias != "mm-general" {
		t.Fatalf("SelectedAlias = %q", d.SelectedAlias)
	}
	if d.ConfiguredPriority == nil {
		t.Fatal("expected configured priority")
	}
}

func TestSelectNoEligible(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: map[string]quota.Snapshot{}, Health: map[string]quota.ProviderState{}, QuotaMaxAge: time.Hour})
	if d.SelectedAlias != "" {
		t.Fatalf("SelectedAlias = %q, want empty", d.SelectedAlias)
	}
	if d.DecisionID == "" {
		t.Fatal("decision id should be set")
	}
	if len(d.Reasons) == 0 {
		t.Fatal("expected reasons")
	}
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
	if d1.SelectedAlias != "a" {
		t.Fatalf("SelectedAlias = %q, want a", d1.SelectedAlias)
	}
	if d1.DecisionID != d2.DecisionID {
		t.Fatal("deterministic tie-break produced different decision IDs")
	}
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
	if d.SelectedProvider != "provider-a" {
		t.Fatalf("SelectedProvider = %q, want provider-a (provider name tie-break, model ranks not cross-provider comparable)", d.SelectedProvider)
	}
	if d.SelectedAlias != "a1" {
		t.Fatalf("SelectedAlias = %q, want a1", d.SelectedAlias)
	}
}

func TestSelectProviderOnlyPriorityReported(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Providers = map[string]settings.ProviderRouting{
		"commandcode": {Priority: 1},
		"minimax":     {Priority: 2},
	}
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	if !d.ConfiguredPreferenceInfluenced {
		t.Fatal("provider-only configured priority should influence selection")
	}
	if d.ConfiguredPriority == nil {
		t.Fatal("ConfiguredPriority should be reported for provider-only priority")
	}
	if d.ConfiguredPriority.Provider != "commandcode" || d.ConfiguredPriority.ProviderPriority != 1 {
		t.Fatalf("ConfiguredPriority = %+v", d.ConfiguredPriority)
	}
	if d.ConfiguredPriority.ModelPriority != nil {
		t.Fatalf("ModelPriority = %v, want nil for provider-only priority", *d.ConfiguredPriority.ModelPriority)
	}
}

func TestSelectAffinityRejectionsInDecisionRejected(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: req.SessionID, Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	if !d.AffinityInfluenced {
		t.Fatal("expected affinity influence")
	}
	if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "session_affinity" {
		t.Fatalf("affinity elimination missing from Decision.Rejected: %+v", d.Rejected)
	}
}

func TestSelectOverrideRejectionsInDecisionRejected(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "mm-general"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	if !d.OverrideApplied {
		t.Fatal("override should apply")
	}
	if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "manual_override" {
		t.Fatalf("override elimination missing from Decision.Rejected: %+v", d.Rejected)
	}
}

func TestSelectRequestPreferenceDoesNotOverrideConfiguredPriority(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Preference = &RoutePreference{ModelAlias: "mm-general"}
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want cc-coding; request preference should not override configured priority", d.SelectedAlias)
	}
	found := false
	for _, r := range d.Reasons {
		if strings.Contains(r, "request model preference") && strings.Contains(r, "not selected") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons missing preference explanation: %v", d.Reasons)
	}
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
	if d.SelectedAlias != "b" {
		t.Fatalf("SelectedAlias = %q, want b", d.SelectedAlias)
	}
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
	if d.SelectedAlias != "a" {
		t.Fatalf("SelectedAlias = %q, want a", d.SelectedAlias)
	}
	if d.PerformanceInfluenced {
		t.Fatal("performance should not be reported as influence when request preference decided")
	}
}

func TestSelectAffinityDifferentSessionNoInfluence(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s2"
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	if d.AffinityInfluenced {
		t.Fatal("affinity influenced for different session")
	}
	if d.SelectedAlias == "mm-general" {
		t.Fatal("wrong-session affinity selected")
	}
}

func TestSelectAffinityEmptySessionIDIgnored(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.SessionID = "s1"
	state := SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: "", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour}
	d := sel.Select(req, state)
	if d.AffinityInfluenced {
		t.Fatal("empty affinity session id should not influence selection")
	}
	if d.SelectedAlias == "mm-general" {
		t.Fatal("empty affinity session id acted as wildcard")
	}
}

func TestSelectInvalidRequest(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(RouteRequest{}, SelectState{Now: selNow})
	if d.SelectedAlias != "" {
		t.Fatalf("SelectedAlias = %q", d.SelectedAlias)
	}
	if d.DecisionID == "" {
		t.Fatal("decision id should be set")
	}
	if len(d.Reasons) == 0 || !strings.Contains(d.Reasons[0], "request invalid") {
		t.Fatalf("reasons = %v", d.Reasons)
	}
}

func TestSelectPreferredCandidateIneligibleFallsThrough(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	q := availableQuota(selNow, "commandcode", "minimax")
	limit, rem, used := 100.0, 0.0, 100.0
	q["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem, Used: &used, ResetsAt: selNow.Add(time.Hour)}}, 0.1)
	d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	if d.SelectedAlias != "mm-general" {
		t.Fatalf("SelectedAlias = %q, want mm-general fallback", d.SelectedAlias)
	}
	if d.ConfiguredPriority == nil || d.ConfiguredPriority.Provider != "minimax" {
		t.Fatalf("ConfiguredPriority = %+v", d.ConfiguredPriority)
	}
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
		if !hasStage(d, s) {
			t.Fatalf("missing stage %q in %+v", s, d.Stages)
		}
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
	if d.SelectedAlias != "mm-general" {
		t.Fatalf("SelectedAlias = %q, want mm-general", d.SelectedAlias)
	}
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
	if d.OverrideApplied || d.SelectedAlias == "mm-general" {
		t.Fatalf("override bypassed exhaustion: %+v", d)
	}

	// cooldown
	h := healthyProviders(selNow, "commandcode", "minimax")
	h["minimax"] = quota.ProviderState{Provider: "minimax", Health: quota.Healthy, ObservedAt: selNow, CooldownUntil: selNow.Add(time.Minute)}
	d = sel.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
	if d.OverrideApplied || d.SelectedAlias == "mm-general" {
		t.Fatalf("override bypassed cooldown: %+v", d)
	}

	// disabled
	cfg2 := testConfig()
	m := cfg2.ModelAliases["mm-general"]
	m.Enabled = boolPtr(false)
	cfg2.ModelAliases["mm-general"] = m
	d = Selector{Catalogue: settings.Catalogue{ModelAliases: cfg2.ModelAliases, Routing: cfg2.Routing}}.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
	if d.OverrideApplied || d.SelectedAlias == "mm-general" {
		t.Fatalf("override selected disabled: %+v", d)
	}
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
		if d.SelectedAlias == "cc-coding" {
			t.Fatal("disabled selected")
		}
		if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "configuration" {
			t.Fatalf("disabled rejection: %+v ok=%v", r, ok)
		}
	}

	// Capabilities
	{
		req := baseRequest()
		req.Capabilities = &RequiredCapabilities{StructuredOutput: true}
		d := sel.Select(req, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		if r, ok := findRejected(d, "cc-fast"); !ok || r.Stage != "capabilities" {
			t.Fatalf("cap rejection: %+v ok=%v", r, ok)
		}
	}

	// Provider health unavailable
	{
		h := healthyProviders(selNow, "commandcode", "minimax")
		h["commandcode"] = quota.NewProviderState("commandcode", quota.Unavailable, quota.ReasonServerError, selNow)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
		if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "provider_health" {
			t.Fatalf("health rejection: %+v ok=%v", r, ok)
		}
	}

	// Unknown health
	{
		h := healthyProviders(selNow, "commandcode", "minimax")
		h["commandcode"] = quota.NewProviderState("commandcode", quota.HealthUnknown, quota.ReasonNotProbed, selNow)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
		if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "provider_health" {
			t.Fatalf("unknown health rejection: %+v ok=%v", r, ok)
		}
	}

	// Exhausted quota
	{
		q := availableQuota(selNow, "commandcode", "minimax")
		q["commandcode"] = quota.Observed("commandcode", "test", selNow, exhaustedWin, 0.1)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "subscription_quota" {
			t.Fatalf("exhausted rejection: %+v ok=%v", r, ok)
		}
	}

	// Unknown quota
	{
		q := availableQuota(selNow, "commandcode", "minimax")
		q["commandcode"] = quota.Failed("commandcode", "test", selNow, quota.MalformedResponse)
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "subscription_quota" {
			t.Fatalf("unknown quota rejection: %+v ok=%v", r, ok)
		}
	}

	// Cooldown
	{
		h := healthyProviders(selNow, "commandcode", "minimax")
		hs := quota.NewProviderState("commandcode", quota.Healthy, quota.ReasonNone, selNow)
		hs.CooldownUntil = selNow.Add(10 * time.Minute)
		h["commandcode"] = hs
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: h, QuotaMaxAge: time.Hour})
		if r, ok := findRejected(d, "cc-coding"); !ok || r.Stage != "temporary_exclusions" {
			t.Fatalf("cooldown rejection: %+v ok=%v", r, ok)
		}
	}

	// Affinity
	{
		d := sel.Select(RouteRequest{Version: RouteRequestVersionV1, SessionID: "s1", Capabilities: &RequiredCapabilities{}}, SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), Affinity: &Affinity{SessionID: "s1", Alias: "mm-general", ExpiresAt: selNow.Add(time.Hour)}, QuotaMaxAge: time.Hour})
		if !d.AffinityInfluenced || d.SelectedAlias != "mm-general" {
			t.Fatalf("affinity: %+v", d)
		}
	}

	// Configured preference
	{
		d := sel.Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour})
		if !d.ConfiguredPreferenceInfluenced || d.SelectedAlias != "cc-coding" {
			t.Fatalf("preference: %+v", d)
		}
	}

	// Performance tie-break
	{
		c := testConfig()
		c.Routing.Providers["commandcode"] = settings.ProviderRouting{Priority: 1, Models: map[string]int{"cc-coding": 1, "cc-fast": 1}}
		d := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: availableQuota(selNow, "commandcode", "minimax"), Health: healthyProviders(selNow, "commandcode", "minimax"), QuotaMaxAge: time.Hour, MaxPerformanceAge: time.Hour, Performance: map[string]PerformanceRecord{"cc-fast": {Alias: "cc-fast", Score: 1, Fresh: true, Sufficient: true, ObservedAt: selNow}, "cc-coding": {Alias: "cc-coding", Score: 10, Fresh: true, Sufficient: true, ObservedAt: selNow}}})
		if !d.PerformanceInfluenced || d.SelectedAlias != "cc-fast" {
			t.Fatalf("perf tie break: %+v", d)
		}
	}

	// Deterministic tie-break
	{
		c := defaultCatalogue()
		c.ModelAliases = map[string]settings.Model{"b": {Model: "p1/b", Enabled: boolPtr(true)}, "a": {Model: "p1/a", Enabled: boolPtr(true)}}
		q := availableQuota(selNow, "p1")
		d1 := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "p1"), QuotaMaxAge: time.Hour})
		d2 := (Selector{Catalogue: settings.Catalogue{ModelAliases: c.ModelAliases, Routing: c.Routing}}).Select(baseRequest(), SelectState{Now: selNow, Quota: q, Health: healthyProviders(selNow, "p1"), QuotaMaxAge: time.Hour})
		if d1.SelectedAlias != "a" || d1.DecisionID != d2.DecisionID {
			t.Fatalf("tie break: %q %q %q", d1.SelectedAlias, d1.DecisionID, d2.DecisionID)
		}
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
	if d.AffinityInfluenced {
		t.Fatal("expired affinity influenced selection")
	}
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want cc-coding from a fresh evaluation", d.SelectedAlias)
	}
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	if !strings.Contains(note, "expired") {
		t.Fatalf("affinity stage note = %q, want it to say the pin expired", note)
	}

	// The same inputs with an unexpired pin choose the other route, so expiry is
	// what decided — not a different candidate set.
	state.Affinity.ExpiresAt = selNow.Add(time.Minute)
	if d := sel.Select(req, state); d.SelectedAlias != "mm-general" || !d.AffinityInfluenced {
		t.Fatalf("live pin: SelectedAlias = %q influenced = %v", d.SelectedAlias, d.AffinityInfluenced)
	}
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
	if d.AffinityInfluenced || d.SelectedAlias == "mm-general" {
		t.Fatalf("pin without expiry decided the route: %+v", d)
	}
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
			if d.SelectedAlias != tc.want {
				t.Fatalf("SelectedAlias = %q, want %q", d.SelectedAlias, tc.want)
			}
			if held := tc.want == "mm-general"; d.AffinityInfluenced != held {
				t.Fatalf("AffinityInfluenced = %v, want %v", d.AffinityInfluenced, held)
			}
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
	if d.SelectedAlias != "cc-coding" || d.AffinityInfluenced {
		t.Fatalf("pin survived a disabled alias: %+v", d)
	}
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
	if d.SelectedAlias == "mm-general" || d.AffinityInfluenced {
		t.Fatalf("pinned session held on a cooled provider: %+v", d)
	}
	if d.SelectedAlias != "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want cc-coding while minimax is cooled", d.SelectedAlias)
	}
	if r, ok := rejectedCandidate(d.Rejected, "mm-general"); !ok || r.Transient {
		t.Fatalf("cooldown rejection = %+v transient=%v, want a durable rejection", r, r.Transient)
	}
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	if !strings.Contains(note, "durable") {
		t.Fatalf("affinity note = %q, want it to name the durable exclusion", note)
	}

	// After the cooldown expires, the session is pinned to whatever the policy
	// now selects. Here that is cc-coding, because the pin it used to hold is
	// gone from the stored route; selection is ordinary again.
	state.Health = healthyProviders(selNow, "commandcode", "minimax")
	state.Affinity = &Affinity{SessionID: "s1", Alias: "cc-coding", ExpiresAt: selNow.Add(time.Hour)}
	d = sel.Select(req, state)
	if d.SelectedAlias != "cc-coding" || !d.AffinityInfluenced {
		t.Fatalf("after cooldown: SelectedAlias = %q influenced = %v", d.SelectedAlias, d.AffinityInfluenced)
	}
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
	if d.AffinityInfluenced {
		t.Fatal("disabled pin influenced selection")
	}
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	if !strings.Contains(note, "configuration") || !strings.Contains(note, "disabled") {
		t.Fatalf("affinity note = %q, want it to name the configuration exclusion", note)
	}
}
