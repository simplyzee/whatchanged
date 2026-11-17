package timeparse

import (
	"testing"
	"time"
)

func TestPoint(t *testing.T) {
	now := time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     now,
		"now":                  now,
		"30m":                  now.Add(-30 * time.Minute),
		"1d":                   now.Add(-24 * time.Hour),
		"1.5h":                 now.Add(-90 * time.Minute),
		"2026-11-17T18:30:00Z": time.Date(2026, 11, 17, 18, 30, 0, 0, time.UTC),
		"2026-11-17T18:30":     time.Date(2026, 11, 17, 18, 30, 0, 0, time.UTC),
		"2026-11-16":           time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
		"1763404200":           time.Unix(1763404200, 0).UTC(),
		"1763404200000":        time.UnixMilli(1763404200000).UTC(),
	}
	for in, want := range cases {
		got, err := Point(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("Point(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-5m", "2026-13-45", "5x"} {
		if _, err := Point(bad, now); err == nil {
			t.Errorf("Point(%q) should fail", bad)
		}
	}
}
