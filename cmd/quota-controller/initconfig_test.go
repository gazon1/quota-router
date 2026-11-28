package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gazon1/quota-router/cmd/quota-controller/templates"
	"github.com/gazon1/quota-router/internal/config"
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
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	if got, want := string(templates.ConfigExample), string(example); got != want {
		t.Fatal("templates/config.example.yaml has drifted from the root config.example.yaml; copy it again")
	}
}

func TestInitConfigWritesUserPath(t *testing.T) {
	home := isolatedHome(t)
	var out, errOut bytes.Buffer

	if code := initConfig(nil, &out, &errOut); code != 0 {
		t.Fatalf("initConfig() = %d, stderr=%s", code, errOut.String())
	}
	path := filepath.Join(home, ".config", "quota-router", "config.yaml")
	if !strings.Contains(out.String(), path) {
		t.Fatalf("stdout = %q, want it to name %q", out.String(), path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(body) != string(templates.ConfigExample) {
		t.Fatal("written file is not the embedded starter")
	}
}

// The file outlives a reinstall by design, so a second run must never clobber
// what an operator has since put there — secrets included.
func TestInitConfigRefusesToOverwrite(t *testing.T) {
	isolatedHome(t)
	var out, errOut bytes.Buffer
	if code := initConfig(nil, &out, &errOut); code != 0 {
		t.Fatalf("first initConfig() = %d, stderr=%s", code, errOut.String())
	}
	path, err := config.UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath: %v", err)
	}
	edited := []byte("server:\n  addr: \"127.0.0.1:9999\"\n")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := initConfig(nil, &out, &errOut); code == 0 {
		t.Fatal("initConfig() overwrote an existing file")
	}
	if !strings.Contains(errOut.String(), "already exists") {
		t.Fatalf("stderr = %q, want it to say the file already exists", errOut.String())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(edited) {
		t.Fatal("the existing file was modified")
	}
}

// The written file must be exactly what Load reads: mode 0600, in a 0700
// directory, at the path the daemon resolves with no environment variable set.
func TestInitConfigResultIsLoadableAndPrivate(t *testing.T) {
	isolatedHome(t)
	var out, errOut bytes.Buffer
	if code := initConfig(nil, &out, &errOut); code != 0 {
		t.Fatalf("initConfig() = %d, stderr=%s", code, errOut.String())
	}
	path, err := config.UserConfigPath()
	if err != nil {
		t.Fatalf("UserConfigPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory mode = %04o, want 0700", perm)
	}
	// The shipped example is a valid configuration: loading it must not fail.
	t.Setenv("QUOTA_ROUTER_CONFIG_FILE", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() of the written file: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Fatalf("Server.Addr = %q, want the example's value", cfg.Server.Addr)
	}
}

func TestInitConfigAcceptsExplicitPath(t *testing.T) {
	isolatedHome(t)
	path := filepath.Join(t.TempDir(), "elsewhere", "custom.yaml")
	var out, errOut bytes.Buffer
	if code := initConfig([]string{path}, &out, &errOut); code != 0 {
		t.Fatalf("initConfig(path) = %d, stderr=%s", code, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func TestInitConfigRejectsExtraArguments(t *testing.T) {
	isolatedHome(t)
	var out, errOut bytes.Buffer
	if code := initConfig([]string{"a", "b"}, &out, &errOut); code != 2 {
		t.Fatalf("initConfig(2 args) = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Fatalf("stderr = %q, want usage", errOut.String())
	}
}

// -h is help, never a path. Treating it as one wrote a file literally named "-h"
// into the working directory.
func TestInitConfigHelpDoesNotCreateFiles(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			home := isolatedHome(t)
			w := t.TempDir()
			wd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(w); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(wd) })

			var out, errOut bytes.Buffer
			if code := initConfig([]string{flag}, &out, &errOut); code != 0 {
				t.Fatalf("initConfig(%q) = %d, want 0", flag, code)
			}
			if !strings.Contains(out.String(), "usage:") {
				t.Fatalf("stdout = %q, want the usage text", out.String())
			}
			// Nothing may be created: not in the working directory, not at home.
			entries, err := os.ReadDir(w)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("working directory gained %v, want nothing", names)
			}
			if _, err := os.Stat(filepath.Join(home, ".config", "quota-router", "config.yaml")); !os.IsNotExist(err) {
				t.Fatal("help wrote the user config, want nothing created")
			}
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
	if err := writeNewFile(path, []byte("ok\n")); err != nil {
		t.Fatalf("writeNewFile: %v", err)
	}
	// A second call must refuse and leave the first file intact.
	if err := writeNewFile(path, []byte("second\n")); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want an already-exists refusal", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok\n" {
		t.Fatalf("content = %q, want the first write intact", body)
	}
}

// QUOTA_ROUTER_CONFIG_FILE is what the daemon reads, so init-config must write
// there too. Writing to the user-level path instead let an operator believe
// configuration was initialized while the daemon read another file.
func TestInitConfigHonorsConfigFileEnv(t *testing.T) {
	home := isolatedHome(t)
	explicit := filepath.Join(t.TempDir(), "etc", "quota", "config.yaml")
	t.Setenv("QUOTA_ROUTER_CONFIG_FILE", explicit)

	var out, errOut bytes.Buffer
	if code := initConfig(nil, &out, &errOut); code != 0 {
		t.Fatalf("initConfig() = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), explicit) {
		t.Fatalf("stdout = %q, want it to name %q", out.String(), explicit)
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("stat %s: %v", explicit, err)
	}
	// The user-level path must be untouched: one file, the one the daemon reads.
	if _, err := os.Stat(filepath.Join(home, ".config", "quota-router", "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("init-config also wrote the user-level path, want only the configured one")
	}

	// The daemon then reads exactly what was written.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() of the written file: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Fatalf("Server.Addr = %q, want it read from %s", cfg.Server.Addr, explicit)
	}
}

// An explicit path argument still wins over the variable, so the command can
// write a sample somewhere else on purpose.
func TestInitConfigArgumentBeatsConfigFileEnv(t *testing.T) {
	isolatedHome(t)
	t.Setenv("QUOTA_ROUTER_CONFIG_FILE", filepath.Join(t.TempDir(), "from-env.yaml"))
	argPath := filepath.Join(t.TempDir(), "from-arg.yaml")

	var out, errOut bytes.Buffer
	if code := initConfig([]string{argPath}, &out, &errOut); code != 0 {
		t.Fatalf("initConfig(path) = %d, stderr=%s", code, errOut.String())
	}
	if _, err := os.Stat(argPath); err != nil {
		t.Fatalf("stat %s: %v", argPath, err)
	}
	if _, err := os.Stat(os.Getenv("QUOTA_ROUTER_CONFIG_FILE")); !os.IsNotExist(err) {
		t.Fatal("the environment path was written even though an argument named another file")
	}
}
