package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gazon1/quota-router/cmd/quota-controller/templates"
	"github.com/gazon1/quota-router/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolatedHome points the user config directory at a temporary tree.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// The embedded starter must be the shipped example, not a stale copy: an operator
// who runs init-config gets the configuration the docs describe.
func TestInitConfigTemplateMatchesExample(t *testing.T) {
	example, err := os.ReadFile("../../config.example.yaml")
	require.NoError(t, err, "read config.example.yaml")
	assert.Equal(t, string(example), string(templates.ConfigExample),
		"templates/config.example.yaml has drifted from the root config.example.yaml; copy it again")
}

func TestInitConfigWritesUserPath(t *testing.T) {
	home := isolatedHome(t)
	var out, errOut bytes.Buffer

	code := initConfig(nil, &out, &errOut)
	require.Equal(t, 0, code, "initConfig() stderr=%s", errOut.String())
	path := filepath.Join(home, ".config", "quota-router", "config.yaml")
	assert.Contains(t, out.String(), path)
	body, err := os.ReadFile(path)
	require.NoError(t, err, "read written config")
	assert.Equal(t, string(templates.ConfigExample), string(body), "written file is not the embedded starter")
}

// The file outlives a reinstall by design, so a second run must never clobber
// what an operator has since put there — secrets included.
func TestInitConfigRefusesToOverwrite(t *testing.T) {
	isolatedHome(t)
	var out, errOut bytes.Buffer
	code := initConfig(nil, &out, &errOut)
	require.Equal(t, 0, code, "first initConfig() stderr=%s", errOut.String())
	path, err := config.UserConfigPath()
	require.NoError(t, err, "UserConfigPath")
	edited := []byte("server:\n  addr: \"127.0.0.1:9999\"\n")
	require.NoError(t, os.WriteFile(path, edited, 0o600), "write the edited file")

	out.Reset()
	errOut.Reset()
	code = initConfig(nil, &out, &errOut)
	require.NotEqual(t, 0, code, "initConfig() overwrote an existing file")
	assert.Contains(t, errOut.String(), "already exists")
	body, err := os.ReadFile(path)
	require.NoError(t, err, "read the existing file")
	assert.Equal(t, string(edited), string(body), "the existing file was modified")
}

// The written file must be exactly what Load reads: mode 0600, in a 0700
// directory, at the path the daemon resolves with no environment variable set.
func TestInitConfigResultIsLoadableAndPrivate(t *testing.T) {
	isolatedHome(t)
	var out, errOut bytes.Buffer
	code := initConfig(nil, &out, &errOut)
	require.Equal(t, 0, code, "initConfig() stderr=%s", errOut.String())
	path, err := config.UserConfigPath()
	require.NoError(t, err, "UserConfigPath")
	info, err := os.Stat(path)
	require.NoError(t, err, "stat the written config")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err, "stat the config directory")
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
	// The shipped example is a valid configuration: loading it must not fail.
	t.Setenv("QUOTA_ROUTER_CONFIG_FILE", "")
	cfg, err := config.Load()
	require.NoError(t, err, "Load() of the written file")
	assert.Equal(t, ":8080", cfg.Server.Addr, "the example's value")
}

func TestInitConfigAcceptsExplicitPath(t *testing.T) {
	isolatedHome(t)
	path := filepath.Join(t.TempDir(), "elsewhere", "custom.yaml")
	var out, errOut bytes.Buffer
	code := initConfig([]string{path}, &out, &errOut)
	require.Equal(t, 0, code, "initConfig(path) stderr=%s", errOut.String())
	_, err := os.Stat(path)
	require.NoError(t, err, "stat %s", path)
}

func TestInitConfigRejectsExtraArguments(t *testing.T) {
	isolatedHome(t)
	var out, errOut bytes.Buffer
	code := initConfig([]string{"a", "b"}, &out, &errOut)
	assert.Equal(t, 2, code, "initConfig(2 args)")
	assert.Contains(t, errOut.String(), "usage:")
}

// -h is help, never a path. Treating it as one wrote a file literally named "-h"
// into the working directory.
func TestInitConfigHelpDoesNotCreateFiles(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			home := isolatedHome(t)
			w := t.TempDir()
			wd, err := os.Getwd()
			require.NoError(t, err, "getwd")
			require.NoError(t, os.Chdir(w), "chdir")
			t.Cleanup(func() { _ = os.Chdir(wd) })

			var out, errOut bytes.Buffer
			code := initConfig([]string{flag}, &out, &errOut)
			require.Equal(t, 0, code, "initConfig(%q)", flag)
			assert.Contains(t, out.String(), "usage:")
			// Nothing may be created: not in the working directory, not at home.
			entries, err := os.ReadDir(w)
			require.NoError(t, err, "read the working directory")
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				require.Fail(t, "the working directory gained files", "working directory gained %v, want nothing", names)
			}
			_, err = os.Stat(filepath.Join(home, ".config", "quota-router", "config.yaml"))
			assert.True(t, os.IsNotExist(err), "help wrote the user config, want nothing created")
		})
	}
}

// writeNewFile creates the file once and refuses afterwards, leaving the first
// contents intact. The partial-file removal on a failed write is not exercised
// here: forcing a mid-write failure portably needs an injectable writer, which
// would be more machinery than the four cleanup lines it would cover.
func TestWriteNewFileCreatesOnceAndRefusesAfter(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a file cannot be created; force the write itself to
	// fail by making the target a directory-less path on a read-only dir is
	// platform-specific, so use /dev/full via a path we can actually open.
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, writeNewFile(path, []byte("ok\n")), "writeNewFile")
	// A second call must refuse and leave the first file intact.
	err := writeNewFile(path, []byte("second\n"))
	require.Error(t, err, "error = %v, want an already-exists refusal", err)
	assert.Contains(t, err.Error(), "already exists")
	body, err := os.ReadFile(path)
	require.NoError(t, err, "read the written file")
	assert.Equal(t, "ok\n", string(body), "the first write must be intact")
}

// QUOTA_ROUTER_CONFIG_FILE is what the daemon reads, so init-config must write
// there too. Writing to the user-level path instead let an operator believe
// configuration was initialized while the daemon read another file.
func TestInitConfigHonorsConfigFileEnv(t *testing.T) {
	home := isolatedHome(t)
	explicit := filepath.Join(t.TempDir(), "etc", "quota", "config.yaml")
	t.Setenv("QUOTA_ROUTER_CONFIG_FILE", explicit)

	var out, errOut bytes.Buffer
	code := initConfig(nil, &out, &errOut)
	require.Equal(t, 0, code, "initConfig() stderr=%s", errOut.String())
	assert.Contains(t, out.String(), explicit)
	_, err := os.Stat(explicit)
	require.NoError(t, err, "stat %s", explicit)
	// The user-level path must be untouched: one file, the one the daemon reads.
	_, err = os.Stat(filepath.Join(home, ".config", "quota-router", "config.yaml"))
	assert.True(t, os.IsNotExist(err), "init-config also wrote the user-level path, want only the configured one")

	// The daemon then reads exactly what was written.
	cfg, err := config.Load()
	require.NoError(t, err, "Load() of the written file")
	assert.Equal(t, ":8080", cfg.Server.Addr, "it must be read from %s", explicit)
}

// An explicit path argument still wins over the variable, so the command can
// write a sample somewhere else on purpose.
func TestInitConfigArgumentBeatsConfigFileEnv(t *testing.T) {
	isolatedHome(t)
	t.Setenv("QUOTA_ROUTER_CONFIG_FILE", filepath.Join(t.TempDir(), "from-env.yaml"))
	argPath := filepath.Join(t.TempDir(), "from-arg.yaml")

	var out, errOut bytes.Buffer
	code := initConfig([]string{argPath}, &out, &errOut)
	require.Equal(t, 0, code, "initConfig(path) stderr=%s", errOut.String())
	_, err := os.Stat(argPath)
	require.NoError(t, err, "stat %s", argPath)
	_, err = os.Stat(os.Getenv("QUOTA_ROUTER_CONFIG_FILE"))
	assert.True(t, os.IsNotExist(err), "the environment path was written even though an argument named another file")
}
