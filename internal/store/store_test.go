package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

var base = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func mk(id string, minsAgo int, ns string, impact change.Impact, action string) change.Change {
	return change.Change{ID: id, Time: base.Add(-time.Duration(minsAgo) * time.Minute), Source: "kubernetes",
		Kind: "Deployment", Namespace: ns, Name: id, Action: action, Impact: impact, Summary: "summary of " + id}
}

func TestAddQueryDedupe(t *testing.T) {
	s, err := Open(Options{Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	var hooked int
	s.OnAdd(func(change.Change) { hooked++ })
	for _, c := range []change.Change{
		mk("a", 50, "payments", change.ImpactHigh, "image"),
		mk("c", 10, "web", change.ImpactLow, "scale"),
		mk("b", 30, "payments", change.ImpactMedium, "config"), // out of order on purpose
		mk("w", 5, "payments", change.ImpactLow, change.ActionWarning),
	} {
		if ok, err := s.Add(c); !ok || err != nil {
			t.Fatalf("add %s: %v %v", c.ID, ok, err)
		}
	}
	if ok, _ := s.Add(mk("a", 50, "payments", change.ImpactHigh, "image")); ok {
		t.Error("duplicate ID should be rejected")
	}
	if hooked != 4 {
		t.Errorf("hook called %d times, want 4", hooked)
	}

	ids := func(cs []change.Change) string {
		out := ""
		for _, c := range cs {
			out += c.ID
		}
		return out
	}
	if got := ids(s.Query(Filter{})); got != "wcba" {
		t.Errorf("newest first: got %q", got)
	}
	if got := ids(s.Query(Filter{Namespaces: []string{"payments"}, NoSignals: true})); got != "ba" {
		t.Errorf("namespace filter: got %q", got)
	}
	if got := ids(s.Query(Filter{MinImpact: change.ImpactMedium})); got != "ba" {
		t.Errorf("impact filter: got %q", got)
	}
	if got := ids(s.Query(Filter{Since: base.Add(-35 * time.Minute), Until: base.Add(-8 * time.Minute)})); got != "cb" {
		t.Errorf("time window: got %q", got)
	}
	if got := ids(s.Query(Filter{Text: "OF B"})); got != "b" {
		t.Errorf("text search: got %q", got)
	}
	if got := ids(s.Query(Filter{Limit: 2})); got != "wc" {
		t.Errorf("limit: got %q", got)
	}
}

func TestPersistenceRetentionAndCorruption(t *testing.T) {
	dir := t.TempDir()
	now := base
	clock := func() time.Time { return now }
	s, err := Open(Options{Dir: dir, Retention: time.Hour, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	s.Add(mk("old", 50, "a", change.ImpactLow, "scale"))
	s.Add(mk("new", 5, "a", change.ImpactLow, "scale"))
	if ok, _ := s.Add(mk("ancient", 120, "a", change.ImpactLow, "scale")); ok {
		t.Error("change outside retention should be rejected")
	}
	s.Close()

	// Simulate a crash mid-write: append a truncated line.
	f, _ := os.OpenFile(filepath.Join(dir, fileName), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"id":"broken","time":"2026-`)
	f.Close()

	now = base.Add(20 * time.Minute) // "old" is now 70 minutes old
	s2, err := Open(Options{Dir: dir, Retention: time.Hour, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Query(Filter{})
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("after reload got %+v", got)
	}
	// The file should have been compacted (no corrupt line, no expired one).
	s2.Add(mk("newer", 1, "a", change.ImpactLow, "scale"))
	s2.Close()
	s3, _ := Open(Options{Dir: dir, Retention: time.Hour, Now: clock})
	if n := s3.Len(); n != 2 {
		t.Fatalf("reload after compaction: %d items", n)
	}

	now = base.Add(2 * time.Hour)
	n, err := s3.Prune()
	if err != nil || n != 2 || s3.Len() != 0 {
		t.Fatalf("prune: n=%d err=%v len=%d", n, err, s3.Len())
	}
	// Still writable after prune reopened the file.
	if ok, err := s3.Add(mk("after", -110, "a", change.ImpactLow, "scale")); !ok || err != nil {
		t.Fatalf("add after prune: %v %v", ok, err)
	}
	s3.Close()
}

func TestMaxItems(t *testing.T) {
	s, _ := Open(Options{MaxItems: 3, Now: func() time.Time { return base }})
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		s.Add(mk(id, 10-i, "", change.ImpactLow, "scale"))
	}
	got := s.Query(Filter{})
	if len(got) != 3 || got[0].ID != "e" || got[2].ID != "c" {
		t.Fatalf("got %+v", got)
	}
	// Evicted IDs can be stored again.
	if ok, _ := s.Add(mk("a", 0, "", change.ImpactLow, "scale")); !ok {
		t.Error("evicted id should be addable again")
	}
}

func TestGetByPrefixAndFlags(t *testing.T) {
	s, _ := Open(Options{Now: func() time.Time { return base }})
	a := mk("abcdef123456", 5, "x", change.ImpactLow, "scale")
	a.Flags = []string{change.FlagManual}
	s.Add(a)
	s.Add(mk("abcdff999999", 4, "x", change.ImpactLow, "scale"))
	if c, ok, err := s.Get("abcdef123456"); !ok || err != nil || c.ID != a.ID {
		t.Errorf("exact get: %v %v", ok, err)
	}
	if c, ok, _ := s.Get("abcdef"); !ok || c.ID != a.ID {
		t.Error("prefix get")
	}
	if _, _, err := s.Get("abcd"); err != nil {
		t.Error("short prefixes are not searched")
	}
	if _, _, err := s.Get("abcd" + "f"); err != nil {
		t.Errorf("unique 5-char prefix is too short to match, not an error: %v", err)
	}
	s.Add(mk("abcdef777777", 3, "x", change.ImpactLow, "scale"))
	if _, _, err := s.Get("abcdef"); err != ErrAmbiguous {
		t.Errorf("want ambiguous, got %v", err)
	}
	if got := s.Query(Filter{Flags: []string{"manual", "freeze"}}); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("flag filter: %+v", got)
	}
}
