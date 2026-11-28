package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	want := filepath.Join(home, ".config", "quota-router", "config.yaml")
	assert.Equal(t, want, got)
}

// XDG_CONFIG_HOME is honored, because that is what os.UserConfigDir documents
// and a session that relocates it must find the same file the loader reads.
func TestUserConfigPathHonorsXDG(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := UserConfigPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "quota-router", "config.yaml"), got)
}

// The whole point of the user-level path: a config written there is read with no
// environment variable set, so it survives anything that replaces the binary.
func TestLoadReadsUserConfigPathByDefault(t *testing.T) {
	isolatedHome(t)
	path, err := UserConfigPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("server:\n  addr: \"127.0.0.1:7777\"\n"), 0o600))

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", cfg.Server.Addr, "want the value from the user-level file")
}

// An absent default file is not an error: the service starts on defaults.
func TestLoadWithoutUserConfigUsesDefaults(t *testing.T) {
	isolatedHome(t)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, Default().Server.Addr, cfg.Server.Addr, "want the default")
}

// An explicit path wins over the default file, and still must exist: a typo is
// reported rather than silently running on the user-level file.
func TestExplicitConfigPathWinsAndMustExist(t *testing.T) {
	isolatedHome(t)
	defaultPath, err := UserConfigPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(defaultPath), 0o700))
	require.NoError(t, os.WriteFile(defaultPath, []byte("server:\n  addr: \"127.0.0.1:7777\"\n"), 0o600))

	explicit := writeConfig(t, "server:\n  addr: \"127.0.0.1:8888\"")
	t.Setenv(ConfigFileEnv, explicit)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8888", cfg.Server.Addr, "want the explicitly named file")

	absent := filepath.Join(t.TempDir(), "typo.yaml")
	t.Setenv(ConfigFileEnv, absent)
	_, err = Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, absent, "want it to name the missing explicit file")
}

// No home directory means no default path and no error from that: a container
// running without HOME still starts, just from defaults and the environment.
func TestLoadWithoutHomeUsesDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	cfg, err := Load()
	require.NoError(t, err, "want defaults when no home resolves")
	assert.Equal(t, Default().Server.Addr, cfg.Server.Addr, "want the default")
}

// A default file that exists but cannot be parsed is reported by name, not
// silently skipped: the operator must learn which file broke startup, the same
// way an explicit path is reported.
func TestUnreadableDefaultFileIsNamed(t *testing.T) {
	isolatedHome(t)
	path, err := UserConfigPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("server: [this is not a mapping"), 0o600))
	_, err = Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, path, "want it to name the file")
}
