package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		assert.NotContains(t, got, "sk-live", "%s leaked the secret", name)
	}

	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "token", s)
	assert.NotContains(t, buf.String(), "sk-live", "slog output leaked the secret")

	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "token", s)
	assert.NotContains(t, buf.String(), "sk-live", "JSON log output leaked the secret")
	encoded, err := json.Marshal(struct{ T Secret }{T: s})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "sk-live", "json.Marshal leaked the secret")
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
	assert.True(t, m.IsEnabled(), "model = %+v", m)
	assert.Equal(t, "provider", m.Provider(), "model = %+v", m)
	_, ok := m.Unavailable.WindowAt(time.Date(2026, time.November, 30, 0, 30, 0, 0, time.UTC))
	assert.True(t, ok, "expected window to match")

	cat := Catalogue{ModelAliases: map[string]Model{"m": m}, Routing: Routing{LowQuotaThreshold: 0.1}}
	got := cat.Candidates(Requirements{Streaming: true, Tasks: []string{"coding"}})
	assert.Len(t, got, 1)
	assert.Equal(t, "m", got[0])

	var sec Secret = "sk-test"
	assert.Equal(t, "sk-test", string(sec))
	assert.NotContains(t, sec.String(), "sk-test", "secret redaction/runtime behavior broken")
}
