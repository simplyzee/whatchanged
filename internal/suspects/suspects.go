// Package suspects ranks recent changes by how likely they are to explain a
// problem that started at a given time. The scoring is deliberately simple
// and explainable: every score comes with the reasons behind it.
package suspects

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

// Suspect is a change with a score in 0–100 and the reasons for it.
type Suspect struct {
	change.Change
	Score   int      `json:"score"`
	Reasons []string `json:"reasons"`
	raw     float64
}

// Query describes the problem being investigated.
type Query struct {
	At         time.Time     // when the problem started (default: now)
	Window     time.Duration // how far back to look
	Namespaces []string      // where the problem is (optional)
	Limit      int
	// Relations links symptoms to changes (optional).
	Relations Relations
}

// Relations knows how objects relate. tracker.Graph implements it.
type Relations interface {
	// Owner returns the top-level controller ("Deployment/api") of an object.
	Owner(ns, kind, name string) string
	// Related returns the workloads a change to (kind, name) can affect.
	Related(ns, kind, name string) []string
}

// warningLink is how long after a change a Warning event still counts as
// "probably caused by it".
const warningLink = 15 * time.Minute

// eventSlack tolerates Event timestamps having one-second resolution (and
// a little clock skew) relative to when we observed the change.
const eventSlack = 5 * time.Second

// Rank scores candidate changes. records may contain signals (Warning
// events); they are used as evidence but never returned as suspects.
func Rank(records []change.Change, q Query) []Suspect {
	if q.Window <= 0 {
		q.Window = time.Hour
	}
	from := q.At.Add(-q.Window)
	halfLife := q.Window / 4
	nsSet := map[string]bool{}
	for _, n := range q.Namespaces {
		nsSet[n] = true
	}

	var signals []change.Change
	for _, c := range records {
		if c.IsSignal() {
			signals = append(signals, c)
		}
	}

	var out []Suspect
	for _, c := range records {
		if c.IsSignal() || c.Time.After(q.At) || c.Time.Before(from) {
			continue
		}
		s := Suspect{Change: c}
		score := c.Impact.Weight()
		s.Reasons = append(s.Reasons, string(c.Impact)+" impact "+c.Action)

		age := q.At.Sub(c.Time)
		recency := math.Pow(0.5, float64(age)/float64(halfLife))
		recency = math.Max(recency, 0.15) // old-but-in-window still counts a little
		score *= recency
		s.Reasons = append(s.Reasons, humanAge(age)+" before")

		if len(nsSet) > 0 {
			switch {
			case nsSet[c.Namespace]:
				s.Reasons = append(s.Reasons, "same namespace")
			case c.Namespace == "":
				score *= 0.8
				s.Reasons = append(s.Reasons, "cluster-wide")
			default:
				score *= 0.35
			}
		}

		// Symptoms that appeared shortly after the change. Symptoms on the
		// workloads this change affects are strong evidence; anything else
		// in the same namespace is weak evidence.
		related := map[string]bool{}
		if q.Relations != nil {
			for _, r := range q.Relations.Related(c.Namespace, c.Kind, c.Name) {
				related[r] = true
			}
		}
		direct, nearby := map[string]int{}, map[string]int{}
		nd, nn := 0, 0
		for _, w := range signals {
			if w.Time.Before(c.Time.Add(-eventSlack)) || w.Time.Sub(c.Time) > warningLink || w.Time.After(q.At.Add(warningLink)) {
				continue
			}
			if c.Namespace != "" && w.Namespace != c.Namespace {
				continue
			}
			reason, _, _ := strings.Cut(w.Summary, ":")
			owner := w.Labels["owner"]
			if owner == "" && q.Relations != nil {
				owner = q.Relations.Owner(w.Namespace, w.Kind, w.Name)
			}
			if owner != "" && related[owner] {
				nd++
				direct[reason]++
				continue
			}
			if owner != "" && len(related) > 0 {
				continue // a known, different workload: not evidence for this change
			}
			nn++
			nearby[reason]++
		}
		if nd > 0 {
			score *= 1 + math.Min(1.0, 0.3*float64(nd))
			who := "its pods"
			if c.Kind == "ConfigMap" || c.Kind == "Secret" {
				var ws []string
				for w := range related {
					ws = append(ws, w)
				}
				sort.Strings(ws)
				who = "pods of " + strings.Join(ws, ", ") + " (which use it)"
			}
			s.Reasons = append(s.Reasons, fmt.Sprintf("%s then failed: %d warning(s): %s", who, nd, topReasons(direct, 3)))
		}
		if nn > 0 {
			score *= 1 + math.Min(0.6, 0.12*float64(nn))
			s.Reasons = append(s.Reasons, fmt.Sprintf("followed by %d warning(s): %s", nn, topReasons(nearby, 3)))
		}

		// Risk flags.
		if c.HasFlag(change.FlagFreeze) {
			score *= 1.2
			s.Reasons = append(s.Reasons, "made during a change freeze")
		}
		if c.HasFlag(change.FlagManual) {
			score *= 1.15
			s.Reasons = append(s.Reasons, "manual change ("+c.Actor+")")
		}
		if c.HasFlag(change.FlagOffHours) {
			score *= 1.1
			s.Reasons = append(s.Reasons, "made outside business hours")
		}

		// Map the unbounded raw score onto 0–100 without saturating, so
		// strong evidence still separates suspects at the top.
		s.raw = score
		s.Score = int(math.Round(100 * score / (score + 0.4)))
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].raw != out[j].raw {
			return out[i].raw > out[j].raw
		}
		return out[i].Time.After(out[j].Time)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

func topReasons(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range m {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	var parts []string
	for i, e := range list {
		if i == n {
			break
		}
		parts = append(parts, e.k)
	}
	return strings.Join(parts, ", ")
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
