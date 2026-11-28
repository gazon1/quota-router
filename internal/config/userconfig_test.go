package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedHome points the user config directory at a temporary tree, so a test
// states its own HOME instead of reading the developer's.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestUserConfigPathIsUnderHomeConfig(t *testing.T) {
	home := isolatedHome(t)
	got, err := UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath() error = %v", err)
	}
	want := filepath.Join(home, ".config", "quota-router", "config.yaml")
	if got != want {
		t.Fatalf("UserConfigPath() = %q, want %q", got, want)
	}
}

// XDG_CONFIG_HOME is honored, because that is what os.UserConfigDir documents
// and a session that relocates it must find the same file the loader reads.
func TestUserConfigPathHonorsXDG(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath() error = %v", err)
	}
	if want := filepath.Join(dir, "quota-router", "config.yaml"); got != want {
		t.Fatalf("UserConfigPath() = %q, want %q", got, want)
	}
}

// The whole point of the user-level path: a config written there is read with no
// environment variable set, so it survives anything that replaces the binary.
func TestLoadReadsUserConfigPathByDefault(t *testing.T) {
	isolatedHome(t)
	path, err := UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("server:\n  addr: \"127.0.0.1:7777\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:7777" {
		t.Fatalf("Server.Addr = %q, want the value from the user-level file", cfg.Server.Addr)
	}
}

// An absent default file is not an error: the service starts on defaults.
func TestLoadWithoutUserConfigUsesDefaults(t *testing.T) {
	isolatedHome(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != Default().Server.Addr {
		t.Fatalf("Server.Addr = %q, want the default", cfg.Server.Addr)
	}
}

// An explicit path wins over the default file, and still must exist: a typo is
// reported rather than silently running on the user-level file.
func TestExplicitConfigPathWinsAndMustExist(t *testing.T) {
	isolatedHome(t)
	defaultPath, err := UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte("server:\n  addr: \"127.0.0.1:7777\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	explicit := writeConfig(t, "server:\n  addr: \"127.0.0.1:8888\"")
	t.Setenv(ConfigFileEnv, explicit)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:8888" {
		t.Fatalf("Server.Addr = %q, want the explicitly named file", cfg.Server.Addr)
	}

	absent := filepath.Join(t.TempDir(), "typo.yaml")
	t.Setenv(ConfigFileEnv, absent)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), absent) {
		t.Fatalf("error = %v, want it to name the missing explicit file", err)
	}
}

// No home directory means no default path and no error from that: a container
// running without HOME still starts, just from defaults and the environment.
func TestLoadWithoutHomeUsesDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want defaults when no home resolves", err)
	}
	if cfg.Server.Addr != Default().Server.Addr {
		t.Fatalf("Server.Addr = %q, want the default", cfg.Server.Addr)
	}
}

// A default file that exists but cannot be parsed is reported by name, not
// silently skipped: the operator must learn which file broke startup, the same
// way an explicit path is reported.
func TestUnreadableDefaultFileIsNamed(t *testing.T) {
	isolatedHome(t)
	path, err := UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("server: [this is not a mapping"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want it to name %q", err, path)
	}
}
