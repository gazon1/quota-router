package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
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
	cfg := readLiteLLMConfig(t)

	// The full tuple per model, decoded rather than matched as text: quoting a
	// value is legal YAML and must not fail the build, while a changed upstream
	// provider, base URL or credential name must.
	want := map[string]struct{ model, apiBase, apiKey string }{
		"minimax/MiniMax-M2.7": {
			model:   "openai/MiniMax-M2.7",
			apiBase: "https://api.minimax.io/v1",
			apiKey:  "os.environ/MINIMAX_TOKEN_PLAN_KEY",
		},
		"commandcode/recursive/flint-v1.0:free": {
			model:   "openai/recursive/flint-v1.0:free",
			apiBase: "https://api.commandcode.ai/provider/v1",
			apiKey:  "os.environ/COMMAND_CODE_API_KEY",
		},
		"commandcode/stealth/glyph-cluster:free": {
			model:   "openai/stealth/glyph-cluster:free",
			apiBase: "https://api.commandcode.ai/provider/v1",
			apiKey:  "os.environ/COMMAND_CODE_API_KEY",
		},
		"commandcode/deepseek-v4.1-flash": {
			model: "anthropic/deepseek/deepseek-v4.1-flash",
			// LiteLLM appends /v1/messages to an anthropic api_base, so a trailing
			// /v1 produces .../v1/v1/messages and a 404. Verified on the image.
			apiBase: "https://api.commandcode.ai/provider",
			apiKey:  "os.environ/COMMAND_CODE_API_KEY",
		},
	}
	if len(cfg.models) != len(want) {
		t.Errorf("litellm-config.yaml: %d models configured, expected %d", len(cfg.models), len(want))
	}
	// A duplicate would otherwise pay for a missing model in the count check.
	seen := map[string]int{}
	for _, m := range cfg.models {
		seen[m.name]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("litellm-config.yaml: model %q is configured %d times", name, n)
		}
	}
	for _, m := range cfg.models {
		want, ok := want[m.name]
		if !ok {
			t.Errorf("litellm-config.yaml: unexpected model %q", m.name)
			continue
		}
		if m.model != want.model {
			t.Errorf("litellm-config.yaml: %s model = %q, want %q", m.name, m.model, want.model)
		}
		if m.apiBase != want.apiBase {
			t.Errorf("litellm-config.yaml: %s api_base = %q, want %q", m.name, m.apiBase, want.apiBase)
		}
		if m.apiKey != want.apiKey {
			t.Errorf("litellm-config.yaml: %s api_key = %q, want %q (never a literal)", m.name, m.apiKey, want.apiKey)
		}
		// An anthropic provider with no api_base would fall through to the vendor's
		// default host, which is not this account's provider.
		if strings.HasPrefix(m.model, "anthropic/") && m.apiBase == "" {
			t.Errorf("litellm-config.yaml: %s needs an explicit api_base", m.name)
		}
	}

	// Inference and quota are different hosts. A quota host used as api_base
	// would make every model request a quota lookup.
	for _, m := range cfg.models {
		if strings.Contains(m.apiBase, "www.minimax.io") {
			t.Errorf("litellm-config.yaml: %s api_base uses the quota host, not the inference host", m.name)
		}
	}
	// Verified: without disable_cooldowns, one rejected provider key marks the
	// deployment unhealthy and the next request answers 429, which reads as rate
	// limiting; num_retries must stay 0 so a retry cannot repeat a tool call.
	if !cfg.router.DisableCooldowns {
		t.Error("litellm-config.yaml: router_settings.disable_cooldowns must be true")
	}
	if cfg.router.NumRetries != 0 {
		t.Errorf("litellm-config.yaml: router_settings.num_retries = %d, want 0", cfg.router.NumRetries)
	}
}

// liteLLMConfig is the slice of the proxy config these tests assert on.
type liteLLMConfig struct {
	raw    string
	models []liteLLMModel
	router struct {
		DisableCooldowns bool `yaml:"disable_cooldowns"`
		NumRetries       int  `yaml:"num_retries"`
	}
}

type liteLLMModel struct {
	name    string
	model   string
	apiBase string
	apiKey  string
}

// readLiteLLMConfig decodes litellm-config.yaml with the YAML parser the project
// already depends on through koanf, so a quoted or reflowed value is judged by
// meaning rather than by spelling.
func readLiteLLMConfig(t *testing.T) liteLLMConfig {
	t.Helper()
	raw, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	var doc struct {
		ModelList []struct {
			ModelName  string `yaml:"model_name"`
			Parameters struct {
				Model   string `yaml:"model"`
				APIBase string `yaml:"api_base"`
				APIKey  string `yaml:"api_key"`
			} `yaml:"litellm_params"`
		} `yaml:"model_list"`
		Router struct {
			DisableCooldowns bool `yaml:"disable_cooldowns"`
			NumRetries       int  `yaml:"num_retries"`
		} `yaml:"router_settings"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse litellm-config.yaml: %v", err)
	}
	cfg := liteLLMConfig{raw: string(raw), router: doc.Router}
	for _, m := range doc.ModelList {
		cfg.models = append(cfg.models, liteLLMModel{
			name: m.ModelName, model: m.Parameters.Model,
			apiBase: m.Parameters.APIBase, apiKey: m.Parameters.APIKey,
		})
	}
	return cfg
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
