package config

import (
	"os"
	"os/exec"
	"path/filepath"
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
	// The runtime catalogue is the tag-free shape routing reads, so it is not
	// assignable from the configuration DTOs.
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
}

// The runtime types carry no configuration-format metadata: koanf names and
// validate rules live on the config DTOs, which Catalogue converts.
func TestSettingsCarriesNoConfigFormatTags(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "settings", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no settings sources found")
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range []string{`koanf:"`, `validate:"`} {
			if strings.Contains(string(src), tag) {
				t.Fatalf("%s carries %s; that is config-format metadata", path, tag)
			}
		}
	}
}
