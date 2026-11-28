package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRoutingProviders(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/providers.yaml")))

	cfg, err := Load()
	require.NoError(t, err)
	want := map[string]ProviderRouting{
		"commandcode": {Priority: 1, Models: map[string]int{"coding": 1}},
		"minimax":     {Priority: 2, Models: map[string]int{"fast": 1}},
	}
	assert.Equal(t, want, cfg.Routing.Providers)
}

func TestLoadRoutingProvidersFromEnv(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__CODING__MODEL", "commandcode/stealth/glyph-cluster:free")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__PRIORITY", "1")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__CODING", "1")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__PRIORITY", "2")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__MODELS__FAST", "1")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.Routing.Providers["commandcode"].Priority)
	assert.Equal(t, 1, cfg.Routing.Providers["minimax"].Models["fast"])
}

func TestValidateRoutingRejectsInvalidConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		fixture string
		want    string
	}{
		{
			name:    "duplicate provider priority",
			fixture: "routing/duplicate-provider-priority.yaml",
			want:    "duplicates",
		},
		{
			name:    "duplicate model priority within provider",
			fixture: "routing/duplicate-model-priority.yaml",
			want:    "duplicates",
		},
		{
			name:    "unknown alias",
			fixture: "routing/unknown-alias.yaml",
			want:    "unknown catalogue alias",
		},
		{
			name:    "alias under wrong provider",
			fixture: "routing/alias-under-wrong-provider.yaml",
			want:    "belongs to provider",
		},
		{
			name:    "missing enabled alias",
			fixture: "routing/missing-enabled-alias.yaml",
			want:    "missing enabled catalogue alias",
		},
		{
			name:    "missing provider",
			fixture: "routing/missing-provider.yaml",
			want:    "missing provider",
		},
		{
			name:    "unknown provider with no enabled aliases",
			fixture: "routing/unknown-provider-no-aliases.yaml",
			want:    "no enabled catalogue aliases",
		},
		{
			name:    "non-positive provider priority",
			fixture: "routing/non-positive-provider-priority.yaml",
			want:    "priority must be a positive integer",
		},
		{
			name:    "non-positive model priority",
			fixture: "routing/non-positive-model-priority.yaml",
			want:    "positive integer",
		},
		{
			name:    "disabled alias referenced",
			fixture: "routing/disabled-alias-referenced.yaml",
			want:    "disabled catalogue alias",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, tt.fixture)))

			_, err := Load()
			require.Error(t, err, "expected error containing %q", tt.want)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestValidateRoutingAllowsDisabledAliasOmitted(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/disabled-alias-omitted.yaml")))

	cfg, err := Load()
	require.NoError(t, err)
	_, ok := cfg.Routing.Providers["p1"].Models["a2"]
	assert.False(t, ok, "disabled alias should not be required in routing order")
}

func TestValidateRoutingRequiresCompleteCoverageWhenConfigured(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/incomplete-provider-coverage.yaml")))

	_, err := Load()
	require.Error(t, err, "expected missing provider error")
	assert.ErrorContains(t, err, "missing provider")
	assert.ErrorContains(t, err, "p2")
}

func TestLoadRoutingProviderModelFromEnvHyphenatedAlias(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE-CODING__MODEL", "commandcode/stealth/glyph-cluster:free")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")
	// Literal hyphens in env var names are supported and map to hyphenated aliases.
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__PRIORITY", "1")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING", "1")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__PRIORITY", "2")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__MODELS__FAST", "1")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.Routing.Providers["commandcode"].Models["free-coding"], "models=%v", cfg.Routing.Providers["commandcode"].Models)
}

func TestLoadRoutingProviderModelFromEnvUnderscoreAliasNormalized(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE-CODING__MODEL", "commandcode/stealth/glyph-cluster:free")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")
	// Common env-var convention uses underscores; normalize only when the
	// hyphenated form is a known catalogue alias and the underscore form is not.
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__PRIORITY", "1")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING", "1")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__PRIORITY", "2")
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__MODELS__FAST", "1")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.Routing.Providers["commandcode"].Models["free-coding"], "models=%v", cfg.Routing.Providers["commandcode"].Models)
	_, ok := cfg.Routing.Providers["commandcode"].Models["free_coding"]
	assert.False(t, ok, "underscore key should have been normalized; models=%v", cfg.Routing.Providers["commandcode"].Models)
}

func TestLoadRoutingProviderModelEnvNoAmbiguousNormalize(t *testing.T) {
	cleanEnv(t)
	// Both forms exist as catalogue aliases - no silent normalization.
	t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/hyphen-and-underscore-aliases.yaml")))
	// Env override targets the underscore alias literally via exact key.
	t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING", "3")

	cfg, err := Load()
	require.NoError(t, err)
	// free_coding is a real alias, so env sets it directly; free-coding stays 1.
	assert.Equal(t, 3, cfg.Routing.Providers["commandcode"].Models["free_coding"], "models=%v", cfg.Routing.Providers["commandcode"].Models)
	assert.Equal(t, 1, cfg.Routing.Providers["commandcode"].Models["free-coding"], "models=%v", cfg.Routing.Providers["commandcode"].Models)
}
