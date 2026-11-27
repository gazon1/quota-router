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
