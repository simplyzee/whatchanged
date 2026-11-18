package suspects

import (
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

func TestRank(t *testing.T) {
	at := time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)
	c := func(id string, minsAgo int, ns string, impact change.Impact, action string) change.Change {
		return change.Change{ID: id, Name: id, Kind: "Deployment", Time: at.Add(-time.Duration(minsAgo) * time.Minute),
			Namespace: ns, Impact: impact, Action: action, Summary: action}
	}
	warn := func(id string, minsAgo int, ns, reason string) change.Change {
		w := c(id, minsAgo, ns, change.ImpactLow, change.ActionWarning)
		w.Summary = reason + ": something bad"
		return w
	}
	records := []change.Change{
		c("api-image", 12, "payments", change.ImpactHigh, change.ActionImage),
		c("web-image", 8, "web", change.ImpactHigh, change.ActionImage),
		c("api-scale", 3, "payments", change.ImpactLow, change.ActionScale),
		c("node", 20, "", change.ImpactMedium, change.ActionCordon),
		c("too-old", 90, "payments", change.ImpactHigh, change.ActionImage),
		c("after", -5, "payments", change.ImpactHigh, change.ActionImage),
		warn("w1", 10, "payments", "BackOff"),
		warn("w2", 9, "payments", "BackOff"),
		warn("w3", 2, "payments", "OOMKilling"),
		warn("w4", 7, "web", "Unhealthy"),
	}
	got := Rank(records, Query{At: at, Window: time.Hour, Namespaces: []string{"payments"}})
	if len(got) != 4 {
		t.Fatalf("want 4 suspects (no signals, nothing outside window), got %d: %+v", len(got), got)
	}
	if got[0].ID != "api-image" {
		t.Errorf("top suspect = %s, want api-image; ranking: %v", got[0].ID, ids(got))
	}
	found := false
	for _, r := range got[0].Reasons {
		if strings.HasPrefix(r, "followed by 3 warning(s): BackOff, OOMKilling") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning correlation reason, got %v", got[0].Reasons)
	}
	for _, s := range got {
		if s.Score < 0 || s.Score > 100 {
			t.Errorf("score out of range: %d", s.Score)
		}
	}
	// Without a namespace hint, the more recent high-impact change wins.
	got = Rank(records, Query{At: at, Window: time.Hour, Limit: 2})
	if len(got) != 2 || got[0].ID != "web-image" && got[0].ID != "api-image" {
		t.Errorf("unexpected ranking %v", ids(got))
	}
}

func ids(s []Suspect) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.ID)
	}
	return out
}

type fakeRel struct{}

func (fakeRel) Owner(ns, kind, name string) string {
	if kind == "Pod" && strings.HasPrefix(name, "api-") {
		return "Deployment/api"
	}
	return ""
}

func (fakeRel) Related(ns, kind, name string) []string {
	switch kind {
	case "Deployment":
		return []string{kind + "/" + name}
	case "ConfigMap":
		if name == "api-config" {
			return []string{"Deployment/api"}
		}
	}
	return nil
}

func TestOwnerCorrelationAndFlags(t *testing.T) {
	at := time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)
	mk := func(id, kind, name string, minsAgo int, impact change.Impact, action string) change.Change {
		return change.Change{ID: id, Kind: kind, Name: name, Namespace: "payments", Time: at.Add(-time.Duration(minsAgo) * time.Minute),
			Impact: impact, Action: action, Summary: action}
	}
	cm := mk("cm", "ConfigMap", "api-config", 10, change.ImpactMedium, "config")
	other := mk("web", "Deployment", "web", 9, change.ImpactHigh, "image") // more impactful, but its pods are fine
	flagged := mk("flagged", "Deployment", "worker", 30, change.ImpactMedium, "config")
	flagged.Flags = []string{change.FlagManual, change.FlagOffHours}
	flagged.Actor = "kubectl-edit"
	w := func(id string, minsAgo int, pod string) change.Change {
		c := mk(id, "Pod", pod, minsAgo, change.ImpactLow, change.ActionWarning)
		c.Summary = "BackOff: crash"
		return c
	}
	records := []change.Change{cm, other, flagged, w("w1", 8, "api-1"), w("w2", 7, "api-2"), w("w3", 6, "api-3")}
	got := Rank(records, Query{At: at, Window: time.Hour, Namespaces: []string{"payments"}, Relations: fakeRel{}})
	if got[0].ID != "cm" {
		t.Fatalf("ConfigMap used by the failing workload should rank first, got %v", ids(got))
	}
	if !strings.Contains(strings.Join(got[0].Reasons, "|"), "pods of Deployment/api (which use it) then failed: 3 warning(s): BackOff") {
		t.Errorf("reasons: %v", got[0].Reasons)
	}
	for _, s := range got {
		if s.ID == "flagged" {
			r := strings.Join(s.Reasons, "|")
			if !strings.Contains(r, "manual change (kubectl-edit)") || !strings.Contains(r, "outside business hours") {
				t.Errorf("flag reasons: %v", s.Reasons)
			}
		}
	}
}
