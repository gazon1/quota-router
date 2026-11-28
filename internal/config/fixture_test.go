package config

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture loads a YAML scenario from the package's testdata directory. The
// error names the fixture path, so a missing or unreadable fixture is obvious
// without guessing which one the test wanted.
func fixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(body)
}
