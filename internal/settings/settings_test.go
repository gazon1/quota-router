package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
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
