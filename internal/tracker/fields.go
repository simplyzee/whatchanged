package tracker

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

// fobj is a generic fingerprint: path -> hash, plus printable values for
// short scalars and arbitrary state used for signals.
type fobj struct {
	Keys  map[string]string
	Show  map[string]string
	State map[string]string
}

func newFobj() fobj {
	return fobj{Keys: map[string]string{}, Show: map[string]string{}, State: map[string]string{}}
}

// put records a value at path.
func (f fobj) put(path string, v any) {
	b, _ := json.Marshal(v)
	f.Keys[path] = hashBytes(b)
	switch t := v.(type) {
	case string:
		if len(t) <= 80 {
			f.Show[path] = t
		}
	case float64, bool, int, int64:
		f.Show[path] = fmt.Sprint(t)
	case nil:
		f.Show[path] = "unset"
	}
}

// fields records each top-level key of m under prefix, except skip.
func (f fobj) fields(prefix string, m map[string]any, skip ...string) {
outer:
	for k, v := range m {
		for _, s := range skip {
			if k == s {
				continue outer
			}
		}
		f.put(prefix+k, v)
	}
}

// named records list items keyed by a name field (or their index).
func (f fobj) named(prefix string, list any, nameField string, transform func(map[string]any) map[string]any) {
	items, _ := list.([]any)
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			f.put(fmt.Sprintf("%s[%d]", prefix, i), it)
			continue
		}
		name, _ := m[nameField].(string)
		if name == "" {
			name = fmt.Sprint(i)
		}
		if transform != nil {
			m = transform(cloneMap(m))
		}
		f.put(prefix+" "+name, m)
	}
}

// annotations records meaningful annotations (kubectl bookkeeping is ignored).
func (f fobj) annotations(meta map[string]any) {
	ann, _ := meta["annotations"].(map[string]any)
	for k, v := range ann {
		if strings.HasPrefix(k, "kubectl.kubernetes.io/") || strings.HasPrefix(k, "deployment.kubernetes.io/") ||
			strings.HasPrefix(k, "argocd.argoproj.io/tracking") || k == "meta.helm.sh/release-name" || k == "meta.helm.sh/release-namespace" {
			continue
		}
		f.put("annotation "+k, v)
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mget(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func mmap(m map[string]any, path ...string) map[string]any {
	v, _ := mget(m, path...).(map[string]any)
	if v == nil {
		v = map[string]any{}
	}
	return v
}

func mstr(m map[string]any, path ...string) string {
	s, _ := mget(m, path...).(string)
	return s
}

// condition returns (status, message) of a status condition.
func condition(obj map[string]any, typ string) (string, string) {
	conds, _ := mget(obj, "status", "conditions").([]any)
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		if cm["type"] == typ {
			s, _ := cm["status"].(string)
			msg, _ := cm["message"].(string)
			return s, msg
		}
	}
	return "", ""
}

// FieldSpec describes a tracker built on generic field fingerprints.
type FieldSpec struct {
	Name, Kind, Source string
	Optional           bool
	Paths              []string // alternatives, tried in order
	Fingerprint        func(obj map[string]any) fobj
	Action             string        // default "config"
	Impact             change.Impact // default medium
	// ImpactFor can raise or lower the impact based on what changed.
	ImpactFor func(old, cur fobj, changed, added, removed []string) change.Impact
	// Describe overrides the generated summary.
	Describe func(old, cur fobj, summary string) string
	// CreateImpact/DeleteImpact: empty means "don't record".
	CreateImpact, DeleteImpact change.Impact
	CreateSummary              func(cur fobj) string
	// Signals emits extra records from state transitions.
	Signals func(e Emitter, old, cur fobj)
}

func decodeFields(fp func(map[string]any) fobj) func(json.RawMessage) (Meta, fobj, error) {
	return func(raw json.RawMessage) (Meta, fobj, error) {
		var meta struct {
			Metadata Meta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return Meta{}, fobj{}, err
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return Meta{}, fobj{}, err
		}
		return meta.Metadata, fp(obj), nil
	}
}

func label(path string) string { return strings.TrimPrefix(path, "spec.") }

// describeFields builds "minReplicas 2 → 3; changed metrics; added host x".
func describeFields(old, cur fobj, changed, added, removed []string) string {
	var parts []string
	for _, k := range changed {
		o, ok1 := old.Show[k]
		c, ok2 := cur.Show[k]
		if ok1 && ok2 {
			parts = append(parts, fmt.Sprintf("%s %s → %s", label(k), o, c))
		} else {
			parts = append(parts, "changed "+label(k))
		}
	}
	for _, k := range added {
		if v, ok := cur.Show[k]; ok {
			parts = append(parts, fmt.Sprintf("set %s %s", label(k), v))
		} else {
			parts = append(parts, "added "+label(k))
		}
	}
	for _, k := range removed {
		parts = append(parts, "removed "+label(k))
	}
	if len(parts) > 5 {
		parts = append(parts[:5], fmt.Sprintf("+%d more", len(parts)-5))
	}
	return strings.Join(parts, "; ")
}

func fieldTracker(fs FieldSpec, env Env) Tracker {
	if fs.Action == "" {
		fs.Action = change.ActionConfig
	}
	if fs.Impact == "" {
		fs.Impact = change.ImpactMedium
	}
	if fs.Source == "" {
		fs.Source = "kubernetes"
	}
	res := make([]kube.Resource, len(fs.Paths))
	for i, p := range fs.Paths {
		res[i] = kube.Resource{Path: p}
	}
	spec := Spec[fobj]{
		Name:      fs.Name,
		Kind:      fs.Kind,
		Source:    fs.Source,
		Optional:  fs.Optional,
		Resources: res,
		Decode:    decodeFields(fs.Fingerprint),
		Diffable:  true,
		Updated: func(e Emitter, _ Meta, old, cur fobj) {
			changed, added, removed := diffKeys(old.Keys, cur.Keys)
			if len(changed)+len(added)+len(removed) > 0 {
				impact := fs.Impact
				if fs.ImpactFor != nil {
					impact = fs.ImpactFor(old, cur, changed, added, removed)
				}
				summary := describeFields(old, cur, changed, added, removed)
				if fs.Describe != nil {
					summary = fs.Describe(old, cur, summary)
				}
				e.Emit(fs.Action, impact, summary)
			}
			if fs.Signals != nil {
				fs.Signals(e, old, cur)
			}
		},
	}
	if fs.CreateImpact != "" {
		spec.Added = func(e Emitter, m Meta, cur fobj) {
			s := "created"
			if fs.CreateSummary != nil {
				s = fs.CreateSummary(cur)
			}
			e.Emit(change.ActionCreate, fs.CreateImpact, s, At(m.CreationTimestamp))
		}
	}
	if fs.DeleteImpact != "" {
		spec.Deleted = func(e Emitter, _ Meta, _ fobj) {
			e.Emit(change.ActionDelete, fs.DeleteImpact, "deleted")
		}
	}
	return New(spec, env)
}

func anyMatch(keys []string, pred func(string) bool) bool {
	for _, k := range keys {
		if pred(k) {
			return true
		}
	}
	return false
}

func hostsOf(f fobj) string {
	var hosts []string
	for k := range f.Keys {
		if h, ok := strings.CutPrefix(k, "host "); ok {
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return limitList(hosts, 3)
}
