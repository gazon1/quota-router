package routing

import (
	"testing"
	"time"

	"github.com/gazon1/quota-router/internal/quota"
	"github.com/gazon1/quota-router/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// candidateByAliasEval finds one evaluated candidate.
func candidateByAliasEval(ev []CandidateEvaluation, alias string) (CandidateEvaluation, bool) {
	for _, c := range ev {
		if c.Alias == alias {
			return c, true
		}
	}
	return CandidateEvaluation{}, false
}

func TestCandidatesCoverEveryAliasInEvaluationOrder(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)

	require.Len(t, d.Candidates, 3)
	assert.Equal(t, "cc-coding", d.Candidates[0].Alias)
	assert.Equal(t, "cc-fast", d.Candidates[1].Alias)
	assert.Equal(t, "mm-general", d.Candidates[2].Alias)
	for i, c := range d.Candidates {
		assert.Equal(t, i+1, c.Ordinal)
	}
}

func TestCandidatesCarrySelectedAndRejectedCodes(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)

	selected, ok := candidateByAliasEval(d.Candidates, d.SelectedAlias)
	require.True(t, ok)
	assert.True(t, selected.Selected)
	assert.True(t, selected.Eligible)
	assert.Equal(t, ReasonSelectedByPreference, selected.ReasonCode)

	// The other two survived every hard constraint and lost the ranking.
	for _, alias := range []string{"cc-fast", "mm-general"} {
		c, ok := candidateByAliasEval(d.Candidates, alias)
		require.True(t, ok)
		assert.False(t, c.Selected)
		assert.True(t, c.Eligible)
		assert.Equal(t, ReasonEligibleNotSelected, c.ReasonCode)
		assert.Empty(t, c.EliminatedStage)
	}
}

func TestCandidatesRejectedByHardStageAreNotEligible(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	quotaState := availableQuota(selNow, "commandcode", "minimax")
	// commandcode quota is exhausted: both its aliases are eliminated.
	limit, rem := 100.0, 0.0
	quotaState["commandcode"] = quota.Observed("commandcode", "test", selNow, []quota.Window{{Name: "5h", Limit: &limit, Remaining: &rem}}, 0.1)
	state := SelectState{
		Now:         selNow,
		Quota:       quotaState,
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)

	assert.Equal(t, "mm-general", d.SelectedAlias)
	for _, alias := range []string{"cc-coding", "cc-fast"} {
		c, ok := candidateByAliasEval(d.Candidates, alias)
		require.True(t, ok)
		assert.False(t, c.Selected)
		assert.False(t, c.Eligible)
		assert.Equal(t, "subscription_quota", c.EliminatedStage)
		assert.Equal(t, ReasonQuotaExhausted, c.ReasonCode)
		assert.Equal(t, string(quota.Exhausted), c.QuotaState)
	}
}

func TestCandidatesOverrideMarksOthersNotSelectedButEligible(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	req := baseRequest()
	req.Override = &ManualOverride{ModelAlias: "mm-general"}
	d := sel.Select(req, state)

	require.Equal(t, "mm-general", d.SelectedAlias)
	selected, ok := candidateByAliasEval(d.Candidates, "mm-general")
	require.True(t, ok)
	assert.True(t, selected.Selected)
	assert.Equal(t, ReasonSelectedByOverride, selected.ReasonCode)

	other, ok := candidateByAliasEval(d.Candidates, "cc-coding")
	require.True(t, ok)
	assert.False(t, other.Selected)
	assert.True(t, other.Eligible, "an override skips a preference stage, not a hard constraint")
	assert.Equal(t, ReasonNotSelectedByOverride, other.ReasonCode)
}

func TestCandidatesUnknownObservationsStayUnknown(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	// commandcode quota failed to be observed; minimax health is stale.
	quotaState := availableQuota(selNow, "commandcode", "minimax")
	quotaState["commandcode"] = quota.Failed("commandcode", "test", selNow, quota.TransientFetchFailure)
	healthState := healthyProviders(selNow, "commandcode", "minimax")
	healthState["minimax"] = quota.NewProviderState("minimax", quota.Healthy, quota.ReasonNone, selNow.Add(-48*time.Hour))
	state := SelectState{
		Now:         selNow,
		Quota:       quotaState,
		Health:      healthState,
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)

	cc, ok := candidateByAliasEval(d.Candidates, "cc-coding")
	require.True(t, ok)
	assert.Equal(t, string(quota.QuotaUnknown), cc.QuotaState)
	assert.Equal(t, string(quota.TransientFetchFailure), cc.QuotaUnknownReason)
	assert.NotEqual(t, string(quota.Available), cc.QuotaState)

	mm, ok := candidateByAliasEval(d.Candidates, "mm-general")
	require.True(t, ok)
	assert.Equal(t, string(quota.HealthUnknown), mm.HealthState)
	assert.Equal(t, string(quota.StaleObservation), mm.HealthReason)
	assert.NotEqual(t, string(quota.Healthy), mm.HealthState)
}

func TestCandidatesWithoutObservationCarryNothing(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode"),
		Health:      healthyProviders(selNow, "commandcode"),
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)

	mm, ok := candidateByAliasEval(d.Candidates, "mm-general")
	require.True(t, ok)
	assert.Empty(t, mm.QuotaState)
	assert.Nil(t, mm.QuotaObservedAt)
	assert.Nil(t, mm.QuotaResetAt)
	assert.Empty(t, mm.HealthState)
	assert.Nil(t, mm.HealthObservedAt)
}

func TestCandidatesNoEligibleCandidateSelectsNothing(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	// Both providers are unavailable: every candidate is eliminated and none is
	// selected, but every candidate is still reported.
	healthState := map[string]quota.ProviderState{
		"commandcode": quota.NewProviderState("commandcode", quota.Unavailable, quota.ReasonServerError, selNow),
		"minimax":     quota.NewProviderState("minimax", quota.Unavailable, quota.ReasonServerError, selNow),
	}
	state.Health = healthState
	d := sel.Select(baseRequest(), state)

	assert.Empty(t, d.SelectedAlias)
	require.Len(t, d.Candidates, 3)
	for _, c := range d.Candidates {
		assert.False(t, c.Selected)
		assert.False(t, c.Eligible)
		assert.Equal(t, ReasonProviderUnavailable, c.ReasonCode)
		assert.Equal(t, "provider_health", c.EliminatedStage)
	}
}

func TestCandidatesInvalidRequestEvaluatesNothing(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	req := baseRequest()
	req.SessionID = ""
	d := sel.Select(req, state)

	assert.Empty(t, d.Candidates)
	assert.Empty(t, d.SelectedAlias)
}

// The selected candidate carries the decision's explanation beside its code,
// so a stored candidate row explains itself without the payload.
func TestCandidatesSelectedCarriesTheExplanation(t *testing.T) {
	cfg := testConfig()
	sel := Selector{Catalogue: settings.Catalogue{ModelAliases: cfg.ModelAliases, Routing: cfg.Routing}}
	state := SelectState{
		Now:         selNow,
		Quota:       availableQuota(selNow, "commandcode", "minimax"),
		Health:      healthyProviders(selNow, "commandcode", "minimax"),
		QuotaMaxAge: time.Hour,
	}
	d := sel.Select(baseRequest(), state)

	selected, ok := candidateByAliasEval(d.Candidates, d.SelectedAlias)
	require.True(t, ok)
	assert.NotEmpty(t, selected.Reason, "the selected candidate must explain itself")
	assert.Equal(t, d.PrimaryReason(), selected.Reason)
}
