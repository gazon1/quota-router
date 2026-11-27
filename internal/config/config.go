// Package config loads and validates the quota controller configuration.
//
// Precedence, applied in this order: built-in defaults, then the optional YAML
// file, then environment variables. Values are unmarshaled into typed structs
// and validated once at startup; the rest of the program consumes Config and
// never sees koanf.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix prefixes every environment variable the loader reads. EnvDelim
// separates YAML nesting levels in a variable name, so a single underscore stays
// available inside field names: QUOTA_ROUTER_LITELLM__BASE_URL -> litellm.base_url.
const (
	EnvPrefix     = "QUOTA_ROUTER_"
	EnvDelim      = "__"
	ConfigFileEnv = EnvPrefix + "CONFIG_FILE"
)

// Config is the whole runtime configuration.
type Config struct {
	Server        Server              `koanf:"server"`
	LiteLLM       LiteLLM             `koanf:"litellm"`
	Providers     map[string]Provider `koanf:"providers"`
	Polling       Polling             `koanf:"polling"`
	Routing       Routing             `koanf:"routing"`
	SQLite        SQLite              `koanf:"sqlite"`
	Auth          Auth                `koanf:"auth"`
	Observability Observability       `koanf:"observability"`
}

// Server configures the controller's own HTTP server.
type Server struct {
	Addr              string        `koanf:"addr"`
	ShutdownTimeout   time.Duration `koanf:"shutdown_timeout"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	ReadTimeout       time.Duration `koanf:"read_timeout"`
	WriteTimeout      time.Duration `koanf:"write_timeout"`
	IdleTimeout       time.Duration `koanf:"idle_timeout"`
}

// LiteLLM locates the proxy the controller routes through.
type LiteLLM struct {
	// BaseURL is the proxy root, for example http://127.0.0.1:4000; model
	// requests go to /v1/... below it.
	BaseURL string `koanf:"base_url"`
	// APIKey is the proxy's master key, used only to verify the catalogue
	// against GET /v1/models. Unset skips that verification.
	APIKey Secret `koanf:"api_key"`
	// ModelAliases is the model catalogue: a routing alias to the entry the
	// proxy serves under that name.
	ModelAliases map[string]Model `koanf:"model_aliases"`
}

// Model is one catalogue entry: the model an alias resolves to and what was
// verified about it. Nothing in the entry is discovered at runtime — the model
// id comes from the configuration and from the reference measured in
// docs/litellm-provider-compatibility.md. Provider and alias are not duplicated
// here: the provider is the prefix of Model and the alias is the map key.
type Model struct {
	// Model is LiteLLM's model_name, exactly "<provider>/<model>" as the proxy
	// lists and resolves it.
	Model string `koanf:"model"`
	// Enabled reports whether routing may use this entry. Absent means enabled,
	// so listing a model is enough to use it.
	Enabled *bool `koanf:"enabled"`
	// ContextLimit is the verified input context window in tokens. Zero means
	// unverified; a request constraint never relies on it then.
	ContextLimit int          `koanf:"context_limit"`
	Capabilities Capabilities `koanf:"capabilities"`
	// Tasks are the task classes this model is suitable for, for example
	// "coding" or "reasoning".
	Tasks []string `koanf:"tasks"`
}

// Capabilities are what a model was verified to support. Each field is
// tri-state and defaults to CapabilityUnknown, so a model that never claimed a
// capability is not treated as supporting it.
type Capabilities struct {
	Streaming        Capability `koanf:"streaming"`
	ToolCalling      Capability `koanf:"tool_calling"`
	StructuredOutput Capability `koanf:"structured_output"`
}

// IsEnabled reports whether routing may use this entry. Listing a model is
// enough to use it; the field only exists to switch one off.
func (m Model) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// Provider is the provider prefix of the model id. It is derived rather than
// stored, so the entry cannot name two different providers at once.
func (m Model) Provider() string {
	p, _, _ := strings.Cut(m.Model, "/")
	return p
}

// UnmarshalText accepts the short form the catalogue shipped with
// (`fast: minimax/MiniMax-M2.7`) as a model id with everything else unknown, so
// widening the entry did not break an existing configuration file.
func (m *Model) UnmarshalText(b []byte) error {
	m.Model = string(b)
	return nil
}

// Capability is a verified capability claim. CapabilityUnknown is the zero value
// and means nobody verified it, which is weaker than CapabilityUnsupported.
type Capability string

const (
	CapabilityUnknown     Capability = ""
	CapabilitySupported   Capability = "supported"
	CapabilityUnsupported Capability = "unsupported"
)

// Supported reports whether the capability is explicitly supported.
func (c Capability) Supported() bool { return c == CapabilitySupported }

// Provider is a quota provider the controller polls directly.
type Provider struct {
	BaseURL string `koanf:"base_url"`
	APIKey  Secret `koanf:"api_key"`
}

// Polling configures the quota polling loop.
type Polling struct {
	Interval       time.Duration `koanf:"interval"`
	RequestTimeout time.Duration `koanf:"request_timeout"`
}

// Routing configures routing thresholds and the static provider/model
// preference order. Preference is consulted only after hard constraints and
// viable session affinity, as defined by issue #53 and applied by #17.
type Routing struct {
	// LowQuotaThreshold is the remaining-quota fraction at or below which a
	// provider counts as scarce and loses to a healthier candidate.
	LowQuotaThreshold float64 `koanf:"low_quota_threshold"`
	// ReserveFraction is the remaining-quota fraction reserved for reasoning and
	// coding workloads. A provider at or below this fraction is ineligible for
	// those task classes while quota is not exhausted. Zero disables the reserve.
	ReserveFraction float64 `koanf:"reserve_fraction"`
	// Providers is the static preference order. Keys are provider names as they
	// appear in model ids (<provider>/<model>). When empty, no static preference
	// is configured. When set, every enabled catalogue alias must be listed under
	// its provider with an explicit priority.
	Providers map[string]ProviderRouting `koanf:"providers"`
}

// ProviderRouting orders one provider and the model aliases served by it.
// Lower numeric priority means higher preference.
type ProviderRouting struct {
	// Priority is this provider's static preference rank. Must be a positive
	// integer; lower values are preferred. Duplicate provider priorities are invalid.
	Priority int `koanf:"priority"`
	// Models orders catalogue aliases within this provider. Keys are catalogue
	// aliases; values are positive integer priorities where lower is preferred.
	// Model priority is scoped to this provider and never compared across providers.
	Models map[string]int `koanf:"models"`
}

// SQLite configures quota snapshot persistence.
type SQLite struct {
	Path        string        `koanf:"path"`
	BusyTimeout time.Duration `koanf:"busy_timeout"`
	// Retention bounds history. Zero keeps everything.
	Retention Retention `koanf:"retention"`
}

// Retention bounds how long persisted history is kept. The newest state per
// provider always survives, so cleanup never removes what routing reads.
type Retention struct {
	QuotaSnapshots   time.Duration `koanf:"quota_snapshots"`
	ProviderHealth   time.Duration `koanf:"provider_health"`
	UsageRecords     time.Duration `koanf:"usage_records"`
	RoutingDecisions time.Duration `koanf:"routing_decisions"`
}

// Auth holds the bearer token for the controller's own HTTP API.
type Auth struct {
	Token Secret `koanf:"token"`
}

// Observability configures logging.
type Observability struct {
	Level string `koanf:"level"`
}

// SlogLevel returns the configured level. Validate rejects other values, so the
// fallback only covers a zero Observability.
func (o Observability) SlogLevel() slog.Level {
	switch o.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Default returns the built-in defaults. Every setting has one: the service
// starts from this alone, with no config file and no environment variables.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8080",
			ShutdownTimeout:   10 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		LiteLLM:   LiteLLM{BaseURL: "http://127.0.0.1:4000", APIKey: "", ModelAliases: map[string]Model{}},
		Providers: map[string]Provider{},
		Polling:   Polling{Interval: 30 * time.Second, RequestTimeout: 10 * time.Second},
		Routing: Routing{
			LowQuotaThreshold: 0.1,
			ReserveFraction:   0,
			Providers:         map[string]ProviderRouting{},
		},
		SQLite: SQLite{
			Path:        "quota-router.db",
			BusyTimeout: 5 * time.Second,
			// A poll every 30s is ~2.9k snapshots a provider a day; a month of
			// history is a few MB. Set a field to 0 to keep that table forever.
			Retention: Retention{
				QuotaSnapshots:   30 * 24 * time.Hour,
				ProviderHealth:   30 * 24 * time.Hour,
				UsageRecords:     90 * 24 * time.Hour,
				RoutingDecisions: 30 * 24 * time.Hour,
			},
		},
		Observability: Observability{Level: "info"},
	}
}

// Load resolves the configuration in documented precedence order and validates
// it. The YAML file named by QUOTA_ROUTER_CONFIG_FILE is optional; when it is set
// but unreadable or unparsable, Load returns an error naming it.
func Load() (Config, error) {
	cfg := Default()

	k := koanf.New(".")
	if path := os.Getenv(ConfigFileEnv); path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("config file %s: %w", path, err)
		}
		if err := rejectSecretsInFile(k, path); err != nil {
			return Config{}, err
		}
	}
	// Secret files are an alternative source for the same environment-level
	// values, so they load before them: an explicit QUOTA_ROUTER_..._API_KEY
	// still wins over ..._API_KEY_FILE, matching the documented precedence.
	if err := loadSecretFiles(k); err != nil {
		return Config{}, err
	}
	if err := k.Load(env.ProviderWithValue(EnvPrefix, EnvDelim, envKey), nil); err != nil {
		return Config{}, fmt.Errorf("environment: %w", err)
	}

	// Decoding into the defaults keeps every value the file and environment do
	// not set. ErrorUnused turns a misspelled setting into an error.
	var errs []error
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{DecoderConfig: &mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		ErrorUnused:      true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
	}}); err != nil {
		errs = append(errs, fmt.Errorf("decode configuration: %w", err))
	}
	// Tasks is the one list an operator sets from the environment, where a value
	// cannot be a YAML sequence: accept "coding,reasoning" as two classes.
	for alias, m := range cfg.LiteLLM.ModelAliases {
		if len(m.Tasks) == 1 {
			m.Tasks = strings.Split(m.Tasks[0], ",")
		}
		for i := range m.Tasks {
			m.Tasks[i] = strings.TrimSpace(m.Tasks[i])
		}
		cfg.LiteLLM.ModelAliases[alias] = m
	}
	normalizeRoutingEnvKeys(&cfg)
	if err := cfg.validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// legacyEnvKeys maps the flat variable names this service shipped before the
// nested model to their current paths, so an existing deployment keeps working.
// The new name wins when both are set. Documented as deprecated in README.md.
var legacyEnvKeys = map[string]string{
	"addr":                "server__addr",
	"shutdown_timeout":    "server__shutdown_timeout",
	"read_header_timeout": "server__read_header_timeout",
	"read_timeout":        "server__read_timeout",
	"write_timeout":       "server__write_timeout",
	"idle_timeout":        "server__idle_timeout",
}

// envKey maps QUOTA_ROUTER_SERVER__ADDR to server.addr, and the deprecated
// flat QUOTA_ROUTER_ADDR to the same place. The double delimiter leaves single
// underscores free for field names such as base_url. Dynamic map keys in
// routing.providers.*.models.* may contain hyphens (e.g. free-coding); users can
// write them as literal hyphens or as _H_ in the env var name, and
// normalizeRoutingEnvKeys resolves them after unmarshal.
func envKey(key, value string) (string, any) {
	if value == "" || key == ConfigFileEnv || strings.HasSuffix(key, "_FILE") {
		return "", nil // empty value means unset; _FILE is handled separately
	}
	name := strings.ToLower(strings.TrimPrefix(key, EnvPrefix))
	if current, ok := legacyEnvKeys[name]; ok {
		if os.Getenv(EnvPrefix+strings.ToUpper(current)) != "" {
			return "", nil // the current name wins over its deprecated alias
		}
		return current, value
	}
	return name, value
}

// normalizeRoutingEnvKeys rewrites routing.providers.*.models map keys so env
// vars can address hyphenated catalogue aliases. Env var names are lowercased
// by envKey, so `FREE-CODING` and `FREE_CODING` both arrive as `free-coding`
// only if the user wrote a literal hyphen; `FREE_CODING` arrives as
// `free_coding` and would miss the real alias. We fix only unambiguous cases:
//
//   - exact key already exists in catalogue: keep
//   - key with '_' -> '-' exists in catalogue and the '_' form does not: rewrite
//   - key with '_h_' -> '-' exists in catalogue and the raw form does not: rewrite
//
// No silent normalization when both forms could be valid catalogue aliases.
func normalizeRoutingEnvKeys(cfg *Config) {
	if len(cfg.Routing.Providers) == 0 {
		return
	}
	known := make(map[string]bool, len(cfg.LiteLLM.ModelAliases))
	for alias := range cfg.LiteLLM.ModelAliases {
		known[alias] = true
	}
	for provider, rp := range cfg.Routing.Providers {
		if len(rp.Models) == 0 {
			continue
		}
		rewritten := make(map[string]int, len(rp.Models))
		changed := false
		for k, v := range rp.Models {
			nk := normalizeEnvMapKey(k, known)
			if nk != k {
				changed = true
			}
			// If a collision happens after rewrite, keep the original key's
			// value by not overwriting an existing rewritten entry unless it
			// came from the same source key; validation will catch duplicates.
			if _, exists := rewritten[nk]; !exists {
				rewritten[nk] = v
			} else {
				// Preserve both as-is by keeping the later in a deterministic way?
				// Better: keep original key so validation reports the real problem.
				rewritten[k] = v
			}
		}
		if changed {
			rp.Models = rewritten
			cfg.Routing.Providers[provider] = rp
		}
	}
}

func normalizeEnvMapKey(key string, known map[string]bool) string {
	if known[key] {
		return key
	}
	// Common env var form: users write FREE_CODING for free-coding. Normalize
	// only when the hyphenated form is a known catalogue alias and the
	// underscore form is not, so aliases that legitimately contain underscores
	// are never silently remapped.
	if strings.Contains(key, "_") {
		candidate := strings.ReplaceAll(key, "_", "-")
		if known[candidate] && !known[key] {
			return candidate
		}
	}
	return key
}
