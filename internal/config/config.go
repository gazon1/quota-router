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
	BaseURL string `koanf:"base_url"`
	// ModelAliases maps a routing alias to the LiteLLM "<provider>/<model>" id.
	ModelAliases map[string]string `koanf:"model_aliases"`
}

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

// Routing configures the routing policy thresholds.
type Routing struct {
	// LowQuotaThreshold is the remaining-quota fraction at or below which a
	// provider counts as scarce and loses to a healthier candidate.
	LowQuotaThreshold float64 `koanf:"low_quota_threshold"`
}

// SQLite configures quota snapshot persistence.
type SQLite struct {
	Path        string        `koanf:"path"`
	BusyTimeout time.Duration `koanf:"busy_timeout"`
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
		LiteLLM:       LiteLLM{BaseURL: "http://127.0.0.1:4000", ModelAliases: map[string]string{}},
		Providers:     map[string]Provider{},
		Polling:       Polling{Interval: 30 * time.Second, RequestTimeout: 10 * time.Second},
		Routing:       Routing{LowQuotaThreshold: 0.1},
		SQLite:        SQLite{Path: "quota-router.db", BusyTimeout: 5 * time.Second},
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
// underscores free for field names such as base_url.
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
