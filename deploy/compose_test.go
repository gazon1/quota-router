package deploy

import (
	"os"
	"strings"
	"testing"
)

// TestComposeContract guards the #6 acceptance criteria that would otherwise
// regress silently: no dependency edge, a pinned image, and per-service limits.
func TestComposeContract(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	doc := string(raw)

	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"no depends_on edge anywhere", !strings.Contains(doc, "depends_on")},
		{"litellm pinned by digest", strings.Contains(doc, "litellm:v1.110.0@sha256:")},
		{"no floating tag", !strings.Contains(doc, ":latest") && !strings.Contains(doc, ":stable")},
		{"both services restart", strings.Count(doc, "restart: unless-stopped") == 2},
		{"both services have memory limits", strings.Count(doc, "mem_limit:") == 2},
		{"both services have healthchecks", strings.Count(doc, "healthcheck:") == 2},
		{"litellm bound to localhost", strings.Contains(doc, `"127.0.0.1:4000:4000"`)},
	} {
		if !c.ok {
			t.Errorf("compose.yaml: %s", c.name)
		}
	}
}
