package risk

import (
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

func TestAnnotate(t *testing.T) {
	hours, err := ParseHours("Mon-Fri 08:00-18:00", "Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	freeze, err := ParseWindow("xmas=2026-12-20/2027-01-03", "Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Hours: hours, Freezes: []Window{freeze}}
	cases := []struct {
		at    string
		actor string
		flags []string
	}{
		{"2026-11-17T10:00:00Z", "argocd-controller", nil},                                  // Tuesday morning
		{"2026-11-17T10:00:00Z", "kubectl-edit", []string{"manual"}},                        //
		{"2026-11-17T19:30:00Z", "helm", []string{"off-hours"}},                             // evening
		{"2026-11-21T11:00:00Z", "kubectl", []string{"manual", "off-hours"}},                // Saturday
		{"2027-01-03T12:00:00Z", "helm", []string{"off-hours", "freeze"}},                   // Sunday, last day of freeze
		{"2027-01-04T10:00:00Z", "helm", nil},                                               // freeze over
		{"2026-12-22T09:00:00Z", "kubectl-client-side-apply", []string{"manual", "freeze"}}, // Tuesday in freeze
	}
	for _, c := range cases {
		at, _ := time.Parse(time.RFC3339, c.at)
		ch := change.Change{Time: at, Actor: c.actor, Action: "image"}
		cfg.Annotate(&ch)
		if len(ch.Flags) != len(c.flags) {
			t.Errorf("%s %s: flags %v, want %v", c.at, c.actor, ch.Flags, c.flags)
			continue
		}
		for i := range c.flags {
			if ch.Flags[i] != c.flags[i] {
				t.Errorf("%s %s: flags %v, want %v", c.at, c.actor, ch.Flags, c.flags)
			}
		}
	}
	w := change.Change{Time: time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC), Action: change.ActionWarning}
	cfg.Annotate(&w)
	if len(w.Flags) != 0 {
		t.Error("signals should never be flagged")
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"Mon-Fri", "Xyz 08:00-18:00", "Mon-Fri 8-6"} {
		if _, err := ParseHours(s, "UTC"); err == nil {
			t.Errorf("ParseHours(%q) should fail", s)
		}
	}
	if h, err := ParseHours("", "UTC"); h != nil || err != nil {
		t.Error("empty hours should disable")
	}
	for _, s := range []string{"2026-12-20", "2027-01-03/2026-12-20", "a=b/c"} {
		if _, err := ParseWindow(s, "UTC"); err == nil {
			t.Errorf("ParseWindow(%q) should fail", s)
		}
	}
}
