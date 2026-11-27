package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := Default()
	if cfg.Server != want.Server {
		t.Fatalf("Server = %+v, want %+v", cfg.Server, want.Server)
	}
	if cfg.Polling != want.Polling {
		t.Fatalf("Polling = %+v, want %+v", cfg.Polling, want.Polling)
	}
	if cfg.Routing.LowQuotaThreshold != want.Routing.LowQuotaThreshold || cfg.Routing.ReserveFraction != want.Routing.ReserveFraction || !reflect.DeepEqual(cfg.Routing.Providers, want.Routing.Providers) {
		t.Fatalf("Routing = %+v, want %+v", cfg.Routing, want.Routing)
	}
	if cfg.SQLite != want.SQLite {
		t.Fatalf("SQLite = %+v, want %+v", cfg.SQLite, want.SQLite)
	}
	if cfg.LiteLLM.BaseURL != want.LiteLLM.BaseURL {
		t.Fatalf("LiteLLM.BaseURL = %q, want %q", cfg.LiteLLM.BaseURL, want.LiteLLM.BaseURL)
	}
	if cfg.Observability.Level != "info" {
		t.Fatalf("Observability.Level = %q, want info", cfg.Observability.Level)
	}
}

func TestLoadWorksWithoutConfigFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "127.0.0.1:9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:9090" {
		t.Fatalf("Server.Addr = %q, want 127.0.0.1:9090", cfg.Server.Addr)
	}
}

func TestLoadPrecedenceEnvOverFileOverDefaults(t *testing.T) {
	cleanEnv(t)
	path := writeConfig(t, `
server:
  addr: "127.0.0.1:7000"
  read_timeout: 3s
polling:
  interval: 45s
`)
	t.Setenv(ConfigFileEnv, path)
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "127.0.0.1:9000") // environment wins

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:9000" {
		t.Fatalf("Server.Addr = %q, want the environment value 127.0.0.1:9000", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 3*time.Second {
		t.Fatalf("Server.ReadTimeout = %s, want the file value 3s", cfg.Server.ReadTimeout)
	}
	if cfg.Polling.Interval != 45*time.Second {
		t.Fatalf("Polling.Interval = %s, want the file value 45s", cfg.Polling.Interval)
	}
	if cfg.Server.WriteTimeout != Default().Server.WriteTimeout {
		t.Fatalf("Server.WriteTimeout = %s, want the default", cfg.Server.WriteTimeout)
	}
}

func TestLoadFileSetsNestedAndMapValues(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  base_url: https://litellm.example.com
  model_aliases:
    fast: minimax/MiniMax-M2.7
providers:
  minimax:
    base_url: https://api.minimax.io/v1
`))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.LiteLLM.ModelAliases["fast"]; got.Model != "minimax/MiniMax-M2.7" {
		t.Fatalf("ModelAliases = %v, want the fast alias", cfg.LiteLLM.ModelAliases)
	}
	if got := cfg.Providers["minimax"]; got.BaseURL != "https://api.minimax.io/v1" {
		t.Fatalf("Providers = %v, want minimax from the file", cfg.Providers)
	}
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
			if err == nil {
				t.Fatal("expected a rejection for a credential in the config file")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q must name the setting and the file", err)
			}
			if strings.Contains(err.Error(), "file-secret") {
				t.Fatalf("error %q leaked the credential", err)
			}
		})
	}
}

func TestSecretFileBeatsConfigFile(t *testing.T) {
	cleanEnv(t)
	secretFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(secretFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigFileEnv, writeConfig(t, "providers:\n  minimax:\n    base_url: https://api.minimax.io/v1"))
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE", secretFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Providers["minimax"].APIKey != "from-file" {
		t.Fatalf("api_key = %q, want the mounted secret", cfg.Providers["minimax"].APIKey)
	}
}

func TestLoadEnvSetsProvider(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "https://api.minimax.io/v1")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY", "env-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Providers["minimax"]; got.BaseURL != "https://api.minimax.io/v1" || got.APIKey != "env-secret" {
		t.Fatalf("Providers = %v, want minimax from the environment", cfg.Providers)
	}
}

func TestLoadEnvSetsModelAlias(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST", "minimax/MiniMax-M2.7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.LiteLLM.ModelAliases["fast"].Model; got != "minimax/MiniMax-M2.7" {
		t.Fatalf("ModelAliases = %v, want the fast alias from the environment", cfg.LiteLLM.ModelAliases)
	}
}

func TestLoadMissingConfigFileFails(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "absent.yaml")
	t.Setenv(ConfigFileEnv, path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for an unreadable config file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the config file %q", err, path)
	}
}

func TestLoadInvalidYAMLFails(t *testing.T) {
	cleanEnv(t)
	path := writeConfig(t, "server: [this is not a mapping")
	t.Setenv(ConfigFileEnv, path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for unparsable YAML")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the config file %q", err, path)
	}
}

func TestLoadUnknownSettingFails(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, "server:\n  adr: \"127.0.0.1:7000\"")) // typo

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for an unknown setting")
	}
	if !strings.Contains(err.Error(), "adr") {
		t.Fatalf("error %q does not name the unknown setting", err)
	}
}

func TestLoadInvalidDurationFails(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_SERVER__READ_TIMEOUT", "nope")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "read_timeout") {
		t.Fatalf("error %q does not name the setting", err)
	}
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
			if err == nil {
				t.Fatalf("expected a rejection naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q missing %q", err, tt.want)
			}
		})
	}
}

func TestLoadReportsMultipleErrors(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_SERVER__READ_TIMEOUT", "bad")
	t.Setenv("QUOTA_ROUTER_ROUTING__LOW_QUOTA_THRESHOLD", "9")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"read_timeout", "low_quota_threshold"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestLoadEmptyValuesUseDefaults(t *testing.T) {
	cleanEnv(t)
	for _, key := range configEnvKeys {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want defaults for empty variables", err)
	}
	if cfg.Server != Default().Server {
		t.Fatalf("Server = %+v, want the defaults", cfg.Server)
	}
}

func TestSecretRedaction(t *testing.T) {
	s := Secret("sk-live-do-not-print")
	for name, got := range map[string]string{
		"sprintf %s":    fmt.Sprintf("%s", s),
		"sprintf %q":    fmt.Sprintf("%q", s),
		"sprintf %v":    fmt.Sprintf("%v", s),
		"sprintf %#v":   fmt.Sprintf("%#v", s),
		"String()":      s.String(),
		"GoString()":    s.GoString(),
		"formatted cfg": fmt.Sprintf("%+v", Config{Auth: Auth{Token: s}}),
	} {
		if strings.Contains(got, "sk-live") {
			t.Fatalf("%s = %q, leaked the secret", name, got)
		}
	}

	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "token", s, "cfg", Config{Auth: Auth{Token: s}})
	if strings.Contains(buf.String(), "sk-live") {
		t.Fatalf("slog output leaked the secret: %s", buf.String())
	}

	// main.go logs through JSONHandler, which marshals a Secret as JSON text.
	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "token", s, "cfg", Config{Auth: Auth{Token: s}})
	if strings.Contains(buf.String(), "sk-live") {
		t.Fatalf("JSON log output leaked the secret: %s", buf.String())
	}
	encoded, err := json.Marshal(Config{Auth: Auth{Token: s}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sk-live") {
		t.Fatalf("json.Marshal leaked the secret: %s", encoded)
	}
}

func TestLoadSecretsFromEnvironmentAndFiles(t *testing.T) {
	cleanEnv(t)
	secretFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(secretFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "env-token")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__BASE_URL", "https://api.minimax.io/v1")
	t.Setenv("QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE", secretFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Token != "env-token" {
		t.Fatal("Auth.Token was not read from the environment")
	}
	if cfg.Providers["minimax"].APIKey != "file-token" {
		t.Fatalf("provider api_key = %q, want the trimmed file contents", cfg.Providers["minimax"].APIKey)
	}
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
			if err != nil {
				t.Fatalf("Load() error = %v, want the deprecated flat name to keep working", err)
			}
			if got := tt.got(cfg); got != tt.want {
				t.Fatalf("%s = %v, want %v from the deprecated alias", tt.legacy, got, tt.want)
			}
		})
	}
}

func TestNestedNameBeatsLegacyFlatName(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_ADDR", "127.0.0.1:7001")
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "127.0.0.1:7002")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:7002" {
		t.Fatalf("Server.Addr = %q, want the current name to win over the alias", cfg.Server.Addr)
	}
}

func TestEmptyEnvFallsThroughToFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, "server:\n  addr: \"127.0.0.1:7000\""))
	t.Setenv("QUOTA_ROUTER_SERVER__ADDR", "") // empty is unset, not an override

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:7000" {
		t.Fatalf("Server.Addr = %q, want the file value", cfg.Server.Addr)
	}
}

func TestEnvSecretMakesUnreadableSecretFileIrrelevant(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "env-token")
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))

	if _, err := Load(); err != nil {
		t.Fatalf("Load() error = %v, want the configured value to win over the missing file", err)
	}
}

func TestEnvSecretBeatsSecretFile(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN", "env-token")
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Token != "env-token" {
		t.Fatal("the explicit environment value must win over its _FILE counterpart")
	}
}

func TestLoadMissingSecretFileFails(t *testing.T) {
	cleanEnv(t)
	path := filepath.Join(t.TempDir(), "absent")
	t.Setenv("QUOTA_ROUTER_AUTH__TOKEN_FILE", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for an unreadable secret file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the secret file", err)
	}
}

func TestSecretFileRejectsNonSecretSetting(t *testing.T) {
	cleanEnv(t)
	// A path-shaped value for a non-secret setting must not be read as a file.
	t.Setenv("QUOTA_ROUTER_SQLITE__PATH_FILE", "/etc/passwd")

	_, err := Load()
	if err == nil {
		t.Fatal("expected a rejection for a _FILE variable naming a non-secret setting")
	}
	if !strings.Contains(err.Error(), "not a secret setting") {
		t.Fatalf("error %q does not explain the rejection", err)
	}
}

func TestValidationURLErrorHidesCredentials(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", "http://user:hunter2@example.com")

	_, err := Load()
	if err == nil {
		t.Fatal("a URL with userinfo must be rejected, not accepted and later logged")
	}
	if !strings.Contains(err.Error(), "must not embed credentials") {
		t.Fatalf("error %q does not explain the URL rule", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %q leaked a URL credential", err)
	}

	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__BASE_URL", "user:hunter2@example.com/relative")
	_, err = Load()
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %q leaked a URL credential", err)
	}
}

// setEnv sets one variable and returns a mutator for the table test above.
func setEnv(key, value string) func(*testing.T) {
	return func(t *testing.T) { t.Setenv(key, value) }
}

// writeConfig writes a YAML file into the test's temporary directory.
func TestExampleConfigLoads(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, "../../config.example.yaml")

	if _, err := Load(); err != nil {
		t.Fatalf("the shipped config.example.yaml must load: %v", err)
	}
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
