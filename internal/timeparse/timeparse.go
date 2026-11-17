// Package timeparse parses the time expressions used by the API and CLI.
package timeparse

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration parses Go durations plus a "d" (day) suffix, e.g. "90m", "2h", "7d".
func Duration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// Point parses an absolute or relative point in time:
//   - "30m", "2h", "1d"  → that long before now
//   - RFC 3339           → 2026-11-17T18:30:00Z
//   - unix seconds/ms    → 1763404200 / 1763404200000
//   - "now"
func Point(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "now":
		return now, nil
	case strings.ContainsAny(s, "T-:") && len(s) >= 10:
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("invalid time %q (use RFC 3339, e.g. 2026-11-17T18:30:00Z)", s)
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	d, err := Duration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q (use e.g. 30m, 2h, 1d or RFC 3339)", s)
	}
	return now.Add(-d), nil
}
