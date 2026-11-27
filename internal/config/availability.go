package config

import (
	"fmt"
	"strings"
	"time"

	// The IANA database is embedded so a distroless/alpine runtime without the
	// tzdata package still resolves the zones the catalogue names.
	_ "time/tzdata"
)

// Unavailable is the recurring local-time schedule during which routing must not
// select a model. It is part of the catalogue entry, not a second model
// registry, and it has no stored state: availability is a pure function of the
// configuration and the instant a decision is made.
type Unavailable struct {
	// Timezone is the IANA zone the windows are written in. Required when
	// Windows is not empty; the schedule must not depend on the host's zone.
	Timezone string `koanf:"timezone"`
	// Windows are recurring weekly intervals. An empty list means no
	// time-based restriction.
	Windows []UnavailableWindow `koanf:"windows"`
}

// UnavailableWindow is one recurring weekly interval in local wall-clock time.
// Days name the weekday the window starts on, Start is inclusive and End is
// exclusive. Start after End is an overnight window that runs into the
// following day, so [saturday 23:00, 02:00] covers Saturday night up to Sunday
// 02:00. Equal Start and End is rejected: it names an empty or endless window,
// never a useful one.
type UnavailableWindow struct {
	Days  []string `koanf:"days"`
	Start string   `koanf:"start"`
	End   string   `koanf:"end"`
}

// WindowAt returns the configured window containing instant now, if any. now is
// explicit so the caller owns the clock and a decision can be replayed; policy
// code never reads the wall clock itself.
//
// A zone or window that fails to parse is treated as absent: Validate rejects
// both at startup, so an already-loaded Config can only reach that branch when
// it was built by hand.
func (u *Unavailable) WindowAt(now time.Time) (UnavailableWindow, bool) {
	if u == nil || len(u.Windows) == 0 {
		return UnavailableWindow{}, false
	}
	loc, err := time.LoadLocation(u.Timezone)
	if err != nil {
		return UnavailableWindow{}, false
	}
	local := now.In(loc)
	for _, w := range u.Windows {
		if w.contains(local) {
			return w, true
		}
	}
	return UnavailableWindow{}, false
}

// String renders the interval for a decision reason. It is configuration data
// an operator needs to explain the skip, and it is stable for identical config.
func (w UnavailableWindow) String() string {
	return strings.Join(w.Days, ",") + " " + w.Start + "-" + w.End
}

func (w UnavailableWindow) contains(t time.Time) bool {
	start, okStart := clock(w.Start)
	end, okEnd := clock(w.End)
	if !okStart || !okEnd || start == end {
		return false
	}
	minute := t.Hour()*60 + t.Minute()
	if start < end {
		return minute >= start && minute < end && w.startsOn(t.Weekday())
	}
	// Overnight: the part after midnight belongs to the day the window started.
	return (minute >= start && w.startsOn(t.Weekday())) ||
		(minute < end && w.startsOn(time.Weekday((int(t.Weekday())+6)%7)))
}

func (w UnavailableWindow) startsOn(day time.Weekday) bool {
	for _, d := range w.Days {
		if strings.EqualFold(strings.TrimSpace(d), day.String()) {
			return true
		}
	}
	return false
}

// validate reports every broken unavailable setting under where. Messages name
// the setting path and never echo the configured value.
func (u *Unavailable) validate(where string) []error {
	if u == nil || len(u.Windows) == 0 {
		return nil
	}
	var errs []error
	if u.Timezone == "" {
		errs = append(errs, fmt.Errorf("%s.timezone must name an IANA time zone when windows are configured", where))
	} else if _, err := time.LoadLocation(u.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("%s.timezone must name an IANA time zone", where))
	}
	for i, w := range u.Windows {
		wWhere := fmt.Sprintf("%s.windows[%d]", where, i)
		if len(w.Days) == 0 {
			errs = append(errs, fmt.Errorf("%s.days must name at least one weekday (monday..sunday)", wWhere))
		}
		for _, d := range w.Days {
			if !validWeekday(d) {
				errs = append(errs, fmt.Errorf("%s.days must name weekdays (monday..sunday)", wWhere))
				break
			}
		}
		start, okStart := clock(w.Start)
		end, okEnd := clock(w.End)
		if !okStart {
			errs = append(errs, fmt.Errorf("%s.start must be a 24-hour HH:MM time", wWhere))
		}
		if !okEnd {
			errs = append(errs, fmt.Errorf("%s.end must be a 24-hour HH:MM time", wWhere))
		}
		if okStart && okEnd && start == end {
			errs = append(errs, fmt.Errorf("%s.start and end must differ", wWhere))
		}
	}
	return errs
}

// clock parses a local wall-clock HH:MM into minutes since midnight.
func clock(v string) (int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func validWeekday(name string) bool {
	for day := time.Sunday; day <= time.Saturday; day++ {
		if strings.EqualFold(strings.TrimSpace(name), day.String()) {
			return true
		}
	}
	return false
}
