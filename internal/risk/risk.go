// Package risk flags changes that deserve a second look: manual changes,
// changes outside business hours and changes during a change freeze.
package risk

import (
	"fmt"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

// Window is a named period, e.g. a change freeze.
type Window struct {
	Name       string
	Start, End time.Time
}

// Hours describes business hours in a time zone.
type Hours struct {
	Days       [7]bool // indexed by time.Weekday
	Start, End int     // minutes after midnight
	Loc        *time.Location
}

// Config controls flagging. The zero value flags manual changes only.
type Config struct {
	Hours   *Hours
	Freezes []Window
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// ParseHours parses "Mon-Fri 08:00-18:00" (or "Mon,Wed 09:00-17:00") in tz.
// An empty spec disables business-hours flagging.
func ParseHours(spec, tz string) (*Hours, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "off" {
		return nil, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	days, clock, ok := strings.Cut(spec, " ")
	if !ok {
		return nil, fmt.Errorf("invalid business hours %q (want e.g. \"Mon-Fri 08:00-18:00\")", spec)
	}
	h := &Hours{Loc: loc}
	for _, part := range strings.Split(strings.ToLower(days), ",") {
		a, b, isRange := strings.Cut(part, "-")
		da, ok1 := dayNames[a[:min(3, len(a))]]
		if !ok1 {
			return nil, fmt.Errorf("invalid day %q", a)
		}
		if !isRange {
			h.Days[da] = true
			continue
		}
		db, ok2 := dayNames[b[:min(3, len(b))]]
		if !ok2 {
			return nil, fmt.Errorf("invalid day %q", b)
		}
		for d := da; ; d = (d + 1) % 7 {
			h.Days[d] = true
			if d == db {
				break
			}
		}
	}
	from, to, ok := strings.Cut(strings.TrimSpace(clock), "-")
	if !ok {
		return nil, fmt.Errorf("invalid hours %q (want HH:MM-HH:MM)", clock)
	}
	if h.Start, err = parseClock(from); err != nil {
		return nil, err
	}
	if h.End, err = parseClock(to); err != nil {
		return nil, err
	}
	return h, nil
}

// Within reports whether t falls inside business hours.
func (h *Hours) Within(t time.Time) bool {
	lt := t.In(h.Loc)
	if !h.Days[lt.Weekday()] {
		return false
	}
	m := lt.Hour()*60 + lt.Minute()
	return m >= h.Start && m < h.End
}

func parseInstant(s string, loc *time.Location, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		if endOfDay {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid date %q (want YYYY-MM-DD or RFC 3339)", s)
}

// ParseWindow parses "[name=]start/end", e.g. "xmas=2026-12-20/2027-01-03".
// Dates are whole days in tz (the end date is inclusive).
func ParseWindow(spec, tz string) (Window, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Window{}, err
	}
	name, rng, ok := strings.Cut(spec, "=")
	if !ok {
		name, rng = "change freeze", spec
	}
	a, b, ok := strings.Cut(rng, "/")
	if !ok {
		return Window{}, fmt.Errorf("invalid freeze %q (want [name=]start/end)", spec)
	}
	w := Window{Name: strings.TrimSpace(name)}
	if w.Start, err = parseInstant(strings.TrimSpace(a), loc, false); err != nil {
		return Window{}, err
	}
	if w.End, err = parseInstant(strings.TrimSpace(b), loc, true); err != nil {
		return Window{}, err
	}
	if !w.End.After(w.Start) {
		return Window{}, fmt.Errorf("freeze %q ends before it starts", spec)
	}
	return w, nil
}

// manualManagers are field managers that mean "a human ran kubectl".
func isManual(actor string) bool {
	a := strings.ToLower(actor)
	return a == "kubectl" || strings.HasPrefix(a, "kubectl-") || a == "k9s" || a == "lens" || strings.HasPrefix(a, "kubectl.exe")
}

// Annotate adds risk flags to c (signals are left alone).
func (cfg Config) Annotate(c *change.Change) {
	if c.IsSignal() {
		return
	}
	add := func(f string) {
		if !c.HasFlag(f) {
			c.Flags = append(c.Flags, f)
		}
	}
	if isManual(c.Actor) {
		add(change.FlagManual)
	}
	if cfg.Hours != nil && !cfg.Hours.Within(c.Time) {
		add(change.FlagOffHours)
	}
	for _, w := range cfg.Freezes {
		if !c.Time.Before(w.Start) && c.Time.Before(w.End) {
			add(change.FlagFreeze)
			if c.Labels == nil {
				c.Labels = map[string]string{}
			}
			c.Labels["freeze"] = w.Name
			break
		}
	}
}
