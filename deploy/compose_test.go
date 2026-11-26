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
	services, ok := serviceBlocks(stripComments(doc))
	if !ok {
		t.Fatal("compose.yaml: no top-level services: mapping")
	}

	for _, name := range []string{"quota-controller", "litellm"} {
		if _, ok := services[name]; !ok {
			t.Fatalf("compose.yaml: service %q missing", name)
		}
	}
	// Values are asserted, not just field presence: issue #6 names 128 MiB for
	// the controller, so a silent raise or drop must fail the build.
	for _, c := range []struct {
		what string
		ok   bool
	}{
		{"controller restart policy", strings.Contains(services["quota-controller"], "restart: unless-stopped")},
		{"controller 128m memory limit", strings.Contains(services["quota-controller"], "mem_limit: 128m")},
		{"controller cpu limit", strings.Contains(services["quota-controller"], "cpus:")},
		{"controller health probes /health", strings.Contains(services["quota-controller"], "127.0.0.1:8080/health")},
		{"litellm restart policy", strings.Contains(services["litellm"], "restart: unless-stopped")},
		{"litellm memory limit", strings.Contains(services["litellm"], "mem_limit:")},
		{"litellm cpu limit", strings.Contains(services["litellm"], "cpus:")},
		{"litellm health probes liveliness", strings.Contains(services["litellm"], "127.0.0.1:4000/health/liveliness")},
	} {
		if !c.ok {
			t.Errorf("compose.yaml: missing %s", c.what)
		}
	}

	if strings.Contains(stripComments(doc), "depends_on") {
		t.Error("compose.yaml: a depends_on edge exists; LiteLLM must start with the controller down")
	}
	if strings.Contains(stripComments(doc), ":latest") || strings.Contains(stripComments(doc), ":stable") {
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

// stripComments drops inline comments so a comment that mentions a value cannot
// satisfy an assertion the real value fails, e.g. `mem_limit: 64m  # was 128m`.
func stripComments(doc string) string {
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if j := strings.Index(line, " #"); j >= 0 {
			lines[i] = strings.TrimRight(line[:j], " ")
		}
	}
	return strings.Join(lines, "\n")
}

// serviceBlocks returns one block per service name declared under the
// top-level services: mapping. Names outside that mapping are ignored.
func serviceBlocks(doc string) (map[string]string, bool) {
	blocks := map[string]string{}
	inServices, sawServices := false, false
	name := ""
	var body []string
	flush := func() {
		if name != "" {
			blocks[name] = strings.Join(body, "\n")
		}
	}
	for _, line := range strings.Split(doc, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			// A new top-level key ends the services mapping.
			flush()
			name, body = "", nil
			inServices = line == "services:"
			sawServices = sawServices || inServices
			continue
		}
		if !inServices {
			continue
		}
		// A service key is indented exactly two spaces and ends with a colon.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(line, ":") {
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
	return blocks, sawServices
}
