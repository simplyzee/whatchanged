// Package tracker turns Kubernetes object state into Change records. Each
// tracker keeps a small fingerprint of every object it watches and diffs
// the old and new fingerprint when the object changes.
package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

// Meta is the subset of ObjectMeta that trackers use.
type Meta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Generation        int64             `json:"generation"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	ManagedFields     []ManagedField    `json:"managedFields"`
}

// ManagedField records which field manager last touched the object.
type ManagedField struct {
	Manager     string     `json:"manager"`
	Operation   string     `json:"operation"`
	Subresource string     `json:"subresource"`
	Time        *time.Time `json:"time"`
}

// Actor returns the field manager that most recently changed the object's
// main resource (status updates are ignored). It is a good hint at *who*
// made the change: kubectl-edit, argocd-controller, helm, kustomize-controller…
func (m Meta) Actor() string {
	var best string
	var bestT time.Time
	for _, f := range m.ManagedFields {
		if f.Subresource != "" || f.Time == nil {
			continue
		}
		if best == "" || f.Time.After(bestT) {
			best, bestT = f.Manager, *f.Time
		}
	}
	return best
}

// Sink receives produced changes.
type Sink func(change.Change)

// Tracker is what the runner needs from every resource tracker.
type Tracker interface {
	kube.Handler
	Name() string
	Resources() []kube.Resource // alternatives, tried in order on 404
	Optional() bool             // a missing CRD is not an error
}

// Spec describes how to fingerprint and diff one resource type.
type Spec[T any] struct {
	Name     string
	Kind     string
	Source   string
	Optional bool
	// Resources are tried in order until one exists (handy for CRDs that
	// changed API version).
	Resources []kube.Resource
	Decode    func(raw json.RawMessage) (Meta, T, error)
	// Added is called for objects created while we are watching.
	Added func(e Emitter, m Meta, v T)
	// Updated is called when an object's resourceVersion changes.
	Updated func(e Emitter, m Meta, old, cur T)
	// Deleted is called when an object goes away.
	Deleted func(e Emitter, m Meta, v T)
	// Backfill is called for every object on the very first list, so
	// recent history (Helm revisions, Warning events…) survives restarts.
	Backfill func(e Emitter, m Meta, v T)
}

type entry[T any] struct {
	meta Meta
	val  T
}

// Generic is a Tracker built from a Spec.
type Generic[T any] struct {
	spec  Spec[T]
	sink  Sink
	now   func() time.Time
	mu    sync.Mutex
	state map[string]entry[T]
}

// New builds a tracker from a spec.
func New[T any](spec Spec[T], sink Sink, now func() time.Time) *Generic[T] {
	if now == nil {
		now = time.Now
	}
	return &Generic[T]{spec: spec, sink: sink, now: now, state: map[string]entry[T]{}}
}

func (g *Generic[T]) Name() string               { return g.spec.Name }
func (g *Generic[T]) Resources() []kube.Resource { return g.spec.Resources }
func (g *Generic[T]) Optional() bool             { return g.spec.Optional }

func key(m Meta) string {
	if m.UID != "" {
		return m.UID
	}
	return m.Namespace + "/" + m.Name
}

func (g *Generic[T]) emitter(m Meta) Emitter {
	return Emitter{spec: specInfo{g.spec.Kind, g.spec.Source}, meta: m, sink: g.sink, now: g.now}
}

// Replace implements kube.Handler.
func (g *Generic[T]) Replace(items []json.RawMessage, initial bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := make(map[string]entry[T], len(items))
	for _, raw := range items {
		m, v, err := g.spec.Decode(raw)
		if err != nil {
			continue
		}
		k := key(m)
		next[k] = entry[T]{m, v}
		if initial {
			if g.spec.Backfill != nil {
				g.spec.Backfill(g.emitter(m), m, v)
			}
			continue
		}
		// A relist: diff against what we knew so nothing is missed.
		if old, ok := g.state[k]; ok {
			if old.meta.ResourceVersion != m.ResourceVersion && g.spec.Updated != nil {
				g.spec.Updated(g.emitter(m), m, old.val, v)
			}
		} else if g.spec.Added != nil {
			g.spec.Added(g.emitter(m), m, v)
		}
	}
	if !initial && g.spec.Deleted != nil {
		for k, old := range g.state {
			if _, ok := next[k]; !ok {
				g.spec.Deleted(g.emitter(old.meta), old.meta, old.val)
			}
		}
	}
	g.state = next
}

// Apply implements kube.Handler.
func (g *Generic[T]) Apply(ev kube.Event) {
	m, v, err := g.spec.Decode(ev.Object)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k := key(m)
	old, known := g.state[k]
	switch ev.Type {
	case "ADDED", "MODIFIED":
		g.state[k] = entry[T]{m, v}
		switch {
		case known && old.meta.ResourceVersion == m.ResourceVersion:
			// replayed event, nothing new
		case known:
			if g.spec.Updated != nil {
				g.spec.Updated(g.emitter(m), m, old.val, v)
			}
		case ev.Type == "ADDED":
			if g.spec.Added != nil {
				g.spec.Added(g.emitter(m), m, v)
			}
		}
		// MODIFIED for an object we never saw: just start tracking it.
	case "DELETED":
		delete(g.state, k)
		if g.spec.Deleted != nil {
			g.spec.Deleted(g.emitter(m), m, v)
		}
	}
}

type specInfo struct{ kind, source string }

// Emitter builds changes for one object.
type Emitter struct {
	spec specInfo
	meta Meta
	sink Sink
	now  func() time.Time
}

// Opt adjusts a change before it is emitted.
type Opt func(*change.Change)

// At sets the change time (default: now).
func At(t time.Time) Opt {
	return func(c *change.Change) {
		if !t.IsZero() {
			c.Time = t.UTC()
		}
	}
}

// WithID overrides the ID seed, for changes that must dedupe across
// restarts (e.g. "helm release X revision 7").
func WithID(parts ...string) Opt {
	return func(c *change.Change) { c.ID = change.MakeID(append([]string{c.Source, c.Kind}, parts...)...) }
}

// Object overrides the kind/namespace/name the change is about.
func Object(kind, namespace, name string) Opt {
	return func(c *change.Change) { c.Kind, c.Namespace, c.Name = kind, namespace, name }
}

// Actor overrides the actor.
func Actor(a string) Opt { return func(c *change.Change) { c.Actor = a } }

// Emit sends one change.
func (e Emitter) Emit(action string, impact change.Impact, summary string, opts ...Opt) {
	c := change.Change{
		Time:      e.now().UTC(),
		Source:    e.spec.source,
		Kind:      e.spec.kind,
		Namespace: e.meta.Namespace,
		Name:      e.meta.Name,
		Action:    action,
		Impact:    impact,
		Summary:   summary,
		Actor:     e.meta.Actor(),
	}
	// Default ID: unique per object revision and action, so a relist or a
	// replayed watch event never double-counts.
	c.ID = change.MakeID(c.Source, c.Kind, key(e.meta), e.meta.ResourceVersion, action, summary)
	for _, o := range opts {
		o(&c)
	}
	e.sink(c)
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func decodeInto[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}
