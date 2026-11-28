package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnavailableValidation(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{
			name: "valid",
			file: "valid.yaml",
		},
		{
			name: "valid overnight",
			file: "valid-overnight.yaml",
		},
		{
			name: "no windows",
			file: "no-windows.yaml",
		},
		{
			name: "missing timezone",
			file: "missing-timezone.yaml",
			want: "timezone must name an IANA time zone",
		},
		{
			name: "unknown timezone",
			file: "unknown-timezone.yaml",
			want: "timezone must name an IANA time zone",
		},
		{
			// Go resolves Local to the host's zone, which would make the same config
			// cover different hours on different machines.
			name: "Local is not IANA",
			file: "local-timezone.yaml",
			want: "timezone must name an IANA time zone",
		},
		{
			name: "UTC is IANA",
			file: "utc-timezone.yaml",
		},
		{
			name: "real IANA name",
			file: "iana-timezone.yaml",
		},
		{
			name: "unknown weekday",
			file: "unknown-weekday.yaml",
			want: "days must name weekdays",
		},
		{
			name: "no weekday",
			file: "no-weekday.yaml",
			want: "days must name at least one weekday",
		},
		{
			name: "malformed start",
			file: "malformed-start.yaml",
			want: "start must be a 24-hour HH:MM time",
		},
		{
			name: "out of range end",
			file: "out-of-range-end.yaml",
			want: "end must be a 24-hour HH:MM time",
		},
		{
			name: "equal boundaries",
			file: "equal-boundaries.yaml",
			want: "start and end must differ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			configPath := filepath.Join("testdata", "unavailable", tt.file)
			t.Setenv(ConfigFileEnv, configPath)

			_, err := Load()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Load() error = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one naming %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "litellm.model_aliases.free-coding.unavailable") {
				t.Fatalf("error = %v, want it to name the unavailable setting path", err)
			}
		})
	}
}

// The setting is file-configured only: a nested list has no honest environment
// encoding, and the loader must reject the invented one rather than guess.
func TestUnavailableIsFileConfigured(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  model_aliases:
    free-coding:
      model: commandcode/stealth/glyph-cluster:free
      unavailable:
        timezone: Asia/Yerevan
        windows:
          - days: [saturday, sunday]
            start: "02:00"
            end: "06:00"
          - days: [monday]
            start: "12:00"
            end: "14:30"
`))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	u := cfg.LiteLLM.ModelAliases["free-coding"].Unavailable
	if u == nil || u.Timezone != "Asia/Yerevan" || len(u.Windows) != 2 {
		t.Fatalf("Unavailable = %+v, want the configured zone and two windows", u)
	}
	if w := u.Windows[0]; w.Days[1] != "sunday" || w.Start != "02:00" || w.End != "06:00" {
		t.Fatalf("first window = %+v", w)
	}
	// The schedule survives the conversion to the runtime catalogue, where
	// routing reads it: Friday 16:00 in Yerevan is outside both windows, and
	// Monday 14:00 is inside one.
	runtime := cfg.Catalogue().ModelAliases["free-coding"].Unavailable
	if runtime == nil {
		t.Fatal("the runtime catalogue lost the unavailable schedule")
	}
	if _, ok := runtime.WindowAt(time.Date(2026, time.November, 27, 12, 0, 0, 0, time.UTC)); ok {
		t.Fatal("friday 16:00 Yerevan must not be restricted")
	}
	if _, ok := runtime.WindowAt(time.Date(2026, time.December, 7, 10, 0, 0, 0, time.UTC)); !ok {
		t.Fatal("monday 14:00 Yerevan (12:00-14:30) must be restricted")
	}
}

func TestLoadRejectsInvalidUnavailable(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  model_aliases:
    free-coding:
      model: commandcode/stealth/glyph-cluster:free
      unavailable:
        windows:
          - days: [saturday]
            start: "02:00"
            end: "02:00"
`))
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "litellm.model_aliases.free-coding.unavailable") {
		t.Fatalf("error = %v, want it to name the unavailable setting path", err)
	}
}

// The documented "file only" claim, asserted: an attempt to encode the nested
// list through the environment is a startup error, not a silently ignored value.
func TestLoadRejectsEnvEncodedWindows(t *testing.T) {
	cleanEnv(t)
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__MODEL", "minimax/MiniMax-M2.7")
	t.Setenv("QUOTA_ROUTER_LITELLM__MODEL_ALIASES__FAST__UNAVAILABLE__WINDOWS", "saturday 02:00-06:00")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unavailable.windows") {
		t.Fatalf("error = %v, want it to reject the environment-encoded window list", err)
	}
}

// The setting goes through the existing koanf path, so a misspelled field inside
// it is still a startup error rather than a silently ignored value.
func TestLoadRejectsUnknownUnavailableSetting(t *testing.T) {
	cleanEnv(t)
	t.Setenv(ConfigFileEnv, writeConfig(t, `
litellm:
  model_aliases:
    free-coding:
      model: commandcode/stealth/glyph-cluster:free
      unavailable:
        timezonee: UTC
        windows:
          - days: [monday]
            start: "01:00"
            end: "02:00"
`))
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "invalid keys: timezonee") {
		t.Fatalf("error = %v, want the unknown setting rejected", err)
	}
}

// A window ending at 23:59 does not cover 23:59 — end is exclusive — so it is not
// a way to say "the whole day". This is the mistake a full-day test fixture makes
// (see TestSelectHonorsUnavailableWindow): the last minute of the day stays
// eligible unless an overnight window is added.
