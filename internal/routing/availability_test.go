package routing

import (
	"strings"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/config"
)

// weekday names the zone-local weekday of an instant, so a test that dates a
// window reads as the window an operator would write.
func inWindow(days []string, start, end string) *config.Unavailable {
	return &config.Unavailable{
		Timezone: "UTC",
		Windows:  []config.UnavailableWindow{{Days: days, Start: start, End: end}},
	}
}

func withUnavailable(cfg config.Config, alias string, u *config.Unavailable) config.Config {
	m := cfg.LiteLLM.ModelAliases[alias]
	m.Unavailable = u
	cfg.LiteLLM.ModelAliases[alias] = m
	return cfg
}

func selectState(now time.Time) SelectState {
	return SelectState{
		Now:         now,
		Quota:       availableQuota(now, "commandcode", "minimax"),
		Health:      healthyProviders(now, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
}

// selNow is Friday 2026-11-27 12:00 UTC.
func TestSelectExcludesModelInsideWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Config: cfg}

	d := sel.Select(baseRequest(), selectState(selNow))
	// cc-fast is the next route in configured order; the window removed only the
	// preferred model.
	if d.SelectedAlias != "cc-fast" {
		t.Fatalf("SelectedAlias = %q, want the next eligible route", d.SelectedAlias)
	}
	r, ok := findRejected(d, "cc-coding")
	if !ok || r.Stage != "model_availability" {
		t.Fatalf("rejection = %+v ok=%v, want a model_availability exclusion", r, ok)
	}
	if r.Transient {
		t.Fatal("a configured window says something about the route, so it must not be transient")
	}
	if !strings.Contains(r.Reason, "model unavailable by configured schedule (UTC friday 11:00-13:00)") {
		t.Fatalf("reason = %q, want the stable schedule explanation", r.Reason)
	}
	if !hasStage(d, "model_availability") {
		t.Fatalf("missing stage in %+v", d.Stages)
	}
}

// The window is evaluated at the decision's instant, so the next decision after
// it ends selects the model again with no scheduler, reload or restart.
func TestSelectEligibleImmediatelyAfterWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Config: cfg}

	// Inclusive start, exclusive end: these two instants are the boundary.
	if d := sel.Select(baseRequest(), selectState(time.Date(2026, time.November, 27, 11, 0, 0, 0, time.UTC))); d.SelectedAlias != "cc-fast" {
		t.Fatalf("at the inclusive start SelectedAlias = %q, want the window to apply", d.SelectedAlias)
	}
	if d := sel.Select(baseRequest(), selectState(time.Date(2026, time.November, 27, 13, 0, 0, 0, time.UTC))); d.SelectedAlias != "cc-coding" {
		t.Fatalf("at the exclusive end SelectedAlias = %q, want cc-coding eligible again", d.SelectedAlias)
	}
	// And it stays eligible for an ordinary decision afterwards.
	if d := sel.Select(baseRequest(), selectState(selNow.Add(2*time.Hour))); d.SelectedAlias != "cc-coding" {
		t.Fatalf("after the window SelectedAlias = %q, want cc-coding", d.SelectedAlias)
	}
}

func TestSelectWindowUsesConfiguredZone(t *testing.T) {
	// Noon in Yerevan (UTC+4) is 08:00 UTC. The window names local hours, so the
	// same instant is outside a 09:00-10:00 window but inside a 12:00-13:00 one.
	instant := time.Date(2026, time.November, 28, 8, 0, 0, 0, time.UTC) // Saturday
	cfg := withUnavailable(testConfig(), "cc-coding", &config.Unavailable{
		Timezone: "Asia/Yerevan",
		Windows:  []config.UnavailableWindow{{Days: []string{"saturday"}, Start: "12:00", End: "13:00"}},
	})
	sel := Selector{Config: cfg}
	if d := sel.Select(baseRequest(), selectState(instant)); d.SelectedAlias != "cc-fast" {
		t.Fatalf("SelectedAlias = %q, want the Yerevan noon window to exclude cc-coding", d.SelectedAlias)
	}

	cfg = withUnavailable(testConfig(), "cc-coding", &config.Unavailable{
		Timezone: "Asia/Yerevan",
		Windows:  []config.UnavailableWindow{{Days: []string{"saturday"}, Start: "08:00", End: "09:00"}},
	})
	sel = Selector{Config: cfg}
	if d := sel.Select(baseRequest(), selectState(instant)); d.SelectedAlias != "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want the UTC hour not to be read as local", d.SelectedAlias)
	}
}

// A pin never makes an ineligible model eligible: the window is applied before
// the affinity stage, so a session on a model that entered its window moves.
func TestSelectAffinityCannotBypassWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Config: cfg}
	req := baseRequest()
	state := selectState(selNow)
	state.Affinity = &Affinity{SessionID: req.SessionID, Alias: "cc-coding", ExpiresAt: selNow.Add(time.Hour)}

	d := sel.Select(req, state)
	if d.SelectedAlias != "cc-fast" || d.AffinityInfluenced {
		t.Fatalf("SelectedAlias = %q influenced = %v, want the session moved off the window", d.SelectedAlias, d.AffinityInfluenced)
	}
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	if !strings.Contains(note, "model_availability") || !strings.Contains(note, "unavailable by configured schedule") {
		t.Fatalf("affinity note = %q, want it to name the time-window exclusion", note)
	}
}

func TestSelectOverrideCannotBypassWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Config: cfg}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "cc-coding"}

	d := sel.Select(req, selectState(selNow))
	if d.OverrideApplied {
		t.Fatal("override was honored for a model inside its unavailable window")
	}
	if d.SelectedAlias == "cc-coding" {
		t.Fatalf("SelectedAlias = %q, want the override target rejected", d.SelectedAlias)
	}
	if d.SelectedAlias != "cc-fast" {
		t.Fatalf("SelectedAlias = %q, want normal selection to continue", d.SelectedAlias)
	}
	var rejected bool
	for _, reason := range d.Reasons {
		if strings.Contains(reason, `manual override "cc-coding" rejected`) && strings.Contains(reason, "not eligible after hard constraints") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("reasons = %v, want an explanation that the override target was ineligible", d.Reasons)
	}
}

// Every configured route inside its window leaves the existing no-eligible-model
// answer in place: an empty decision, which the server reports as 409.
func TestSelectNoEligibleWhenEveryModelIsInWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	cfg = withUnavailable(cfg, "cc-fast", inWindow([]string{"friday"}, "11:00", "13:00"))
	cfg = withUnavailable(cfg, "mm-general", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Config: cfg}

	d := sel.Select(baseRequest(), selectState(selNow))
	if d.SelectedAlias != "" {
		t.Fatalf("SelectedAlias = %q, want none", d.SelectedAlias)
	}
	if d.DecisionID == "" {
		t.Fatal("decision id should be set")
	}
	for _, alias := range []string{"cc-coding", "cc-fast", "mm-general"} {
		if r, ok := findRejected(d, alias); !ok || r.Stage != "model_availability" {
			t.Fatalf("%s rejection = %+v ok=%v", alias, r, ok)
		}
	}
}

// Identical configuration and instant produce the identical decision, including
// the exclusion text an operator reads from the audit log.
func TestSelectWindowExplanationIsStable(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Config: cfg}
	state := selectState(selNow)

	d1 := sel.Select(baseRequest(), state)
	d2 := sel.Select(baseRequest(), state)
	if d1.DecisionID != d2.DecisionID {
		t.Fatalf("decision id not deterministic: %q vs %q", d1.DecisionID, d2.DecisionID)
	}
	r1, _ := findRejected(d1, "cc-coding")
	r2, _ := findRejected(d2, "cc-coding")
	if r1.Reason != r2.Reason {
		t.Fatalf("reason not stable: %q vs %q", r1.Reason, r2.Reason)
	}
}

// A schedule that does not currently apply still changes the configuration the
// decision was made under, so the audit identifier must change with it.
func TestDecisionIDCoversConfiguredWindows(t *testing.T) {
	plain := testConfig()
	windowed := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"monday"}, "11:00", "13:00"))
	// selNow is a Friday, so neither decision is excluded by the window itself.
	d1 := Selector{Config: plain}.Select(baseRequest(), selectState(selNow))
	d2 := Selector{Config: windowed}.Select(baseRequest(), selectState(selNow))
	if d1.SelectedAlias != d2.SelectedAlias {
		t.Fatalf("selection differs: %q vs %q", d1.SelectedAlias, d2.SelectedAlias)
	}
	if d1.DecisionID == d2.DecisionID {
		t.Fatal("decision id must depend on the configured schedule")
	}
}
