package routing

import (
	"strings"
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// weekday names the zone-local weekday of an instant, so a test that dates a
// window reads as the window an operator would write.
func inWindow(days []string, start, end string) *settings.Unavailable {
	return &settings.Unavailable{
		Timezone: "UTC",
		Windows:  []settings.UnavailableWindow{{Days: days, Start: start, End: end}},
	}
}

func withUnavailable(cfg settings.Catalogue, alias string, u *settings.Unavailable) settings.Catalogue {
	m := cfg.ModelAliases[alias]
	m.Unavailable = u
	cfg.ModelAliases[alias] = m
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
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	d := sel.Select(baseRequest(), selectState(selNow))
	// cc-fast is the next route in configured order; the window removed only the
	// preferred model.
	assert.Equal(t, "cc-fast", d.SelectedAlias, "want the next eligible route")
	r, ok := findRejected(d, "cc-coding")
	require.True(t, ok, "want a model_availability exclusion")
	assert.Equal(t, "model_availability", r.Stage)
	assert.False(t, r.Transient, "a configured window says something about the route, so it must not be transient")
	assert.Contains(t, r.Reason, "model unavailable by configured schedule (UTC friday 11:00-13:00)", "want the stable schedule explanation")
	assert.True(t, hasStage(d, "model_availability"), "missing stage in %+v", d.Stages)
}

// The window is evaluated at the decision's instant, so the next decision after
// it ends selects the model again with no scheduler, reload or restart.
func TestSelectEligibleImmediatelyAfterWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	// Inclusive start, exclusive end: these two instants are the boundary.
	d := sel.Select(baseRequest(), selectState(time.Date(2026, time.November, 27, 11, 0, 0, 0, time.UTC)))
	assert.Equal(t, "cc-fast", d.SelectedAlias, "want the window to apply")
	d = sel.Select(baseRequest(), selectState(time.Date(2026, time.November, 27, 13, 0, 0, 0, time.UTC)))
	assert.Equal(t, "cc-coding", d.SelectedAlias, "want cc-coding eligible again")
	// And it stays eligible for an ordinary decision afterwards.
	d = sel.Select(baseRequest(), selectState(selNow.Add(2*time.Hour)))
	assert.Equal(t, "cc-coding", d.SelectedAlias, "want cc-coding")
}

func TestSelectWindowUsesConfiguredZone(t *testing.T) {
	// Noon in Yerevan (UTC+4) is 08:00 UTC. The window names local hours, so the
	// same instant is outside a 09:00-10:00 window but inside a 12:00-13:00 one.
	instant := time.Date(2026, time.November, 28, 8, 0, 0, 0, time.UTC) // Saturday
	cfg := withUnavailable(testConfig(), "cc-coding", &settings.Unavailable{
		Timezone: "Asia/Yerevan",
		Windows:  []settings.UnavailableWindow{{Days: []string{"saturday"}, Start: "12:00", End: "13:00"}},
	})
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d := sel.Select(baseRequest(), selectState(instant))
	assert.Equal(t, "cc-fast", d.SelectedAlias, "want the Yerevan noon window to exclude cc-coding")

	cfg = withUnavailable(testConfig(), "cc-coding", &settings.Unavailable{
		Timezone: "Asia/Yerevan",
		Windows:  []settings.UnavailableWindow{{Days: []string{"saturday"}, Start: "08:00", End: "09:00"}},
	})
	sel = Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	d = sel.Select(baseRequest(), selectState(instant))
	assert.Equal(t, "cc-coding", d.SelectedAlias, "want the UTC hour not to be read as local")
}

// A pin never makes an ineligible model eligible: the window is applied before
// the affinity stage, so a session on a model that entered its window moves.
func TestSelectAffinityCannotBypassWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	state := selectState(selNow)
	state.Affinity = &Affinity{SessionID: req.SessionID, Alias: "cc-coding", ExpiresAt: selNow.Add(time.Hour)}

	d := sel.Select(req, state)
	assert.Equal(t, "cc-fast", d.SelectedAlias, "want the session moved off the window")
	assert.False(t, d.AffinityInfluenced, "want the session moved off the window")
	var note string
	for _, s := range d.Stages {
		if s.Stage == "session_affinity" {
			note = s.Note
		}
	}
	assert.Contains(t, note, "model_availability", "want it to name the time-window exclusion")
	assert.Contains(t, note, "unavailable by configured schedule", "want it to name the time-window exclusion")
}

func TestSelectOverrideCannotBypassWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "cc-coding"}

	d := sel.Select(req, selectState(selNow))
	assert.False(t, d.OverrideApplied, "override was honored for a model inside its unavailable window")
	assert.NotEqual(t, "cc-coding", d.SelectedAlias, "want the override target rejected")
	assert.Equal(t, "cc-fast", d.SelectedAlias, "want normal selection to continue")
	var rejected bool
	for _, reason := range d.Reasons {
		if strings.Contains(reason, `manual override "cc-coding" rejected`) && strings.Contains(reason, "not eligible after hard constraints") {
			rejected = true
		}
	}
	assert.True(t, rejected, "reasons = %v, want an explanation that the override target was ineligible", d.Reasons)
}

// Every configured route inside its window leaves the existing no-eligible-model
// answer in place: an empty decision, which the server reports as 409.
func TestSelectNoEligibleWhenEveryModelIsInWindow(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	cfg = withUnavailable(cfg, "cc-fast", inWindow([]string{"friday"}, "11:00", "13:00"))
	cfg = withUnavailable(cfg, "mm-general", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}

	d := sel.Select(baseRequest(), selectState(selNow))
	assert.Equal(t, "", d.SelectedAlias, "want none")
	assert.NotEmpty(t, d.DecisionID, "decision id should be set")
	for _, alias := range []string{"cc-coding", "cc-fast", "mm-general"} {
		r, ok := findRejected(d, alias)
		require.True(t, ok, "%s must be rejected", alias)
		assert.Equal(t, "model_availability", r.Stage, "%s rejection", alias)
	}
}

// Identical configuration and instant produce the identical decision, including
// the exclusion text an operator reads from the audit log.
func TestSelectWindowExplanationIsStable(t *testing.T) {
	cfg := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"friday"}, "11:00", "13:00"))
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := selectState(selNow)

	d1 := sel.Select(baseRequest(), state)
	d2 := sel.Select(baseRequest(), state)
	assert.Equal(t, d2.DecisionID, d1.DecisionID, "decision id not deterministic")
	r1, _ := findRejected(d1, "cc-coding")
	r2, _ := findRejected(d2, "cc-coding")
	assert.Equal(t, r2.Reason, r1.Reason, "reason not stable")
}

// A schedule that does not currently apply still changes the configuration the
// decision was made under, so the audit identifier must change with it.
func TestDecisionIDCoversConfiguredWindows(t *testing.T) {
	plain := testConfig()
	windowed := withUnavailable(testConfig(), "cc-coding", inWindow([]string{"monday"}, "11:00", "13:00"))
	// selNow is a Friday, so neither decision is excluded by the window itself.
	d1 := Selector{Catalogue: settings.Catalogue{ModelAliases: plain.ModelAliases, Routing: plain.Routing}}.Select(baseRequest(), selectState(selNow))
	d2 := Selector{Catalogue: settings.Catalogue{ModelAliases: windowed.ModelAliases, Routing: windowed.Routing}}.Select(baseRequest(), selectState(selNow))
	assert.Equal(t, d2.SelectedAlias, d1.SelectedAlias, "selection differs")
	assert.NotEqual(t, d2.DecisionID, d1.DecisionID, "decision id must depend on the configured schedule")
}
