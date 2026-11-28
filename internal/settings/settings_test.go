package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSecretRedaction(t *testing.T) {
	s := Secret("sk-live-do-not-print")
	for name, got := range map[string]string{
		"sprintf %s":  s.String(),
		"sprintf %q":  fmt.Sprintf("%q", s),
		"sprintf %v":  fmt.Sprintf("%v", s),
		"sprintf %#v": fmt.Sprintf("%#v", s),
		"String()":    s.String(),
		"GoString()":  s.GoString(),
	} {
		if strings.Contains(got, "sk-live") {
			t.Fatalf("%s = %q, leaked the secret", name, got)
		}
	}

	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "token", s)
	if strings.Contains(buf.String(), "sk-live") {
		t.Fatalf("slog output leaked the secret: %s", buf.String())
	}

	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "token", s)
	if strings.Contains(buf.String(), "sk-live") {
		t.Fatalf("JSON log output leaked the secret: %s", buf.String())
	}
	encoded, err := json.Marshal(struct{ T Secret }{T: s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sk-live") {
		t.Fatalf("json.Marshal leaked the secret: %s", encoded)
	}
}

func TestSettingsRuntimeTypesUsableWithoutConfigLoader(t *testing.T) {
	enabled := true
	m := Model{
		Model:        "provider/model",
		Enabled:      &enabled,
		ContextLimit: 128000,
		Capabilities: Capabilities{Streaming: CapabilitySupported},
		Tasks:        []string{"coding"},
		Unavailable: &Unavailable{
			Timezone: "UTC",
			Windows:  []UnavailableWindow{{Days: []string{"monday"}, Start: "00:00", End: "01:00"}},
		},
	}
	if !m.IsEnabled() || m.Provider() != "provider" {
		t.Fatalf("model = %+v", m)
	}
	if _, ok := m.Unavailable.WindowAt(time.Date(2026, time.November, 30, 0, 30, 0, 0, time.UTC)); !ok {
		t.Fatal("expected window to match")
	}

	cat := Catalogue{ModelAliases: map[string]Model{"m": m}, Routing: Routing{LowQuotaThreshold: 0.1}}
	if got := cat.Candidates(Requirements{Streaming: true, Tasks: []string{"coding"}}); len(got) != 1 || got[0] != "m" {
		t.Fatalf("Candidates = %v", got)
	}

	var sec Secret = "sk-test"
	if string(sec) != "sk-test" || strings.Contains(sec.String(), "sk-test") {
		t.Fatalf("secret redaction/runtime behavior broken: %q %q", string(sec), sec.String())
	}
}
