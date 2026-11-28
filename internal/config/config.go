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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	env "github.com/knadh/koanf/providers/env/v2"
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
	// envHyphenEscape encodes a literal hyphen inside one environment variable
	// name segment: FREE_H_CODING addresses the alias free-coding. Only
	// environment names use it; file keys are literal. A segment that is
	// exactly "h" is left alone, so a provider named h keeps its name.
	envHyphenEscape = "_h_"
)

// Config is the whole runtime configuration.
type Config struct {
	Server        Server              `koanf:"server"`
	LiteLLM       LiteLLM             `koanf:"litellm"`
	Providers     map[string]Provider `koanf:"providers" validate:"dive,keys,required,endkeys,required"`
	Polling       Polling             `koanf:"polling"`
	Routing       Routing             `koanf:"routing"`
	SQLite        SQLite              `koanf:"sqlite"`
	Auth          Auth                `koanf:"auth"`
	Observability Observability       `koanf:"observability"`
}

// Server configures the controller's own HTTP server.
type Server struct {
	Addr              string        `koanf:"addr" validate:"required"`
	ShutdownTimeout   time.Duration `koanf:"shutdown_timeout" validate:"gt=0"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout" validate:"gt=0"`
	ReadTimeout       time.Duration `koanf:"read_timeout" validate:"gt=0"`
	WriteTimeout      time.Duration `koanf:"write_timeout" validate:"gt=0"`
	IdleTimeout       time.Duration `koanf:"idle_timeout" validate:"gt=0"`
}

// LiteLLM locates the proxy the controller routes through.
type LiteLLM struct {
	// BaseURL is the proxy root, for example http://127.0.0.1:4000; model
	// requests go to /v1/... below it.
	BaseURL string `koanf:"base_url" validate:"required,http_url"`
	// APIKey is the proxy's master key, used only to verify the catalogue
	// against GET /v1/models. Unset skips that verification.
	APIKey Secret `koanf:"api_key"`
	// ModelAliases is the model catalogue: a routing alias to the entry the
	// proxy serves under that name.
	ModelAliases map[string]Model `koanf:"model_aliases" validate:"dive,keys,alias_name,endkeys,required"`
}

// Provider is one quota provider the controller polls directly.
type Provider struct {
	BaseURL string `koanf:"base_url" validate:"required,http_url"`
	APIKey  Secret `koanf:"api_key"`
}

// Polling configures the quota polling loop.
type Polling struct {
	Interval       time.Duration `koanf:"interval" validate:"gt=0"`
	RequestTimeout time.Duration `koanf:"request_timeout" validate:"gt=0"`
}

// SQLite configures quota snapshot persistence.
type SQLite struct {
	Path        string        `koanf:"path" validate:"required,sqlite_path"`
	BusyTimeout time.Duration `koanf:"busy_timeout" validate:"gt=0"`
	// Retention bounds history. Zero keeps everything.
	Retention Retention `koanf:"retention"`
}

// Retention bounds how long persisted history is kept. The newest state per
// provider always survives, so cleanup never removes what routing reads.
type Retention struct {
	QuotaSnapshots   time.Duration `koanf:"quota_snapshots" validate:"gte=0"`
	ProviderHealth   time.Duration `koanf:"provider_health" validate:"gte=0"`
	UsageRecords     time.Duration `koanf:"usage_records" validate:"gte=0"`
	RoutingDecisions time.Duration `koanf:"routing_decisions" validate:"gte=0"`
}

// Auth holds the bearer token for the controller's own HTTP API.
type Auth struct {
	Token Secret `koanf:"token"`
}

// Observability configures logging.
type Observability struct {
	Level string `koanf:"level" validate:"required,oneof=debug info warn error"`
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

// UserConfigPath returns the user-level configuration path,
// ~/.config/quota-router/config.yaml on Linux (XDG_CONFIG_HOME honored,
// $HOME/Library/Application Support on Darwin). It is outside the install
// directory on purpose: a reinstall replaces the binary and leaves this file
// alone, so operator settings and secrets survive an upgrade.
//
// An error means no home directory could be resolved — for example in a
// container running without HOME — and the caller then has no default file to
// fall back to.
func UserConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config directory: %w", err)
	}
	return filepath.Join(dir, "quota-router", "config.yaml"), nil
}

// ConfigPath returns the path the configuration is read from, applying the one
// documented rule both the daemon and `init-config` use: an explicit
// QUOTA_ROUTER_CONFIG_FILE wins — whether or not the file exists yet — and
// otherwise the user-level path is used when that file is present.
//
// It returns "" when nothing is configured and no default file exists: the
// service then starts on defaults, which is not an error.
//
// init-config writes here rather than to UserConfigPath so a file it creates is
// the file the daemon reads. Writing to the default path while the variable
// named another one would let an operator believe configuration was initialized
// while the daemon kept reading somewhere else.
func ConfigPath() string {
	if path := os.Getenv(ConfigFileEnv); path != "" {
		return path
	}
	path, err := UserConfigPath()
	if err != nil {
		return "" // no home: nothing to read, defaults only
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return path
}

// Load resolves the configuration in documented precedence order and validates
// it. The YAML file is the one ConfigPath selects. An explicit path that is
// unreadable or unparsable, and a default file that is unreadable, are startup
// errors naming the file.
//
// The file is read as it is: its keys are literal. The environment is
// normalized before it is merged, so a variable that addresses a hyphenated
// alias reaches the same configuration path the file key does.
func Load() (Config, error) {
	cfg := Default()

	k := koanf.New(".")
	if path := ConfigPath(); path != "" {
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
	if err := loadEnv(k); err != nil {
		return Config{}, err
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

// cataloguePath is where the model catalogue lives in the configuration.
const cataloguePath = "litellm.model_aliases"

// envSource returns the environment variables the loader reads. It is
// os.Environ in production; tests replace it to control which variables exist
// and in which order they are seen.
var envSource = os.Environ

// envVar is one environment variable after the loader has normalized its name.
type envVar struct {
	name  string   // the variable name as it appears in the environment
	path  []string // the configuration path it addresses
	value string
}

// collectEnv turns every environment variable the loader understands into the
// configuration path it addresses. This is the syntax layer: the prefix, the
// deprecated flat names and the _H_ escape. No alias is resolved here, so the
// result does not depend on the catalogue.
func collectEnv(src []string) []envVar {
	var out []envVar
	seen := map[string]bool{}
	for _, kv := range src {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, EnvPrefix) || seen[name] {
			continue
		}
		path, ok := envPath(name, value, src)
		if !ok {
			continue
		}
		seen[name] = true
		out = append(out, envVar{name: name, path: path, value: value})
	}
	return out
}

// envPath maps an environment variable name to the configuration path it
// addresses: QUOTA_ROUTER_SERVER__ADDR becomes server.addr, and the deprecated
// flat QUOTA_ROUTER_ADDR the same place. The double delimiter leaves single
// underscores free for field names such as base_url. A hyphen in a name
// segment (an alias such as free-coding) is written as _H_, so FREE_H_CODING
// addresses free-coding; file keys are literal.
//
// ok is false when the variable carries no configuration value: an empty value
// means unset, a _FILE variable is loaded from its file separately, and a
// deprecated flat name gives way to its current counterpart.
func envPath(name, value string, src []string) ([]string, bool) {
	if value == "" || name == ConfigFileEnv || strings.HasSuffix(name, "_FILE") {
		return nil, false
	}
	raw := strings.ToLower(strings.TrimPrefix(name, EnvPrefix))
	if current, ok := legacyEnvKeys[raw]; ok {
		if envSet(src, EnvPrefix+strings.ToUpper(current)) {
			return nil, false // the current name wins over its deprecated alias
		}
		return strings.Split(current, EnvDelim), true
	}
	return decodeEnvHyphens(raw), true
}

// decodeEnvHyphens replaces the _H_ escape with a hyphen in every segment of
// an environment variable name, so FREE_H_CODING addresses free-coding.
// Segments are decoded one by one, so a segment that is exactly "h" keeps its
// name and the __ nesting delimiter is never touched.
//
// The escape is reserved in environment names: it always decodes, so an alias
// whose name contains "_h_" can only be set from a configuration file.
func decodeEnvHyphens(name string) []string {
	segments := strings.Split(name, EnvDelim)
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(s, envHyphenEscape, "-")
	}
	return segments
}

// envSet reports whether a variable is set to a non-empty value in the
// environment the loader reads.
func envSet(src []string, name string) bool {
	for _, kv := range src {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name && v != "" {
			return true
		}
	}
	return false
}

// catalogueAliases returns every alias name the catalogue holds: the aliases
// the file declares plus the aliases the environment declares.
func catalogueAliases(k *koanf.Koanf, vars []envVar) map[string]bool {
	known := map[string]bool{}
	if aliases, ok := k.Get(cataloguePath).(map[string]interface{}); ok {
		for alias := range aliases {
			known[alias] = true
		}
	}
	for _, v := range vars {
		if len(v.path) > 2 && v.path[0] == "litellm" && v.path[1] == "model_aliases" {
			known[v.path[2]] = true
		}
	}
	return known
}

// resolveRoutingAliases rewrites the alias of every routing model path to the
// catalogue alias it addresses. Only environment paths are rewritten: file
// keys are literal.
func resolveRoutingAliases(vars []envVar, known map[string]bool) {
	for i := range vars {
		pos, ok := routingAlias(vars[i].path)
		if !ok {
			continue
		}
		vars[i].path[pos] = resolveAlias(vars[i].path[pos], known)
	}
}

// routingAlias returns the position of the alias in a routing model path,
// routing.providers.<provider>.models.<alias>.
func routingAlias(path []string) (int, bool) {
	if len(path) == 5 && path[0] == "routing" && path[1] == "providers" && path[3] == "models" {
		return 4, true
	}
	return 0, false
}

// resolveAlias maps a routing model key to the catalogue alias it addresses.
// The exact name wins. Otherwise the key matches an alias that differs only in
// the separator (free_coding for free-coding), which is what an environment
// variable without hyphens produces. A key that matches nothing is left as it
// is, and validation reports it as an unknown alias.
func resolveAlias(key string, known map[string]bool) string {
	if known[key] {
		return key
	}
	if hyphenated := strings.ReplaceAll(key, "_", "-"); known[hyphenated] {
		return hyphenated
	}
	return key
}

// checkEnvCollisions reports the variables that address one configuration
// path. Two spellings of one key are a startup error: the loader must not
// pick a winner by environment order, and that order is not the operator's.
//
// The message is deterministic: the paths are sorted, and so are the variable
// names inside one path, so the same conflict always reads the same way.
func checkEnvCollisions(vars []envVar) error {
	groups := map[string][]string{}
	for _, v := range vars {
		path := strings.Join(v.path, ".")
		groups[path] = append(groups[path], v.name)
	}
	paths := make([]string, 0, len(groups))
	for path, names := range groups {
		if len(names) > 1 {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	conflicts := make([]string, 0, len(paths))
	for _, path := range paths {
		names := groups[path]
		sort.Strings(names)
		conflicts = append(conflicts, fmt.Sprintf("%s all set %s", strings.Join(names, ", "), path))
	}
	return fmt.Errorf("environment: conflicting variables: %s", strings.Join(conflicts, "; "))
}

// loadEnv normalizes the environment and loads it into koanf. Normalization
// happens before the merge, so a variable addresses exactly the configuration
// path a file key does and the environment wins over the file. Conflicting
// variables are reported before anything is loaded.
func loadEnv(k *koanf.Koanf) error {
	src := envSource()
	vars := collectEnv(src)
	resolveRoutingAliases(vars, catalogueAliases(k, vars))
	if err := checkEnvCollisions(vars); err != nil {
		return err
	}
	paths := make(map[string]envVar, len(vars))
	for _, v := range vars {
		paths[v.name] = v
	}
	err := k.Load(env.Provider(EnvDelim, env.Opt{
		Prefix:      EnvPrefix,
		EnvironFunc: func() []string { return src },
		TransformFunc: func(name, value string) (string, any) {
			v, ok := paths[name]
			if !ok {
				return "", nil
			}
			return strings.Join(v.path, EnvDelim), v.value
		},
	}), nil)
	if err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	return nil
}
