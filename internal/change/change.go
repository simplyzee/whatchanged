// Package change defines the Change record that every source produces.
package change

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Impact is a coarse estimate of how likely a change is to break things.
type Impact string

const (
	ImpactHigh   Impact = "high"
	ImpactMedium Impact = "medium"
	ImpactLow    Impact = "low"
)

// Weight returns a numeric weight used when ranking suspects.
func (i Impact) Weight() float64 {
	switch i {
	case ImpactHigh:
		return 1.0
	case ImpactMedium:
		return 0.6
	default:
		return 0.3
	}
}

// AtLeast reports whether i is at least as severe as min.
func (i Impact) AtLeast(min Impact) bool { return i.Weight() >= min.Weight() }

// ParseImpact parses an impact name, defaulting to low.
func ParseImpact(s string) (Impact, error) {
	switch Impact(strings.ToLower(s)) {
	case ImpactHigh:
		return ImpactHigh, nil
	case ImpactMedium:
		return ImpactMedium, nil
	case ImpactLow, "":
		return ImpactLow, nil
	}
	return "", fmt.Errorf("unknown impact %q (want high, medium or low)", s)
}

// Well-known actions. Sources may use others.
const (
	ActionCreate  = "create"
	ActionDelete  = "delete"
	ActionImage   = "image"
	ActionScale   = "scale"
	ActionConfig  = "config"
	ActionRestart = "restart"
	ActionRelease = "release"
	ActionSync    = "sync"
	ActionPolicy  = "policy"
	ActionCordon  = "cordon"
	ActionNode    = "node"
	ActionWarning = "warning"
	ActionDeploy  = "deploy"
)

// Change is a single thing that changed somewhere in (or around) the cluster.
type Change struct {
	ID        string            `json:"id"`
	Time      time.Time         `json:"time"`
	Source    string            `json:"source"`
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Action    string            `json:"action"`
	Impact    Impact            `json:"impact"`
	Summary   string            `json:"summary"`
	Actor     string            `json:"actor,omitempty"`
	URL       string            `json:"url,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Flags are risk markers such as "manual", "off-hours" or "freeze".
	Flags []string `json:"flags,omitempty"`
	// Diff is a redacted, line-based diff of the object ("- path: old",
	// "+ path: new"). Only populated where diffs are enabled.
	Diff []string `json:"diff,omitempty"`
}

// HasFlag reports whether the change carries flag f.
func (c Change) HasFlag(f string) bool {
	for _, x := range c.Flags {
		if x == f {
			return true
		}
	}
	return false
}

// Well-known flags.
const (
	FlagManual   = "manual"
	FlagOffHours = "off-hours"
	FlagFreeze   = "freeze"
)

// Object returns "kind/name" for display.
func (c Change) Object() string {
	if c.Kind == "" {
		return c.Name
	}
	return c.Kind + "/" + c.Name
}

// IsSignal reports whether the record is an observed symptom (e.g. a Warning
// event) rather than an actual change. Signals are never suspects.
func (c Change) IsSignal() bool { return c.Action == ActionWarning }

// MakeID builds a deterministic ID so the same change observed twice (after a
// restart, a relist or a backfill) is stored once.
func MakeID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

// Normalize fills defaults and validates a change. It is used for changes
// arriving over the ingest API.
func (c *Change) Normalize(now time.Time) error {
	c.Source = strings.TrimSpace(c.Source)
	c.Name = strings.TrimSpace(c.Name)
	c.Summary = strings.TrimSpace(c.Summary)
	if c.Source == "" {
		return fmt.Errorf("source is required")
	}
	if c.Name == "" && c.Summary == "" {
		return fmt.Errorf("name or summary is required")
	}
	if c.Name == "" {
		c.Name = c.Source
	}
	if c.Action == "" {
		c.Action = ActionDeploy
	}
	if c.Time.IsZero() {
		c.Time = now
	}
	c.Time = c.Time.UTC()
	imp, err := ParseImpact(string(c.Impact))
	if err != nil {
		return err
	}
	if c.Impact == "" {
		imp = ImpactMedium
	}
	c.Impact = imp
	if len(c.Summary) > 1024 {
		c.Summary = c.Summary[:1024]
	}
	if c.ID == "" {
		c.ID = MakeID(c.Source, c.Kind, c.Namespace, c.Name, c.Action, c.Summary, c.Time.Format(time.RFC3339Nano))
	}
	return nil
}
