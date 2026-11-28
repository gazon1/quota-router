package settings

import (
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
