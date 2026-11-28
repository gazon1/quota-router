package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A base URL is the root a provider adapter appends its endpoint path to, so a
// query string or a fragment in it would swallow the appended path and send
// the request somewhere else. The loader must reject such a URL instead of
// failing later at provider initialization.
func TestBaseURLQueryAndFragmentRejected(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"litellm query string", "https://litellm.example.com?token=abc", "litellm.base_url: must not contain a query string"},
		{"litellm bare query", "https://litellm.example.com?", "litellm.base_url: must not contain a query string"},
		{"litellm fragment", "https://litellm.example.com#section", "litellm.base_url: must not contain a fragment"},
		{"litellm bare fragment", "https://litellm.example.com/x#", "litellm.base_url: must not contain a fragment"},
		{"litellm credentials", "https://user:pass@litellm.example.com", "litellm.base_url: must not embed credentials"},
		{"provider query string", "https://api.minimax.io/v1?token=abc", "providers.minimax.base_url: must not contain a query string"},
		{"provider fragment", "https://api.minimax.io/v1#frag", "providers.minimax.base_url: must not contain a fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			if strings.HasPrefix(tt.want, "providers.") {
				t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", tt.url)
			} else {
				t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", tt.url)
			}

			_, err := Load()
			require.Error(t, err, "expected a rejection naming %q", tt.want)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

// A base URL without a query or a fragment stays valid, including one that
// carries the expected path.
func TestBaseURLWithoutQueryOrFragmentAccepted(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"http", "http://litellm.example.com"},
		{"https", "https://litellm.example.com"},
		{"https with path", "https://www.minimax.io/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", tt.url)

			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, tt.url, cfg.LiteLLM.BaseURL)
		})
	}
}
