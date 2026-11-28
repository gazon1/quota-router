package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The catalogue entry and the provider the environment declares in most of
// these tests, spelled with the documented _H_ escape.
const (
	envAliasFreeCoding = "QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE_H_CODING__MODEL=commandcode/stealth/glyph-cluster:free"
	envProviderRank    = "QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__PRIORITY=1"
)

// envWith makes the loader read exactly the given environment variables, in
// the given order. The process environment is left alone, so a test states
// which variables exist and in which order the loader sees them. A test that
// also needs a configuration file sets ConfigFileEnv after this call.
func envWith(t *testing.T, vars ...string) {
	t.Helper()
	cleanEnv(t)
	prev := envSource
	envSource = func() []string { return vars }
	t.Cleanup(func() { envSource = prev })
}

// TestEnvEncoding covers the ways one name segment can address a hyphenated
// alias.
func TestEnvEncoding(t *testing.T) {
	t.Run("literal hyphen", func(t *testing.T) {
		envWith(t,
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE-CODING__MODEL=commandcode/stealth/glyph-cluster:free",
			envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING=1",
		)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 1}, cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("hyphen escape", func(t *testing.T) {
		envWith(t, envAliasFreeCoding, envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING=1")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 1}, cfg.Routing.Providers["commandcode"].Models)
		assert.Contains(t, cfg.LiteLLM.ModelAliases, "free-coding")
	})

	t.Run("case is folded", func(t *testing.T) {
		envWith(t,
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__Free_H_Coding__MODEL=commandcode/stealth/glyph-cluster:free",
			envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__Free_H_Coding=1",
		)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 1}, cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("double underscore nests", func(t *testing.T) {
		envWith(t, "QUOTA_ROUTER_SERVER__ADDR=:9090")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, ":9090", cfg.Server.Addr)
	})

	t.Run("literal underscore alias", func(t *testing.T) {
		envWith(t,
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE_CODING__MODEL=commandcode/stealth/glyph-cluster:free",
			envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING=1",
		)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free_coding": 1}, cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("the escape is reserved", func(t *testing.T) {
		// "_h_" always decodes, so the alias free_h_coding is out of reach
		// from the environment: the name addresses free-coding instead.
		envWith(t,
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE_H_CODING__MODEL=commandcode/stealth/glyph-cluster:free",
			envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING=1",
		)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Contains(t, cfg.LiteLLM.ModelAliases, "free-coding")
		assert.NotContains(t, cfg.LiteLLM.ModelAliases, "free_h_coding")
	})
}

// TestDecodeEnvHyphens covers the escape itself: it is decoded in every
// segment and never touches the nesting delimiter.
func TestDecodeEnvHyphens(t *testing.T) {
	assert.Equal(t, []string{"routing", "providers", "p", "models", "free-coding"},
		decodeEnvHyphens("routing__providers__p__models__free_h_coding"))
	assert.Equal(t, []string{"litellm", "model_aliases", "free-h_coding"},
		decodeEnvHyphens("litellm__model_aliases__free_h_h_coding"))
	assert.Equal(t, []string{"h"}, decodeEnvHyphens("h"))
	assert.Equal(t, []string{"server", "read_header_timeout"},
		decodeEnvHyphens("server__read_header_timeout"))
}

// TestResolveAlias covers which catalogue alias one routing key addresses.
func TestResolveAlias(t *testing.T) {
	known := map[string]bool{"free-coding": true, "free_coding": true}

	assert.Equal(t, "free-coding", resolveAlias("free-coding", known), "the exact name wins")
	assert.Equal(t, "free_coding", resolveAlias("free_coding", known), "the exact name wins")
	assert.Equal(t, "other", resolveAlias("other", known), "an unknown alias is left alone")
	assert.Equal(t, "free-coding", resolveAlias("free_coding", map[string]bool{"free-coding": true}),
		"an underscore falls back to the hyphenated alias")
}

// TestEnvAliasResolution covers the resolution rules through the loader.
func TestEnvAliasResolution(t *testing.T) {
	t.Run("underscore falls back to the hyphenated alias", func(t *testing.T) {
		envWith(t, envAliasFreeCoding, envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING=1")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 1}, cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("the exact alias wins when both forms exist", func(t *testing.T) {
		envWith(t,
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE_H_CODING__MODEL=commandcode/stealth/glyph-cluster:free",
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE_CODING__MODEL=commandcode/other:free",
			envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING=2",
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING=1",
		)
		// The underscore key is the alias free_coding, not the hyphenated one:
		// a fallback would address free-coding twice.
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 2, "free_coding": 1},
			cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("an unknown alias reaches validation", func(t *testing.T) {
		envWith(t,
			"QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FREE_H_CODING__MODEL=commandcode/stealth/glyph-cluster:free",
			envProviderRank,
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__NOPE=1",
		)
		_, err := Load()
		require.Error(t, err)
		assert.ErrorContains(t, err, `references unknown catalogue alias "nope"`)
	})
}

// TestEnvSourceIsolation covers the rule that the environment is normalized
// and the file is not.
func TestEnvSourceIsolation(t *testing.T) {
	t.Run("a file key stays literal", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/literal-underscore-key.yaml")))
		// The file says free_coding and the catalogue says free-coding. The
		// file key is not rewritten, so validation names the alias that the
		// file does not have.
		_, err := Load()
		require.Error(t, err)
		assert.ErrorContains(t, err, `references unknown catalogue alias "free_coding"`)
	})

	t.Run("a file alias containing the escape stays literal", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/literal-file-keys.yaml")))
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free_coding": 1, "free_h_coding": 2},
			cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("the environment overrides the file for one alias", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/hyphen-alias.yaml")))
		t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING", "2")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 2}, cfg.Routing.Providers["commandcode"].Models)
	})

	t.Run("the file and the environment do not conflict across sources", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "routing/hyphen-and-underscore-aliases.yaml")))
		// The escaped spelling addresses the hyphenated alias and the file
		// keeps its underscore alias: two keys, one source each, no conflict.
		t.Setenv("QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING", "5")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"free-coding": 5, "free_coding": 2},
			cfg.Routing.Providers["commandcode"].Models)
	})
}

// TestEnvCollisions covers the startup error for variables that address one
// configuration path.
func TestEnvCollisions(t *testing.T) {
	const (
		literal = "QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING=1"
		escaped = "QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING=2"
		under   = "QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING=3"
	)

	t.Run("two variables", func(t *testing.T) {
		envWith(t, envAliasFreeCoding, envProviderRank, literal, escaped)
		_, err := Load()
		require.EqualError(t, err, "environment: conflicting variables: "+
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING, "+
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING "+
			"all set routing.providers.commandcode.models.free-coding")
	})

	t.Run("three variables", func(t *testing.T) {
		envWith(t, envAliasFreeCoding, envProviderRank, literal, escaped, under)
		_, err := Load()
		require.EqualError(t, err, "environment: conflicting variables: "+
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING, "+
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING, "+
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING "+
			"all set routing.providers.commandcode.models.free-coding")
	})

	t.Run("the error does not depend on the environment order", func(t *testing.T) {
		const want = "environment: conflicting variables: " +
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING, " +
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_CODING, " +
			"QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE_H_CODING " +
			"all set routing.providers.commandcode.models.free-coding"
		for _, order := range [][]string{
			{envAliasFreeCoding, envProviderRank, literal, escaped, under},
			{under, escaped, literal, envProviderRank, envAliasFreeCoding},
		} {
			envWith(t, order...)
			_, err := Load()
			require.EqualError(t, err, want)
		}
	})

	t.Run("nothing is loaded", func(t *testing.T) {
		envWith(t, "QUOTA_ROUTER_SERVER__ADDR=:9090", literal, escaped)
		cfg, err := Load()
		require.Error(t, err)
		assert.Equal(t, Config{}, cfg)
	})
}

// TestEnvCompatibility covers the rules that must keep working: the
// deprecated flat names, the nested names and the empty value.
func TestEnvCompatibility(t *testing.T) {
	t.Run("the deprecated flat name still works", func(t *testing.T) {
		envWith(t, "QUOTA_ROUTER_ADDR=:9090")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, ":9090", cfg.Server.Addr)
	})

	t.Run("the nested name wins over the deprecated flat name", func(t *testing.T) {
		envWith(t, "QUOTA_ROUTER_ADDR=:9090", "QUOTA_ROUTER_SERVER__ADDR=:7070")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, ":7070", cfg.Server.Addr)
	})

	t.Run("an empty value is unset", func(t *testing.T) {
		envWith(t, "QUOTA_ROUTER_SERVER__ADDR=")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, Default().Server.Addr, cfg.Server.Addr)
	})

	t.Run("the defaults need no environment", func(t *testing.T) {
		envWith(t)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, Default().Server.Addr, cfg.Server.Addr)
	})
}
