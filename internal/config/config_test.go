package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configEnvKeys is every variable the loader reads, for the empty-value test.
var configEnvKeys = []string{
	ConfigFileEnv,
	"QUOTA_ROUTER_SERVER__ADDR",
	"QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT",
	"QUOTA_ROUTER_SERVER__READ_HEADER_TIMEOUT",
	"QUOTA_ROUTER_SERVER__READ_TIMEOUT",
	"QUOTA_ROUTER_SERVER__WRITE_TIMEOUT",
	"QUOTA_ROUTER_SERVER__IDLE_TIMEOUT",
	"QUOTA_ROUTER_LITELLM__BASE_URL",
	"QUOTA_ROUTER_POLLING__INTERVAL",
	"QUOTA_ROUTER_POLLING__REQUEST_TIMEOUT",
	"QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD",
	"QUOTA_ROUTER_SQLITE__PATH",
	"QUOTA_ROUTER_SQLITE__BUSY_TIMEOUT",
	"QUOTA_ROUTER_AUTH__TOKEN",
	"QUOTA_ROUTER_OBSERVABILITY__LEVEL",
}

func TestLoadDefaults(t *testing.T) {
	cleanEnv(t)

	cfg, err := Load()
	require.NoError(t, err)
	want := Default()
	assert.Equal(t, want.Server, cfg.Server)
	assert.Equal(t, want.Polling, cfg.Polling)
	assert.Equal(t, want.Routing.LowQuotaThreshold, cfg.Routing.LowQuotaThreshold)
	assert.Equal(t, want.Routing.ReserveFraction, cfg.Routing.ReserveFraction)
	assert.Equal(t, want.Routing.Providers, cfg.Routing.Providers)
	assert.Equal(t, want.SQLite, cfg.SQLite)
	assert.Equal(t, want.LiteLLM.BaseURL, cfg.LiteLLM.BaseURL)
	assert.Equal(t, "info", cfg.Observability.Level)
}

func TestLoadWorksWithoutConfigFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "127.0.0.1:9090")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9090", cfg.Server.Addr)
}

func TestLoadPrecedenceEnvOverFileOverDefaults(t *testing.T) {
	cleanEnv(t)
	path := writeConfig(t, fixture(t, "loading/precedence-env-over-file.yaml"))
	t.Setenv(ConfigFileEnv, path)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "127.0.0.1:9000") // environment wins

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9000", cfg.Server.Addr, "want the environment value")
	assert.Equal(t, 3*time.Second, cfg.Server.ReadTimeout, "want the file value 3s")
	assert.Equal(t, 45*time.Second, cfg.Polling.Interval, "want the file value 45s")
	assert.Equal(t, Default().Server.WriteTimeout, cfg.Server.WriteTimeout, "want the default")
}

func TestLoadFileSetsNestedAndMapValues(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, fixture(t, "loading/nested-and-map-values.yaml")))

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "minimax/MiniMax-M2.7", cfg.LiteLLM.ModelAliases["fast"].Model, "want the fast alias")
	assert.Equal(t, "https://api.minimax.io/v1", cfg.Providers["minimax"].BaseURL, "want minimax from the file")
}

func TestLoadRejectsCredentialsInConfigFile(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"provider key", "providers:\n  minimax:\n    api_key: file-secret", "providers.minimax.api_key"},
		{"controller token", "auth:\n  token: file-secret", "auth.token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			path := writeConfig(t, tt.body)
			t.Setenv(ConfigFileEnv, path)

			_, err := Load()
			require.Error(t, err, "expected a rejection for a credential in the config file")
			assert.ErrorContains(t, err, tt.want, "must name the setting")
			assert.ErrorContains(t, err, path, "must name the file")
			assert.NotContains(t, err.Error(), "file-secret", "leaked the credential")
		})
	}
}

func TestSecretFileBeatsConfigFile(t *testing.T) {
	cleanEnv(t)
	secretFile := filepath.Join(t.TempDir(), "api-key")
	require.NoError(t, os.WriteFile(secretFile, []byte("from-file\n"), 0o600))
	t.Setenv(ConfigFileEnv, writeConfig(t, "providers:\n  minimax:\n    base_url: https://api.minimax.io/v1"))
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE", secretFile)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "from-file", string(cfg.Providers["minimax"].APIKey), "want the mounted secret")
}

func TestLoadEnvSetsProvider(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "https://api.minimax.io/v1")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY", "env-secret")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "https://api.minimax.io/v1", cfg.Providers["minimax"].BaseURL, "want minimax from the environment")
	assert.Equal(t, "env-secret", string(cfg.Providers["minimax"].APIKey), "want minimax from the environment")
}

func TestLoadEnvSetsModelAlias(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST", "minimax/MiniMax-M2.7")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "minimax/MiniMax-M2.7", cfg.LiteLLM.ModelAliases["fast"].Model, "want the fast alias from the environment")
}

func TestLoadMissingConfigFileFails(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "absent.yaml")
	t.Setenv(ConfigFileEnv, path)

	_, err := Load()
	require.Error(t, err, "expected an error for an unreadable config file")
	assert.ErrorContains(t, err, path, "does not name the config file")
}

func TestLoadInvalidYAMLFails(t *testing.T) {
	cleanEnv(t)
	path := writeConfig(t, "server: [this is not a mapping")
	t.Setenv(ConfigFileEnv, path)

	_, err := Load()
	require.Error(t, err, "expected an error for unparsable YAML")
	assert.ErrorContains(t, err, path, "does not name the config file")
}

func TestLoadUnknownSettingFails(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, "server:\n  adr: \"127.0.0.1:7000\"")) // typo

	_, err := Load()
	require.Error(t, err, "expected an error for an unknown setting")
	assert.ErrorContains(t, err, "adr", "does not name the unknown setting")
}

func TestLoadInvalidDurationFails(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_SERVER__READ_TIMEOUT", "nope")

	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "read_timeout", "does not name the setting")
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{"empty addr", func(t *testing.T) {
			t.Setenv(ConfigFileEnv, writeConfig(t, "server:\n  addr: \"\""))
		}, "server.addr must not be empty"},
		{"zero idle timeout", setEnv("QUOTA_ROUTER_SERVER__IDLE_TIMEOUT", "0s"), "idle_timeout must be > 0"},
		{"negative shutdown timeout", setEnv("QUOTA_ROUTER_SERVER__SHUTDOWN_TIMEOUT", "-1s"), "shutdown_timeout must be > 0"},
		{"relative litellm url", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "litellm:4000"), "litellm.base_url"},
		{"non-http litellm url", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "ftp://litellm"), "litellm.base_url"},
		{"bad model alias", setEnv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST", "no-slash"), "litellm.model_aliases"},
		{"relative provider url", setEnv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "api.minimax.io/v1"), "providers.minimax.base_url"},
		{"litellm query string", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "https://litellm.example.com?token=abc"), "litellm.base_url: must not contain a query string"},
		{"litellm bare query", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "https://litellm.example.com?"), "litellm.base_url: must not contain a query string"},
		{"litellm fragment", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "https://litellm.example.com#section"), "litellm.base_url: must not contain a fragment"},
		{"litellm bare fragment", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "https://litellm.example.com/x#"), "litellm.base_url: must not contain a fragment"},
		{"litellm credentials", setEnv("QUOTA_ROUTER_LITELLM__BASE_URL", "https://user:pass@litellm.example.com"), "litellm.base_url: must not embed credentials"},
		{"provider query string", setEnv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "https://api.minimax.io/v1?token=abc"), "providers.minimax.base_url: must not contain a query string"},
		{"provider fragment", setEnv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "https://api.minimax.io/v1#frag"), "providers.minimax.base_url: must not contain a fragment"},
		{"zero poll interval", setEnv("QUOTA_ROUTER_POLLING__INTERVAL", "0s"), "polling.interval must be > 0"},
		{"timeout exceeds interval", setEnv("QUOTA_ROUTER_POLLING__REQUEST_TIMEOUT", "60s"), "must not exceed polling.interval"},
		{"threshold above one", setEnv("QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD", "1.5"), "low_quota_threshold"},
		{"threshold zero", setEnv("QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD", "0"), "low_quota_threshold"},
		{"non-numeric threshold", setEnv("QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD", "lots"), "low_quota_threshold"},
		{"empty sqlite path", func(t *testing.T) {
			t.Setenv(ConfigFileEnv, writeConfig(t, "sqlite:\n  path: \"\""))
		}, "sqlite.path must not be empty"},
		{"negative usage retention", setEnv("QUOTA_ROUTER_SQLITE__RETENTION__USAGE_RECORDS", "-1h"), "sqlite.retention.usage_records must be >= 0"},
		{"sqlite memory path", setEnv("QUOTA_ROUTER_SQLITE__PATH", ":memory:"), "sqlite.path must be a file path"},
		{"sqlite uri path", setEnv("QUOTA_ROUTER_SQLITE__PATH", "file:/var/lib/qr.db"), "sqlite.path must be a file path, not a SQLite URI"},
		{"sqlite uri path uppercase", setEnv("QUOTA_ROUTER_SQLITE__PATH", "FILE:/var/lib/qr.db"), "sqlite.path must be a file path, not a SQLite URI"},
		{"unknown log level", setEnv("QUOTA_ROUTER_OBSERVABILITY__LEVEL", "verbose"), "observability.level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			tt.mutate(t)

			_, err := Load()
			require.Error(t, err, "expected a rejection naming %q", tt.want)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadReportsMultipleErrors(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_SERVER__READ_TIMEOUT", "bad")
	t.Setenv("QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD", "9")

	_, err := Load()
	require.Error(t, err)
	for _, want := range []string{"read_timeout", "low_quota_threshold"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestLoadEmptyValuesUseDefaults(t *testing.T) {
	cleanEnv(t)
	for _, key := range configEnvKeys {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	require.NoError(t, err, "want defaults for empty variables")
	assert.Equal(t, Default().Server, cfg.Server)
}

func TestSecretRedaction(t *testing.T) {
	s := Secret("sk-live-do-not-print")
	for name, got := range map[string]string{
		"sprintf %s":    s.String(),
		"sprintf %q":    fmt.Sprintf("%q", s),
		"sprintf %v":    fmt.Sprintf("%v", s),
		"sprintf %#v":   fmt.Sprintf("%#v", s),
		"String()":      s.String(),
		"GoString()":    s.GoString(),
		"formatted cfg": fmt.Sprintf("%+v", Config{Auth: Auth{Token: s}}),
	} {
		assert.NotContains(t, got, "sk-live", "%s leaked the secret", name)
	}

	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "token", s, "cfg", Config{Auth: Auth{Token: s}})
	assert.NotContains(t, buf.String(), "sk-live", "slog output leaked the secret")

	// main.go logs through JSONHandler, which marshals a Secret as JSON text.
	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "token", s, "cfg", Config{Auth: Auth{Token: s}})
	assert.NotContains(t, buf.String(), "sk-live", "JSON log output leaked the secret")
	encoded, err := json.Marshal(Config{Auth: Auth{Token: s}})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "sk-live", "json.Marshal leaked the secret")
}

func TestLoadSecretsFromEnvironmentAndFiles(t *testing.T) {
	cleanEnv(t)
	secretFile := filepath.Join(t.TempDir(), "api-key")
	require.NoError(t, os.WriteFile(secretFile, []byte("file-token\n"), 0o600))
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "env-token")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "https://api.minimax.io/v1")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE", secretFile)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "env-token", string(cfg.Auth.Token), "Auth.Token was not read from the environment")
	assert.Equal(t, "file-token", string(cfg.Providers["minimax"].APIKey), "want the trimmed file contents")
}

func TestLegacyFlatEnvNamesStillWork(t *testing.T) {
	// Every alias in legacyEnvKeys, so a removed or renamed mapping fails here.
	tests := []struct {
		legacy, value string
		got           func(Config) any
		want          any
	}{
		{"QUOTA_ROUTER_ADDR", "127.0.0.1:7001", func(c Config) any { return c.Server.Addr }, "127.0.0.1:7001"},
		{"QUOTA_ROUTER_SHUTDOWN_TIMEOUT", "11s", func(c Config) any { return c.Server.ShutdownTimeout }, 11 * time.Second},
		{"QUOTA_ROUTER_READ_HEADER_TIMEOUT", "6s", func(c Config) any { return c.Server.ReadHeaderTimeout }, 6 * time.Second},
		{"QUOTA_ROUTER_READ_TIMEOUT", "7s", func(c Config) any { return c.Server.ReadTimeout }, 7 * time.Second},
		{"QUOTA_ROUTER_WRITE_TIMEOUT", "12s", func(c Config) any { return c.Server.WriteTimeout }, 12 * time.Second},
		{"QUOTA_ROUTER_IDLE_TIMEOUT", "61s", func(c Config) any { return c.Server.IdleTimeout }, 61 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.legacy, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv(tt.legacy, tt.value)

			cfg, err := Load()
			require.NoError(t, err, "want the deprecated flat name to keep working")
			assert.Equal(t, tt.want, tt.got(cfg), "%s from the deprecated alias", tt.legacy)
		})
	}
}

func TestNestedNameBeatsLegacyFlatName(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_ADDR", "127.0.0.1:7001")
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "127.0.0.1:7002")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7002", cfg.Server.Addr, "want the current name to win over the alias")
}

func TestEmptyEnvFallsThroughToFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, "server:\n  addr: \"127.0.0.1:7000\""))
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "") // empty is unset, not an override

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7000", cfg.Server.Addr, "want the file value")
}

func TestEnvSecretMakesUnreadableSecretFileIrrelevant(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "env-token")
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))

	_, err := Load()
	require.NoError(t, err, "want the configured value to win over the missing file")
}

func TestEnvSecretBeatsSecretFile(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("file-token\n"), 0o600))
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "env-token")
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN_FILE", path)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "env-token", string(cfg.Auth.Token), "the explicit environment value must win over its _FILE counterpart")
}

func TestLoadMissingSecretFileFails(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "absent")
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN_FILE", path)

	_, err := Load()
	require.Error(t, err, "expected an error for an unreadable secret file")
	assert.ErrorContains(t, err, path, "does not name the secret file")
}

func TestSecretFileRejectsNonSecretSetting(t *testing.T) {
	cleanEnv(t)
	// A path-shaped value for a non-secret setting must not be read as a file.
	t.Setenv("QUOTA_ROUTER_SQLITE__PATH_FILE", "/etc/passwd")

	_, err := Load()
	require.Error(t, err, "expected a rejection for a _FILE variable naming a non-secret setting")
	assert.ErrorContains(t, err, "not a secret setting", "does not explain the rejection")
}

func TestValidationURLErrorHidesCredentials(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", "http://user:hunter2@example.com")

	_, err := Load()
	require.Error(t, err, "a URL with userinfo must be rejected, not accepted and later logged")
	assert.ErrorContains(t, err, "must not embed credentials", "does not explain the URL rule")
	assert.NotContains(t, err.Error(), "hunter2", "leaked a URL credential")

	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", "user:hunter2@example.com/relative")
	_, err = Load()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2", "leaked a URL credential")
}

// setEnv sets one variable and returns a mutator for the table test above.
func setEnv(key, value string) func(*testing.T) {
	return func(t *testing.T) { t.Setenv(key, value) }
}

// writeConfig writes a YAML file into the test's temporary directory.
func TestExampleConfigLoads(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, "../../config.example.yaml")

	_, err := Load()
	require.NoError(t, err, "the shipped config.example.yaml must load")
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cleanEnv unsets every variable the loader reads for the duration of the test.
// t.Setenv("") unsets the variable and restores it afterwards.
//
// HOME is redirected too: with QUOTA_ROUTER_CONFIG_FILE unset, Load reads the
// user-level file, so a test must not pick up the developer's real
// ~/.config/quota-router/config.yaml — it would change what every default test
// asserts, and pass or fail depending on the machine.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, EnvPrefix) {
			t.Setenv(name, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
}
