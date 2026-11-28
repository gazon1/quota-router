package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal(t, tt.want, cfg.Catalogue().Candidates(tt.req), "Candidates(%+v)", tt.req)
		})
	}
}

func TestLoadRejectsInvalidCatalogue(t *testing.T) {
	for _, tt := range []struct{ name, fixture, want string }{
		{"alias with a slash", "catalogue/alias-with-slash.yaml", "litellm.model_aliases.a/b must not contain a slash"},
		{"model without a provider", "catalogue/model-without-provider.yaml", "litellm.model_aliases.fast.model must be <provider>/<model>"},
		{"empty model name", "catalogue/empty-model-name.yaml", "litellm.model_aliases.fast.model must be <provider>/<model>"},
		{"negative context limit", "catalogue/negative-context-limit.yaml", "litellm.model_aliases.fast.context_limit must be >= 0"},
		{"unknown capability", "catalogue/unknown-capability.yaml", "litellm.model_aliases.fast.capabilities.streaming"},
		{"empty task class", "catalogue/empty-task-class.yaml", "litellm.model_aliases.fast.tasks must not contain an empty task class"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, tt.fixture)))

			_, err := Load()
			require.Error(t, err, "expected a rejection naming %q", tt.want)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

// A duplicate alias is impossible in a mapping, so the case this covers is the
// same id under two aliases: legal, and each alias must stay its own entry.
func TestLoadAcceptsFullCatalogueEntry(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "catalogue/full-entry.yaml")))

	cfg, err := Load()
	require.NoError(t, err)
	// The short form still loads, so an existing file did not break.
	fast := cfg.Catalogue().ModelAliases["fast"]
	assert.Equal(t, "minimax/MiniMax-M2.7", fast.Model)
	assert.True(t, fast.IsEnabled())
	assert.Equal(t, 0, fast.ContextLimit)
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
	assert.Equal(t, want, cfg.LiteLLM.ModelAliases["coding"])
	assert.Equal(t, []string{"coding2", "fast"}, cfg.Catalogue().Candidates(Requirements{}), "want the enabled aliases only, disabled coding excluded")
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
		{"entry without id", `{"data":[{}]}`, http.StatusOK, ErrCatalogueUnavailable, ""},
		{"not json", `not a model list`, http.StatusOK, ErrCatalogueUnavailable, ""},
		{"unauthorized", `{"error":"bad key"}`, http.StatusUnauthorized, ErrCatalogueUnavailable, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/models", r.URL.Path)
				assert.Equal(t, "Bearer master-key", r.Header.Get("Authorization"))
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			cfg := catalogue(map[string]Model{"fast": {Model: "minimax/MiniMax-M2.7"}})
			cfg.LiteLLM.BaseURL = srv.URL
			cfg.LiteLLM.APIKey = Secret("master-key")

			err := cfg.VerifyCatalogue(context.Background(), srv.Client())
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.want == "" {
				require.NoError(t, err, "want a passing check")
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			// A mismatch is a configuration fault, not a transient outage.
			assert.NotErrorIs(t, err, ErrCatalogueUnavailable, "a mismatch must not read as unavailable")
		})
	}
}

// A proxy mounted under a path prefix is a base path, not a query: the check
// must keep it and append the endpoint below it.
func TestVerifyCatalogueKeepsBasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"id":"minimax/MiniMax-M2.7"}]}`))
	}))
	defer srv.Close()

	cfg := catalogue(map[string]Model{"fast": {Model: "minimax/MiniMax-M2.7"}})
	cfg.LiteLLM.BaseURL = srv.URL + "/litellm"
	cfg.LiteLLM.APIKey = Secret("master-key")

	require.NoError(t, cfg.VerifyCatalogue(context.Background(), srv.Client()), "want the check to pass")
	assert.Equal(t, "/litellm/v1/models", gotPath, "want the base path kept with the endpoint appended")
}

func TestVerifyCatalogueSkippedWithoutKey(t *testing.T) {
	cfg := catalogue(map[string]Model{"fast": {Model: "minimax/MiniMax-M2.7"}})
	// No key, no server, no request: the proxy needs its master key for the list.
	require.NoError(t, cfg.VerifyCatalogue(context.Background(), nil), "want the check skipped when litellm.api_key is unset")
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
	require.Error(t, err, "expected an error for a 500")
	assert.NotContains(t, err.Error(), "sk-secret-master", "leaked the master key")
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
	require.NoError(t, err)
	want := Model{
		Model:        "minimax/MiniMax-M2.7",
		Enabled:      ptr(false),
		ContextLimit: 262144,
		Capabilities: Capabilities{ToolCalling: CapabilitySupported},
		Tasks:        []string{"coding", "reasoning"},
	}
	assert.Equal(t, want, cfg.LiteLLM.ModelAliases["fast"])
}
