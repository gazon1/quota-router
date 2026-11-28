package config

import (
	"github.com/gazon1/quota-router/internal/settings"

	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/knadh/koanf/v2"
)

// Secret is defined in internal/settings; config keeps Secret-file loading
// and config-file rejection behavior.

// loadSecretFiles applies values mounted as files so a container can use
// Docker/Kubernetes secrets instead of environment values: an environment
// variable ending in _FILE names a readable file whose trimmed contents become
// the setting. For example
// QUOTA_ROUTER_PROVIDERS__MINIMAX__API_KEY_FILE=/run/secrets/minimax_api_key
// sets providers.minimax.api_key. Only Secret fields are accepted, so no
// arbitrary variable is ever read as a path; the file must exist and be
// readable, and its path is never used as the value itself. A plain variable
// that is already set wins, so an unusable file never fails a configured value.
func loadSecretFiles(k *koanf.Koanf) error {
	allowed := secretPaths()
	for _, kv := range os.Environ() {
		name, path, ok := strings.Cut(kv, "=")
		if !ok || path == "" || name == ConfigFileEnv || !strings.HasPrefix(name, EnvPrefix) || !strings.HasSuffix(name, "_FILE") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(name, EnvPrefix), "_FILE")
		key = strings.ReplaceAll(strings.ToLower(key), EnvDelim, ".")
		// The allowlist is checked first: a _FILE naming a non-secret setting is
		// a configuration mistake and stays an error even when its plain
		// counterpart is set, so it never goes silent.
		if !matches(allowed, key) {
			return fmt.Errorf("%s: not a secret setting; only %s can be set from a file", name, strings.Join(setKeys(allowed), ", "))
		}
		if os.Getenv(strings.TrimSuffix(name, "_FILE")) != "" {
			continue // the plain variable overrides this file
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := k.Set(key, strings.TrimSpace(string(b))); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// secretPaths lists every Secret field path, e.g. auth.token. Map values such as
// providers.<name>.api_key are listed under their wildcard parent.
func secretPaths() map[string]bool {
	paths := map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			switch f.Type.Kind() {
			case reflect.Struct:
				walk(f.Type, prefix+name+".")
			case reflect.Map:
				if f.Type.Elem().Kind() == reflect.Struct {
					walk(f.Type.Elem(), prefix+name+".*.")
				}
			default:
				if f.Type == reflect.TypeOf(settings.Secret("")) {
					paths[prefix+name] = true
				}
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return paths
}

// matches reports whether key is a listed path, treating * as one level.
func matches(m map[string]bool, key string) bool {
	if m[key] {
		return true
	}
	parts := strings.Split(key, ".")
	for p := range m {
		pattern := strings.Split(p, ".")
		if len(pattern) != len(parts) {
			continue
		}
		ok := true
		for i := range pattern {
			if pattern[i] != "*" && pattern[i] != parts[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func setKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// rejectSecretsInFile keeps credentials out of a committed artifact: the config
// file may be copied into a deployment or a repository, so a Secret there is a
// startup error that names the setting, never the value. Use the environment or
// a mounted secret file instead.
func rejectSecretsInFile(k *koanf.Koanf, path string) error {
	allowed := secretPaths()
	for _, key := range k.Keys() {
		if !matches(allowed, key) || k.Get(key) == nil {
			continue
		}
		return fmt.Errorf("config file %s: %s must not be set there; use the environment or a mounted secret file", path, key)
	}
	return nil
}
