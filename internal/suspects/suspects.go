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
}

// Query describes the problem being investigated.
type Query struct {
	At         time.Time     // when the problem started (default: now)
	Window     time.Duration // how far back to look
	Namespaces []string      // where the problem is (optional)
	Limit      int
}

// warningLink is how long after a change a Warning event still counts as
// "probably caused by it".
const warningLink = 15 * time.Minute

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

		// Symptoms that appeared shortly after the change, nearby.
		reasons := map[string]int{}
		n := 0
		for _, w := range signals {
			if w.Time.Before(c.Time) || w.Time.Sub(c.Time) > warningLink || w.Time.After(q.At.Add(warningLink)) {
				continue
			}
			if c.Namespace != "" && w.Namespace != c.Namespace {
				continue
			}
			n++
			reason, _, _ := strings.Cut(w.Summary, ":")
			reasons[reason]++
		}
		if n > 0 {
			score *= 1 + math.Min(0.6, 0.12*float64(n))
			s.Reasons = append(s.Reasons, fmt.Sprintf("followed by %d warning(s): %s", n, topReasons(reasons, 3)))
		}

		s.Score = int(math.Round(math.Min(score, 1) * 100))
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
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
