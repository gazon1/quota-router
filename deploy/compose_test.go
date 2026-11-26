package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestComposeContract guards the #6 acceptance criteria that would otherwise
// regress silently. It reads the file as text: the repository has no YAML
// dependency and this is not worth adding one for.
func TestComposeContract(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	doc := string(raw)
	services := serviceBlocks(t, doc)

	for _, name := range []string{"quota-controller", "litellm"} {
		if _, ok := services[name]; !ok {
			t.Fatalf("compose.yaml: service %q missing", name)
		}
	}
	// Every service block ends at the next top-level "  <name>:" key, so a
	// per-service assertion cannot be satisfied by another service's field.
	for _, name := range []string{"quota-controller", "litellm"} {
		body := services[name]
		for _, c := range []struct {
			what string
			ok   bool
		}{
			{"restart policy", strings.Contains(body, "restart: unless-stopped")},
			{"memory limit", strings.Contains(body, "mem_limit:")},
			{"cpu limit", strings.Contains(body, "cpus:")},
			{"healthcheck", strings.Contains(body, "healthcheck:")},
		} {
			if !c.ok {
				t.Errorf("service %q: missing %s", name, c.what)
			}
		}
	}

	if strings.Contains(doc, "depends_on") {
		t.Error("compose.yaml: a depends_on edge exists; LiteLLM must start with the controller down")
	}
	if strings.Contains(doc, ":latest") || strings.Contains(doc, ":stable") {
		t.Error("compose.yaml: floating image tag (:latest or :stable)")
	}
	// A complete digest, not just the "sha256:" prefix: a truncated digest is
	// an invalid reference that would only fail at run time.
	digest := regexp.MustCompile(`litellm:v1\.110\.0@sha256:[0-9a-f]{64}\b`)
	if !digest.MatchString(services["litellm"]) {
		t.Error("compose.yaml: litellm image is not pinned by a full v1.110.0 tag@sha256:<64 hex> digest")
	}
	if !strings.Contains(services["litellm"], `"127.0.0.1:4000:4000"`) {
		t.Error("compose.yaml: litellm port is not bound to localhost")
	}
	// The controller must not publish a host port at all.
	if strings.Contains(services["quota-controller"], "ports:") {
		t.Error("compose.yaml: quota-controller publishes a host port; it must stay internal")
	}
}

// serviceBlocks splits the services mapping into one block per service name.
func serviceBlocks(t *testing.T, doc string) map[string]string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	blocks := map[string]string{}
	name := ""
	var body []string
	flush := func() {
		if name != "" {
			blocks[name] = strings.Join(body, "\n")
		}
	}
	for _, line := range lines {
		// A service key is indented exactly two spaces and ends with a colon.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(line, ":") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			flush()
			name = strings.TrimSuffix(strings.TrimSpace(line), ":")
			body = nil
			continue
		}
		if name != "" {
			body = append(body, line)
		}
	}
	flush()
	return blocks
}
