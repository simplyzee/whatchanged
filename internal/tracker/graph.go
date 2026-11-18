package tracker

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/simplyzee/whatchanged/internal/kube"
)

// Graph links objects so a symptom can be traced back to a change:
// Pod → ReplicaSet → Deployment, Pod → Job → CronJob, and workloads to the
// ConfigMaps and Secrets they use. It is fed by metadata-only watches and by
// the workload trackers.
type Graph struct {
	now func() time.Time

	mu     sync.RWMutex
	owners map[string]ownerEntry          // "ns/Kind/name" -> owner
	users  map[string]map[string]struct{} // "ns/Kind/name" (ConfigMap/Secret) -> {"Kind/name" workloads}
	refs   map[string][]string            // "ns/Kind/name" (workload) -> ["Kind/name"] it uses
}

type ownerEntry struct {
	kind, name string
	deletedAt  time.Time // kept for a while so late events still resolve
}

// NewGraph builds an empty graph.
func NewGraph(now func() time.Time) *Graph {
	if now == nil {
		now = time.Now
	}
	return &Graph{now: now, owners: map[string]ownerEntry{}, users: map[string]map[string]struct{}{}, refs: map[string][]string{}}
}

func gkey(ns, kind, name string) string { return ns + "/" + kind + "/" + name }

// SetOwner records the controlling owner of an object.
func (g *Graph) SetOwner(ns, kind, name, ownerKind, ownerName string) {
	if g == nil || ownerKind == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.owners[gkey(ns, kind, name)] = ownerEntry{kind: ownerKind, name: ownerName}
}

// Forget marks an object as deleted; it still resolves for an hour.
func (g *Graph) Forget(ns, kind, name string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k := gkey(ns, kind, name)
	if e, ok := g.owners[k]; ok {
		e.deletedAt = g.now()
		g.owners[k] = e
	}
	// opportunistic cleanup
	if len(g.owners)%512 == 0 {
		cut := g.now().Add(-time.Hour)
		for k, e := range g.owners {
			if !e.deletedAt.IsZero() && e.deletedAt.Before(cut) {
				delete(g.owners, k)
			}
		}
	}
}

// Owner walks up the owner chain and returns the top-level controller as
// "Kind/name" (e.g. "Deployment/api"), or "" if unknown.
func (g *Graph) Owner(ns, kind, name string) string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	found := ""
	for i := 0; i < 4; i++ {
		e, ok := g.owners[gkey(ns, kind, name)]
		if !ok {
			break
		}
		kind, name = e.kind, e.name
		found = kind + "/" + name
	}
	return found
}

// SetRefs records which ConfigMaps/Secrets a workload uses ("ConfigMap/x").
func (g *Graph) SetRefs(ns, workload string, refs []string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	wk := ns + "/" + workload
	for _, r := range g.refs[wk] {
		if set := g.users[ns+"/"+r]; set != nil {
			delete(set, workload)
		}
	}
	if len(refs) == 0 {
		delete(g.refs, wk)
		return
	}
	g.refs[wk] = refs
	for _, r := range refs {
		k := ns + "/" + r
		if g.users[k] == nil {
			g.users[k] = map[string]struct{}{}
		}
		g.users[k][workload] = struct{}{}
	}
}

// Users returns the workloads ("Kind/name") that use a ConfigMap or Secret.
func (g *Graph) Users(ns, kind, name string) []string {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	set := g.users[gkey(ns, kind, name)]
	out := make([]string, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// Related returns the workloads a change to (kind, name) can affect,
// including the object itself when it is a workload.
func (g *Graph) Related(ns, kind, name string) []string {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "Rollout":
		return []string{kind + "/" + name}
	case "ConfigMap", "Secret":
		return g.Users(ns, kind, name)
	}
	return nil
}

// --- owner index trackers ------------------------------------------------------

type ownerRef struct{ Kind, Name string }

func decodeOwner(raw json.RawMessage) (Meta, ownerRef, error) {
	var obj struct {
		Metadata struct {
			Meta
			OwnerReferences []struct {
				Kind       string `json:"kind"`
				Name       string `json:"name"`
				Controller *bool  `json:"controller"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, ownerRef{}, err
	}
	var o ownerRef
	for _, r := range obj.Metadata.OwnerReferences {
		if r.Controller != nil && *r.Controller {
			o = ownerRef{r.Kind, r.Name}
			break
		}
		if o.Kind == "" {
			o = ownerRef{r.Kind, r.Name}
		}
	}
	m := obj.Metadata.Meta
	m.ManagedFields = nil // never needed here; don't keep it around
	return m, o, nil
}

func ownerIndexSpec(name, kind, path string) Spec[ownerRef] {
	set := func(e Emitter, m Meta, o ownerRef) {
		e.Graph().SetOwner(m.Namespace, kind, m.Name, o.Kind, o.Name)
	}
	return Spec[ownerRef]{
		Name:      name,
		Kind:      kind,
		Source:    "kubernetes",
		Resources: []kube.Resource{{Path: path, MetadataOnly: true}},
		Decode:    decodeOwner,
		Backfill:  set,
		Added:     set,
		Updated:   func(e Emitter, m Meta, _, cur ownerRef) { set(e, m, cur) },
		Deleted:   func(e Emitter, m Meta, _ ownerRef) { e.Graph().Forget(m.Namespace, kind, m.Name) },
	}
}

// OwnerIndex watches Pod, ReplicaSet and Job metadata so Warning events can
// be attributed to the Deployment, StatefulSet or CronJob that owns them.
// It produces no changes itself.
func OwnerIndex(env Env) Tracker {
	return multi{name: "owner-index", parts: []Tracker{
		New(ownerIndexSpec("owner-index-pods", "Pod", "/api/v1/pods"), env),
		New(ownerIndexSpec("owner-index-replicasets", "ReplicaSet", "/apis/apps/v1/replicasets"), env),
		New(ownerIndexSpec("owner-index-jobs", "Job", "/apis/batch/v1/jobs"), env),
	}}
}

// multi groups trackers under one name; the runner expands it.
type multi struct {
	name  string
	parts []Tracker
}

func (m multi) Name() string                    { return m.name }
func (m multi) Resources() []kube.Resource      { return nil }
func (m multi) Optional() bool                  { return false }
func (m multi) Replace([]json.RawMessage, bool) {}
func (m multi) Apply(kube.Event)                {}
func (m multi) Parts() []Tracker                { return m.parts }

// expand flattens grouped trackers.
func expand(ts []Tracker) []Tracker {
	var out []Tracker
	for _, t := range ts {
		if g, ok := t.(interface{ Parts() []Tracker }); ok {
			out = append(out, expand(g.Parts())...)
			continue
		}
		out = append(out, t)
	}
	return out
}

// workloadRefs extracts the ConfigMaps and Secrets a pod template uses.
func workloadRefs(podSpec map[string]any) []string {
	set := map[string]struct{}{}
	add := func(kind string, v any) {
		if n, ok := v.(string); ok && n != "" {
			set[kind+"/"+n] = struct{}{}
		}
	}
	asMap := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	asList := func(v any) []any { l, _ := v.([]any); return l }
	for _, vol := range asList(podSpec["volumes"]) {
		vm := asMap(vol)
		add("ConfigMap", asMap(vm["configMap"])["name"])
		add("Secret", asMap(vm["secret"])["secretName"])
		for _, src := range asList(asMap(vm["projected"])["sources"]) {
			sm := asMap(src)
			add("ConfigMap", asMap(sm["configMap"])["name"])
			add("Secret", asMap(sm["secret"])["name"])
		}
	}
	for _, field := range []string{"containers", "initContainers"} {
		for _, c := range asList(podSpec[field]) {
			cm := asMap(c)
			for _, ef := range asList(cm["envFrom"]) {
				em := asMap(ef)
				add("ConfigMap", asMap(em["configMapRef"])["name"])
				add("Secret", asMap(em["secretRef"])["name"])
			}
			for _, ev := range asList(cm["env"]) {
				vf := asMap(asMap(ev)["valueFrom"])
				add("ConfigMap", asMap(vf["configMapKeyRef"])["name"])
				add("Secret", asMap(vf["secretKeyRef"])["name"])
			}
		}
	}
	for _, ps := range asList(podSpec["imagePullSecrets"]) {
		add("Secret", asMap(ps)["name"])
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
