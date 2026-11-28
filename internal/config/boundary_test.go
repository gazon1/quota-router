package config

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/gazon1/quota-router/internal/settings"
)

func TestCatalogueBoundary(t *testing.T) {
	cleanEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cat := cfg.Catalogue()
	if cat.Routing.LowQuotaThreshold != cfg.Routing.LowQuotaThreshold {
		t.Fatalf("threshold = %v, want %v", cat.Routing.LowQuotaThreshold, cfg.Routing.LowQuotaThreshold)
	}
	if len(cat.ModelAliases) != len(cfg.LiteLLM.ModelAliases) {
		t.Fatalf("aliases len = %d, want %d", len(cat.ModelAliases), len(cfg.LiteLLM.ModelAliases))
	}
	// Canonical types: a settings.Model is assignable from config.Model (alias).
	var _ settings.Model = cfg.LiteLLM.ModelAliases["x"]
	var _ settings.Routing = cfg.Routing
	var _ settings.Secret = cfg.Auth.Token
}

func TestRoutingAndAgentDoNotImportConfig(t *testing.T) {
	for _, pkg := range []string{"github.com/gazon1/quota-router/internal/routing/...", "github.com/gazon1/quota-router/internal/agent"} {
		out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{.Imports}}", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pkg, err, out)
		}
		if strings.Contains(string(out), "\"github.com/gazon1/quota-router/internal/config\"") {
			t.Fatalf("%s imports internal/config:\n%s", pkg, out)
		}
	}
}

func TestSettingsHasNoConfigLoaderDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/gazon1/quota-router/internal/settings").CombinedOutput()
	if err != nil {
		t.Fatalf("go list deps: %v\n%s", err, out)
	}
	lower := strings.ToLower(string(out))
	for _, bad := range []string{"koanf", "mapstructure", "viper"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("settings depends on %s:\n%s", bad, out)
		}
	}
	// Architectural exception: runtime settings currently carry koanf/validate
	// struct tags for config decoding/validation. Tags do not create runtime
	// dependencies, but they are format metadata; moving them to config-owned
	// DTO/schema types is a separate refactor.
}
