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

	// Issue #6 forbids one edge specifically: LiteLLM must not wait on the
	// controller. Other edges (a future database) stay legal.
	if dep := dependsOn(services["litellm"]); strings.Contains(dep, "quota-controller") {
		t.Errorf("compose.yaml: litellm depends_on %q; it must start with the controller down", dep)
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
	if got := publishedPorts(services["litellm"]); len(got) != 1 || got[0] != "127.0.0.1:4000:4000" {
		t.Errorf("compose.yaml: litellm must publish exactly 127.0.0.1:4000:4000, got %v", got)
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

// dependsOn returns the services the block depends on, space-joined, or "".
// Both Compose syntaxes are read: `depends_on: [a, b]` and the block list
// under a bare `depends_on:`.
func dependsOn(block string) string {
	var names []string
	in := false
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "depends_on:") {
			inline := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "depends_on:")), "[]")
			names = append(names, strings.Split(inline, ",")...)
			in = inline == ""
			continue
		}
		if !in {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "- "):
			names = append(names, strings.TrimPrefix(trimmed, "- "))
		case strings.HasSuffix(trimmed, ":") && strings.HasPrefix(line, "      "):
			// Long syntax: depends_on: {service: {condition: ...}}
			names = append(names, strings.TrimSuffix(trimmed, ":"))
		default:
			return strings.Join(strings.Fields(strings.Join(names, " ")), " ")
		}
	}
	return strings.Join(strings.Fields(strings.Join(names, " ")), " ")
}

// publishedPorts returns the port mappings of a block, in file order.
func publishedPorts(block string) []string {
	var ports []string
	in := false
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "ports:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			break
		}
		ports = append(ports, strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), `"'`))
	}
	return ports
}

// TestLiteLLMConfigContract guards the provider wiring verified in issue #8.
// Each assertion encodes a failure measured against the pinned image, so a
// regression fails here instead of surfacing as a confusing runtime error.
func TestLiteLLMConfigContract(t *testing.T) {
	raw, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	doc := stripComments(string(raw))

	// LiteLLM resolves a request by model_name, so a bare alias is an unknown
	// model: `/v1/models` lists it, but POSTing it returns 400.
	if !strings.Contains(doc, "model_name: minimax/MiniMax-M2.7") {
		t.Error("litellm-config.yaml: minimax model_name must be minimax/MiniMax-M2.7")
	}
	// Free-tier ids carry a slash and a colon. LiteLLM routes them as-is, but a
	// quoted or escaped form would change the string a client must send, so the
	// exact ids are asserted. The paid model they replaced is gone.
	if !strings.Contains(doc, "model_name: commandcode/stealth/glyph-cluster:free") ||
		!strings.Contains(doc, "model_name: commandcode/recursive/flint-v1.0:free") {
		t.Error("litellm-config.yaml: expected the glyph-cluster:free and flint-v1.0:free model names")
	}
	if strings.Contains(doc, "gpt-5.6-sol") {
		t.Error("litellm-config.yaml: gpt-5.6-sol was replaced by the free-tier models")
	}
	// Verified: without disable_cooldowns, one rejected key marks the deployment
	// unhealthy and the next request answers 429, which reads as rate limiting.
	if !strings.Contains(doc, "disable_cooldowns: true") {
		t.Error("litellm-config.yaml: disable_cooldowns must be true or a 401 resurfaces as a 429")
	}
	// Verified: LiteLLM appends /v1/messages to an anthropic api_base, so a
	// trailing /v1 produces .../v1/v1/messages and a 404.
	for _, block := range strings.Split(doc, "- model_name:")[1:] {
		if !strings.Contains(block, "model: anthropic/") {
			continue
		}
		var base string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "api_base: "); ok {
				base = v
			}
		}
		if strings.HasSuffix(base, "/v1") {
			t.Errorf("litellm-config.yaml: anthropic api_base %q must not end in /v1", base)
		}
	}
	// Inference and quota are different hosts. A quota host used as api_base
	// would make every model request a quota lookup.
	if strings.Contains(doc, "www.minimax.io") {
		t.Error("litellm-config.yaml: api_base must use the inference host api.minimax.io, not the quota host www.minimax.io")
	}
	// Every credential is injected, never committed.
	if strings.Contains(doc, "sk-") {
		t.Error("litellm-config.yaml: a literal credential is committed; use os.environ/")
	}
}

// TestEnvExampleCoversConfig reads the variables litellm-config.yaml injects and
// requires each to be declared in .env.example, so setup cannot miss one.
func TestEnvExampleCoversConfig(t *testing.T) {
	cfg, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	env, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	declared := map[string]bool{}
	for _, line := range strings.Split(string(env), "\n") {
		if name, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(name, "#") {
			declared[name] = true
		}
	}
	for _, line := range strings.Split(string(cfg), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "api_key: os.environ/")
		if !ok {
			continue
		}
		if !declared[name] {
			t.Errorf(".env.example does not declare %q, which litellm-config.yaml requires", name)
		}
	}
}

// TestDockerignoreExcludesSecrets guards the controller's build context: the
// Dockerfile runs COPY . . from the repository root, so an unignored .env or
// config.yaml would reach the builder and its layer cache.
func TestDockerignoreExcludesSecrets(t *testing.T) {
	raw, err := os.ReadFile("../.dockerignore")
	if err != nil {
		t.Fatalf("read ../.dockerignore: %v", err)
	}
	var patterns []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}

	// Docker matches a pattern from any path segment, so ".env" covers both
	// the root file and deploy/.env; "deploy/.env" alone would not.
	for _, want := range []string{".env", "config.yaml", "*.db"} {
		ok := false
		for _, p := range patterns {
			if p == want {
				ok = true
			}
		}
		if !ok {
			t.Errorf(".dockerignore does not exclude %q; the controller's build context would carry it", want)
		}
	}
}
