package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gazon1/quota-router/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogueBoundary(t *testing.T) {
	cleanEnv(t)
	cfg, err := Load()
	require.NoError(t, err)
	cat := cfg.Catalogue()
	assert.Equal(t, cfg.Routing.LowQuotaThreshold, cat.Routing.LowQuotaThreshold)
	assert.Len(t, cat.ModelAliases, len(cfg.LiteLLM.ModelAliases))
	// The runtime catalogue is the tag-free shape routing reads, so it is not
	// assignable from the configuration DTOs.
	var _ settings.Secret = cfg.Auth.Token
}

func TestRoutingAndAgentDoNotImportConfig(t *testing.T) {
	for _, pkg := range []string{"github.com/gazon1/quota-router/internal/routing/...", "github.com/gazon1/quota-router/internal/agent"} {
		out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{.Imports}}", pkg).CombinedOutput()
		require.NoError(t, err, "go list %s: %v\n%s", pkg, err, out)
		assert.NotContains(t, string(out), "\"github.com/gazon1/quota-router/internal/config\"", "%s imports internal/config:\n%s", pkg, out)
	}
}

func TestSettingsHasNoConfigLoaderDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/gazon1/quota-router/internal/settings").CombinedOutput()
	require.NoError(t, err, "go list deps: %v\n%s", err, out)
	lower := strings.ToLower(string(out))
	for _, bad := range []string{"koanf", "mapstructure", "viper"} {
		assert.NotContains(t, lower, bad, "settings depends on %s:\n%s", bad, out)
	}
}

// The runtime types carry no configuration-format metadata: koanf names and
// validate rules live on the config DTOs, which Catalogue converts.
func TestSettingsCarriesNoConfigFormatTags(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "settings", "*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no settings sources found")
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, tag := range []string{`koanf:"`, `validate:"`} {
			assert.NotContains(t, string(src), tag, "%s carries %s; that is config-format metadata", path, tag)
		}
	}
}
