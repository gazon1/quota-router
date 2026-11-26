package config

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// catalogue builds a Config with one entry per alias, bypassing the loader so a
// filtering case states only the fields it cares about.
func catalogue(models map[string]Model) Config {
	cfg := Default()
	cfg.LiteLLM.ModelAliases = models
	return cfg
}

func TestCandidatesFiltersCapabilitiesAndDisabled(t *testing.T) {
	supported := Capabilities{Streaming: CapabilitySupported, ToolCalling: CapabilitySupported}
	cfg := catalogue(map[string]Model{
		"tools":    {Model: "p/tools", Capabilities: supported, Tasks: []string{"coding"}},
		"streams":  {Model: "p/streams", Capabilities: Capabilities{Streaming: CapabilitySupported}},
		"off":      {Model: "p/off", Enabled: ptr(false), Capabilities: supported, Tasks: []string{"coding"}},
		"unproven": {Model: "p/unproven", Capabilities: Capabilities{ToolCalling: CapabilityUnsupported}, Tasks: []string{"coding"}},
		"silent":   {Model: "p/silent", Tasks: []string{"coding"}},
		"other":    {Model: "q/tools", Capabilities: supported, Tasks: []string{"reasoning"}},
	})

	for _, tt := range []struct {
		name string
		req  Requirements
		want []string
	}{
		{"nothing required", Requirements{}, []string{"other", "silent", "streams", "tools", "unproven"}},
		{"streaming", Requirements{Streaming: true}, []string{"other", "streams", "tools"}},
		{"tool calling", Requirements{ToolCalling: true}, []string{"other", "tools"}},
		// An unverified capability is not a claim, so the model stays out.
		{"unverified is excluded", Requirements{Streaming: true, ToolCalling: true}, []string{"other", "tools"}},
		{"task class", Requirements{Tasks: []string{"coding"}}, []string{"silent", "tools", "unproven"}},
		{"task class and capability", Requirements{ToolCalling: true, Tasks: []string{"reasoning"}}, []string{"other"}},
		{"no match", Requirements{Tasks: []string{"translation"}}, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.Candidates(tt.req); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Candidates(%+v) = %v, want %v", tt.req, got, tt.want)
			}
		})
	}
}

func TestSatisfiedByIsConservative(t *testing.T) {
	unknown := Model{Model: "p/m"}
	for _, req := range []Requirements{
		{Streaming: true}, {ToolCalling: true}, {StructuredOutput: true}, {Tasks: []string{"coding"}},
	} {
		if req.SatisfiedBy(unknown) {
			t.Fatalf("%+v was satisfied by a model that never claimed it", req)
		}
	}
	// Enabled is opt-out, so a model without the field is usable.
	if !unknown.IsEnabled() {
		t.Error("a model without the enabled field must be enabled")
	}
}

func TestLoadRejectsInvalidCatalogue(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"alias with a slash", `
litellm:
  model_aliases:
    a/b:
      model: minimax/MiniMax-M2.7
`, "litellm.model_aliases.a/b must not contain a slash"},
		{"model without a provider", `
litellm:
  model_aliases:
    fast:
      model: nope
`, "litellm.model_aliases.fast.model must be <provider>/<model>"},
		{"empty model name", `
litellm:
  model_aliases:
    fast:
      model: minimax/
`, "litellm.model_aliases.fast.model must be <provider>/<model>"},
		{"negative context limit", `
litellm:
  model_aliases:
    fast:
      model: minimax/MiniMax-M2.7
      context_limit: -1
`, "litellm.model_aliases.fast.context_limit must be >= 0"},
		{"unknown capability", `
litellm:
  model_aliases:
    fast:
      model: minimax/MiniMax-M2.7
      capabilities:
        streaming: maybe
`, "litellm.model_aliases.fast.capabilities.streaming"},
		{"empty task class", `
litellm:
  model_aliases:
    fast:
      model: minimax/MiniMax-M2.7
      tasks: [" "]
`, "litellm.model_aliases.fast.tasks must not contain an empty task class"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv(ConfigFileEnv, writeConfig(t, tt.body))

			_, err := Load()
			if err == nil {
				t.Fatalf("expected a rejection naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q missing %q", err, tt.want)
			}
		})
	}
}

// A duplicate alias is impossible in a mapping, so the case this covers is the
// same id under two aliases: legal, and each alias must stay its own entry.
func TestLoadAcceptsFullCatalogueEntry(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  base_url: http://127.0.0.1:4000
  model_aliases:
    fast: minimax/MiniMax-M2.7
    coding:
      model: commandcode/stealth/glyph-cluster:free
      enabled: false
      context_limit: 256000
      capabilities:
        streaming: supported
        tool_calling: supported
        structured_output: unsupported
      tasks: [coding]
    coding2:
      model: commandcode/stealth/glyph-cluster:free
`))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// The short form still loads, so an existing file did not break.
	if got := cfg.LiteLLM.ModelAliases["fast"]; got.Model != "minimax/MiniMax-M2.7" || !got.IsEnabled() || got.ContextLimit != 0 {
		t.Fatalf("fast = %+v, want the model and nothing claimed", got)
	}
	want := Model{
		Model:        "commandcode/stealth/glyph-cluster:free",
		Enabled:      ptr(false),
		ContextLimit: 256000,
		Capabilities: Capabilities{
			Streaming:        CapabilitySupported,
			ToolCalling:      CapabilitySupported,
			StructuredOutput: CapabilityUnsupported,
		},
		Tasks: []string{"coding"},
	}
	if got := cfg.LiteLLM.ModelAliases["coding"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("coding = %+v, want %+v", got, want)
	}
	if got := cfg.Candidates(Requirements{}); !reflect.DeepEqual(got, []string{"coding2", "fast"}) {
		t.Fatalf("Candidates = %v, want the enabled aliases only, disabled coding excluded", got)
	}
}

func TestVerifyCatalogue(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		status  int
		wantErr error
		want    string
	}{
		{"matching", `{"data":[{"id":"minimax/MiniMax-M2.7"}]}`, http.StatusOK, nil, ""},
		{"unknown alias", `{"data":[{"id":"minimax/MiniMax-M3"}]}`, http.StatusOK, nil, "litellm.model_aliases.fast"},
		{"empty list", `{"data":[]}`, http.StatusOK, ErrCatalogueUnavailable, ""},
		{"not json", `not a model list`, http.StatusOK, ErrCatalogueUnavailable, ""},
		{"unauthorized", `{"error":"bad key"}`, http.StatusUnauthorized, ErrCatalogueUnavailable, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %q, want /v1/models", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer master-key" {
					t.Errorf("Authorization = %q, want the configured master key", got)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			cfg := catalogue(map[string]Model{"fast": {Model: "minimax/MiniMax-M2.7"}})
			cfg.LiteLLM.BaseURL = srv.URL
			cfg.LiteLLM.APIKey = Secret("master-key")

			err := cfg.VerifyCatalogue(context.Background(), srv.Client())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if tt.want == "" {
				if err != nil {
					t.Fatalf("error = %v, want a passing check", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to name %q", err, tt.want)
			}
			// A mismatch is a configuration fault, not a transient outage.
			if errors.Is(err, ErrCatalogueUnavailable) {
				t.Fatalf("error = %v, but a mismatch must not read as unavailable", err)
			}
		})
	}
}

func TestVerifyCatalogueSkippedWithoutKey(t *testing.T) {
	cfg := catalogue(map[string]Model{"fast": {Model: "minimax/MiniMax-M2.7"}})
	// No key, no server, no request: the proxy needs its master key for the list.
	if err := cfg.VerifyCatalogue(context.Background(), nil); err != nil {
		t.Fatalf("error = %v, want the check skipped when litellm.api_key is unset", err)
	}
}

// The catalogue must not leak the proxy master key through an error string.
func TestVerifyCatalogueErrorHidesKey(t *testing.T) {
	var doc any = map[string]string{"data": "x"}
	body, _ := json.Marshal(doc)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := catalogue(map[string]Model{"fast": {Model: "minimax/MiniMax-M2.7"}})
	cfg.LiteLLM.BaseURL = srv.URL
	cfg.LiteLLM.APIKey = Secret("sk-secret-master")

	err := cfg.VerifyCatalogue(context.Background(), srv.Client())
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	if strings.Contains(err.Error(), "sk-secret-master") {
		t.Fatalf("error %q leaked the master key", err)
	}
}

func ptr[T any](v T) *T { return &v }

// The catalogue is settable through the environment, and the README documents the
// exact variable names: one test asserts the shape rather than trusting it.
func TestLoadEnvSetsCatalogueFields(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__ENABLED", "false")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__CONTEXT_LIMIT", "262144")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__CAPABILITIES__TOOL_CALLING", "supported")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__TASKS", "coding,reasoning")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := Model{
		Model:        "minimax/MiniMax-M2.7",
		Enabled:      ptr(false),
		ContextLimit: 262144,
		Capabilities: Capabilities{ToolCalling: CapabilitySupported},
		Tasks:        []string{"coding", "reasoning"},
	}
	if got := cfg.LiteLLM.ModelAliases["fast"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("fast = %+v, want %+v", got, want)
	}
}
