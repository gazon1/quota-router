// The fixtures in testdata/ are real response bodies captured from the pinned
// deployment image with an OpenAI-shaped upstream behind it. They exist so the
// usage field names this package depends on are asserted against measured bytes,
// not assumed from the OpenAI spec.
//
// Recapture with deploy/interop/run-interop.sh, which starts the pinned image,
// a recording upstream, and writes both files. Upgrade note: a LiteLLM bump that
// renames usage fields fails these tests, which is the point.
package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gazon1/quota-router/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUsageFieldsOnPinnedLiteLLM proves Reply.Attempt (and therefore Report)
// finds the token counts in a response the pinned image actually produced.
func TestUsageFieldsOnPinnedLiteLLM(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "litellm_v1.110.0_chat_completion.json"))
	require.NoError(t, err, "read fixture")
	// The bytes must come from the proxy, not from this test's own expectation:
	// a fixture rewritten by hand would quietly stop proving anything.
	assert.Contains(t, string(raw), `"object":"chat.completion"`, "fixture is not a LiteLLM chat completion")

	usage := agent.ParseUsage(raw)
	require.NotNil(t, usage, "usage object not found: prompt_tokens/completion_tokens moved")
	require.NotNil(t, usage.InputTokens)
	assert.Equal(t, 11, *usage.InputTokens, "want 11 (prompt_tokens)")
	require.NotNil(t, usage.OutputTokens)
	assert.Equal(t, 7, *usage.OutputTokens, "want 7 (completion_tokens)")
}

// TestUsageFieldsAbsentIsNotZero keeps "the proxy reported no usage" distinct
// from "the proxy reported zero", which the report contract depends on.
func TestUsageFieldsAbsentIsNotZero(t *testing.T) {
	for _, body := range []string{
		`{"id":"x","object":"chat.completion","choices":[]}`,
		`{"id":"x","usage":null}`,
		`{"id":"x","usage":{}}`,
		`not json at all`,
	} {
		usage := agent.ParseUsage([]byte(body))
		if usage != nil {
			assert.Nil(t, usage.InputTokens, "body %q produced a count instead of a missing one", body)
			assert.Nil(t, usage.OutputTokens, "body %q produced a count instead of a missing one", body)
		}
	}
	// A reported zero is a reported value, not a missing one.
	usage := agent.ParseUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	require.NotNil(t, usage, "a reported zero was dropped")
	require.NotNil(t, usage.InputTokens)
	assert.Equal(t, 0, *usage.InputTokens, "a reported zero was dropped")
}
