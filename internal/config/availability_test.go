package config

import (
	"strings"
	"testing"
	"time"
)

func wantWindow(t *testing.T, u *Unavailable, now time.Time) (UnavailableWindow, bool) {
	t.Helper()
	return u.WindowAt(now)
}

// at builds an instant from a local wall-clock time in a named zone, so a test
// states the local time it means instead of computing UTC offsets.
func at(t *testing.T, zone string, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", zone, err)
	}
	// 2026: the year the schedule examples are dated, and one where Yerevan has
	// no DST transition to move the offsets under the test.
	return time.Date(2026, month, day, hour, minute, 0, 0, loc)
}

func TestWindowAtWeekdayBoundaries(t *testing.T) {
	yerevan := "Asia/Yerevan"
	u := &Unavailable{
		Timezone: yerevan,
		Windows: []UnavailableWindow{
			{Days: []string{"saturday", "sunday"}, Start: "02:00", End: "06:00"},
			{Days: []string{"monday"}, Start: "12:00", End: "14:30"},
		},
	}
	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		// 2026-11-27 is a Friday, 2026-11-28 a Saturday.
		{"friday just before saturday window", at(t, yerevan, time.November, 27, 23, 59), false},
		{"saturday start inclusive", at(t, yerevan, time.November, 28, 2, 0), true},
		{"saturday inside", at(t, yerevan, time.November, 28, 4, 0), true},
		{"saturday end exclusive", at(t, yerevan, time.November, 28, 6, 0), false},
		{"sunday inside the same weekly window", at(t, yerevan, time.November, 29, 3, 0), true},
		{"monday window inside", at(t, yerevan, time.November, 30, 12, 0), true},
		{"monday window end exclusive", at(t, yerevan, time.November, 30, 14, 30), false},
		{"monday after window", at(t, yerevan, time.November, 30, 15, 0), false},
		// Same wall clock, a weekday no window names.
		{"tuesday 02:00 is outside", at(t, yerevan, time.December, 1, 2, 30), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got := wantWindow(t, u, tt.now)
			if got != tt.want {
				t.Fatalf("WindowAt(%s) = %v, want %v", tt.now.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestWindowAtOvernight(t *testing.T) {
	yerevan := "Asia/Yerevan"
	u := &Unavailable{
		Timezone: yerevan,
		// Saturday night into Sunday morning.
		Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "23:00", End: "02:00"}},
	}
	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"saturday before the start", at(t, yerevan, time.November, 28, 22, 59), false},
		{"saturday start inclusive", at(t, yerevan, time.November, 28, 23, 0), true},
		{"saturday late", at(t, yerevan, time.November, 28, 23, 59), true},
		{"sunday after midnight belongs to saturday", at(t, yerevan, time.November, 29, 1, 0), true},
		{"sunday end exclusive", at(t, yerevan, time.November, 29, 2, 0), false},
		{"sunday late does not extend the window", at(t, yerevan, time.November, 29, 23, 30), false},
		{"monday after midnight does not belong to saturday", at(t, yerevan, time.November, 30, 1, 0), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got := wantWindow(t, u, tt.now)
			if got != tt.want {
				t.Fatalf("WindowAt(%s) = %v, want %v", tt.now.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestWindowAtUsesConfiguredZone(t *testing.T) {
	// 2026-11-28T08:00Z is 12:00 in Yerevan (UTC+4) and 08:00 in UTC, so a noon
	// window matches only when the configured zone is honored.
	instant := time.Date(2026, time.November, 28, 8, 0, 0, 0, time.UTC)
	u := &Unavailable{
		Timezone: "Asia/Yerevan",
		Windows:  []UnavailableWindow{{Days: []string{"saturday"}, Start: "12:00", End: "13:00"}},
	}
	if _, ok := u.WindowAt(instant); !ok {
		t.Fatalf("12:00 Yerevan must be inside the window at %s", instant.Format(time.RFC3339))
	}
	u.Timezone = "UTC"
	if _, ok := u.WindowAt(instant); ok {
		t.Fatal("08:00 UTC must be outside a noon window; the zone was ignored")
	}
}

func TestWindowAtAcrossDSTTransition(t *testing.T) {
	// America/New_York springs forward at 2026-03-08 02:00 local (07:00Z). A
	// window that spans the skipped hour is evaluated in wall-clock time, so the
	// local hour it names still decides membership on both sides of the change.
	zone := "America/New_York"
	u := &Unavailable{
		Timezone: zone,
		Windows:  []UnavailableWindow{{Days: []string{"sunday"}, Start: "01:00", End: "04:00"}},
	}
	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		// EST (UTC-5) before the transition: 06:30Z is 01:30 local.
		{"01:30 EST", time.Date(2026, time.March, 8, 6, 30, 0, 0, time.UTC), true},
		// EDT (UTC-4) after it: 01:59 is skipped, 03:00 local is 07:00Z.
		{"03:00 EDT still inside", time.Date(2026, time.March, 8, 7, 0, 0, 0, time.UTC), true},
		{"04:00 EDT end exclusive", time.Date(2026, time.March, 8, 8, 0, 0, 0, time.UTC), false},
		// One week later the same wall clock is an hour earlier in UTC.
		{"03:00 EDT a week later", time.Date(2026, time.March, 15, 7, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got := u.WindowAt(tt.now)
			if got != tt.want {
				t.Fatalf("WindowAt(%s) = %v, want %v", tt.now.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestWindowAtAbsentConfigurationNeverRestricts(t *testing.T) {
	now := time.Date(2026, time.November, 28, 3, 0, 0, 0, time.UTC)
	for _, u := range []*Unavailable{nil, {}, {Timezone: "Asia/Yerevan"}} {
		if _, ok := u.WindowAt(now); ok {
			t.Fatalf("%+v must not restrict anything", u)
		}
	}
}

func TestUnavailableValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		u    *Unavailable
		want string
	}{
		{"valid", &Unavailable{Timezone: "Asia/Yerevan", Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "02:00", End: "06:00"}}}, ""},
		{"valid overnight", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "23:00", End: "02:00"}}}, ""},
		{"no windows", &Unavailable{Timezone: "Nowhere/Invalid"}, ""},
		{"missing timezone", &Unavailable{Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "02:00", End: "06:00"}}}, "timezone must name an IANA time zone"},
		{"unknown timezone", &Unavailable{Timezone: "Nowhere/Invalid", Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "02:00", End: "06:00"}}}, "timezone must name an IANA time zone"},
		// Go resolves Local to the host's zone, which would make the same config
		// cover different hours on different machines.
		{"Local is not IANA", &Unavailable{Timezone: "Local", Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "02:00", End: "06:00"}}}, "timezone must name an IANA time zone"},
		{"UTC is IANA", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "02:00", End: "06:00"}}}, ""},
		{"real IANA name", &Unavailable{Timezone: "Asia/Yerevan", Windows: []UnavailableWindow{{Days: []string{"saturday"}, Start: "02:00", End: "06:00"}}}, ""},
		{"unknown weekday", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Days: []string{"funday"}, Start: "02:00", End: "06:00"}}}, "days must name weekdays"},
		{"no weekday", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Start: "02:00", End: "06:00"}}}, "days must name at least one weekday"},
		{"malformed start", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Days: []string{"monday"}, Start: "2am", End: "06:00"}}}, "start must be a 24-hour HH:MM time"},
		{"out of range end", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Days: []string{"monday"}, Start: "02:00", End: "24:00"}}}, "end must be a 24-hour HH:MM time"},
		{"equal boundaries", &Unavailable{Timezone: "UTC", Windows: []UnavailableWindow{{Days: []string{"monday"}, Start: "02:00", End: "02:00"}}}, "start and end must differ"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateUnavailableConfig(tt.u)
			if tt.want == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			var got []string
			for _, err := range errs {
				got = append(got, err.Error())
			}
			if !strings.Contains(strings.Join(got, "; "), tt.want) {
				t.Fatalf("errors = %v, want one naming %q", got, tt.want)
			}
			for _, msg := range got {
				if !strings.HasPrefix(msg, "litellm.model_aliases.free-coding.unavailable") {
					t.Fatalf("%q must name the setting path", msg)
				}
			}
		})
	}
}

func validateUnavailableConfig(u *Unavailable) []error {
	c := Default()
	c.LiteLLM.ModelAliases = map[string]Model{
		"free-coding": {Model: "commandcode/stealth/glyph-cluster:free", Unavailable: u},
	}
	err := c.validate()
	if err == nil {
		return nil
	}
	joined, ok := unwrapJoinedConfigError(err)
	if !ok {
		return []error{err}
	}
	var out []error
	for _, e := range joined {
		if strings.Contains(e.Error(), "litellm.model_aliases.free-coding.unavailable") {
			out = append(out, e)
		}
	}
	return out
}

func unwrapJoinedConfigError(err error) ([]error, bool) {
	if err == nil {
		return nil, false
	}
	type multiUnwrapper interface{ Unwrap() []error }
	type singleUnwrapper interface{ Unwrap() error }

	var walk func(error) ([]error, bool)
	walk = func(e error) ([]error, bool) {
		if u, ok := e.(multiUnwrapper); ok {
			return u.Unwrap(), true
		}
		if u, ok := e.(singleUnwrapper); ok {
			return walk(u.Unwrap())
		}
		return nil, false
	}
	return walk(err)
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
	// Friday 16:00 in Yerevan is outside both windows: the schedule restricts
	// exactly its own intervals and nothing else.
	if _, ok := u.WindowAt(time.Date(2026, time.November, 27, 12, 0, 0, 0, time.UTC)); ok {
		t.Fatal("friday 16:00 Yerevan must not be restricted")
	}
	// And it does restrict when the local clock is inside one.
	if _, ok := u.WindowAt(time.Date(2026, time.December, 7, 10, 0, 0, 0, time.UTC)); !ok {
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
func TestWindowAtLastMinuteOfDay(t *testing.T) {
	all := []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	lastMinute := time.Date(2026, time.November, 28, 23, 59, 30, 0, time.UTC)

	dayOnly := &Unavailable{
		Timezone: "UTC",
		Windows:  []UnavailableWindow{{Days: all, Start: "00:00", End: "23:59"}},
	}
	if _, ok := dayOnly.WindowAt(lastMinute); ok {
		t.Fatal("23:59 must be outside a window ending at 23:59, because end is exclusive")
	}

	// The documented way to cover every instant: an overnight window fills the
	// gap the exclusive end leaves.
	full := &Unavailable{
		Timezone: "UTC",
		Windows: []UnavailableWindow{
			{Days: all, Start: "00:00", End: "23:59"},
			{Days: all, Start: "23:59", End: "00:00"},
		},
	}
	base := time.Date(2026, time.November, 22, 0, 0, 0, 0, time.UTC) // Sunday
	for m := 0; m < 7*24*60; m++ {
		instant := base.Add(time.Duration(m) * time.Minute)
		if _, ok := full.WindowAt(instant); !ok {
			t.Fatalf("%s is not covered by the two-window schedule", instant.Format(time.RFC3339))
		}
	}
}
