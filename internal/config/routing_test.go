package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadRoutingProviders(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  model_aliases:
    fast: minimax/MiniMax-M2.7
    coding:
      model: commandcode/stealth/glyph-cluster:free
routing:
  providers:
    commandcode:
      priority: 1
      models:
        coding: 1
    minimax:
      priority: 2
      models:
        fast: 1
`))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := map[string]ProviderRouting{
		"commandcode": {Priority: 1, Models: map[string]int{"coding": 1}},
		"minimax":     {Priority: 2, Models: map[string]int{"fast": 1}},
	}
	if !reflect.DeepEqual(cfg.Routing.Providers, want) {
		t.Fatalf("Routing.Providers = %+v, want %+v", cfg.Routing.Providers, want)
	}
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
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Routing.Providers["commandcode"].Priority != 1 {
		t.Fatalf("commandcode priority = %d, want 1", cfg.Routing.Providers["commandcode"].Priority)
	}
	if cfg.Routing.Providers["minimax"].Models["fast"] != 1 {
		t.Fatalf("minimax fast priority = %d, want 1", cfg.Routing.Providers["minimax"].Models["fast"])
	}
}

func TestValidateRoutingRejectsInvalidConfig(t *testing.T) {
	base := `
litellm:
  model_aliases:
    a1:
      model: p1/m1
    a2:
      model: p1/m2
    b1:
      model: p2/m1
`
	for _, tt := range []struct {
		name    string
		routing string
		want    string
	}{
		{
			name: "duplicate provider priority",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 2}
    p2:
      priority: 1
      models: {b1: 1}
`,
			want: "duplicates",
		},
		{
			name: "duplicate model priority within provider",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 1}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "duplicates",
		},
		{
			name: "unknown alias",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 2, missing: 3}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "unknown catalogue alias",
		},
		{
			name: "alias under wrong provider",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 2, b1: 3}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "belongs to provider",
		},
		{
			name: "missing enabled alias",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "missing enabled catalogue alias",
		},
		{
			name: "missing provider",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 2}
`,
			want: "missing provider",
		},
		{
			name: "unknown provider with no enabled aliases",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 2}
    p2:
      priority: 2
      models: {b1: 1}
    p3:
      priority: 3
      models: {ghost: 1}
`,
			want: "no enabled catalogue aliases",
		},
		{
			name: "non-positive provider priority",
			routing: `
routing:
  providers:
    p1:
      priority: 0
      models: {a1: 1, a2: 2}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "priority must be a positive integer",
		},
		{
			name: "non-positive model priority",
			routing: `
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 0, a2: 2}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "positive integer",
		},
		{
			name: "disabled alias referenced",
			routing: `
litellm:
  model_aliases:
    a1:
      model: p1/m1
    a2:
      model: p1/m2
      enabled: false
    b1:
      model: p2/m1
routing:
  providers:
    p1:
      priority: 1
      models: {a1: 1, a2: 2}
    p2:
      priority: 2
      models: {b1: 1}
`,
			want: "disabled catalogue alias",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			body := base + tt.routing
			if strings.Contains(tt.routing, "litellm:") {
				body = tt.routing
			}
			t.Setenv(ConfigFileEnv, writeConfig(t, body))

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q missing %q", err, tt.want)
			}
		})
	}
}

func TestValidateRoutingAllowsDisabledAliasOmitted(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  model_aliases:
    a1:
      model: p1/m1
    a2:
      model: p1/m2
      enabled: false
routing:
  providers:
    p1:
      priority: 1
      models:
        a1: 1
`))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := cfg.Routing.Providers["p1"].Models["a2"]; ok {
		t.Fatal("disabled alias should not be required in routing order")
	}
}

func TestValidateRoutingRequiresCompleteCoverageWhenConfigured(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  model_aliases:
    a1: p1/m1
    b1: p2/m1
routing:
  providers:
    p1:
      priority: 1
      models:
        a1: 1
`))

	_, err := Load()
	if err == nil {
		t.Fatal("expected missing provider error")
	}
	if !strings.Contains(err.Error(), "missing provider") || !strings.Contains(err.Error(), "p2") {
		t.Fatalf("error %q missing provider p2", err)
	}
}
