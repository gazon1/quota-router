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
	"strings"
	"testing"

	"github.com/gazon1/quota-router/internal/agent"
)

// TestUsageFieldsOnPinnedLiteLLM proves Reply.Attempt (and therefore Report)
// finds the token counts in a response the pinned image actually produced.
func TestUsageFieldsOnPinnedLiteLLM(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "litellm_v1.110.0_chat_completion.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The bytes must come from the proxy, not from this test's own expectation:
	// a fixture rewritten by hand would quietly stop proving anything.
	if !strings.Contains(string(raw), `"object":"chat.completion"`) {
		t.Fatal("fixture is not a LiteLLM chat completion")
	}

	usage := agent.ParseUsage(raw)
	if usage == nil {
		t.Fatal("usage object not found: prompt_tokens/completion_tokens moved")
	}
	if usage.InputTokens == nil || *usage.InputTokens != 11 {
		t.Fatalf("input tokens = %v, want 11 (prompt_tokens)", usage.InputTokens)
	}
	if usage.OutputTokens == nil || *usage.OutputTokens != 7 {
		t.Fatalf("output tokens = %v, want 7 (completion_tokens)", usage.OutputTokens)
	}
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
		if usage := agent.ParseUsage([]byte(body)); usage != nil {
			if usage.InputTokens != nil || usage.OutputTokens != nil {
				t.Fatalf("body %q produced a count instead of a missing one: %+v", body, usage)
			}
		}
	}
	// A reported zero is a reported value, not a missing one.
	usage := agent.ParseUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 0 {
		t.Fatalf("a reported zero was dropped: %+v", usage)
	}
}
