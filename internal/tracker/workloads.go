package tracker

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// workload is the fingerprint of a Deployment, StatefulSet or DaemonSet.
type workload struct {
	Generation  int64
	Replicas    *int32
	Paused      bool
	Images      map[string]string // container name -> image
	RestartedAt string
	// Fields maps a short path (e.g. "containers[api].env") to a hash of
	// its JSON, so we can say *what* in the pod template changed.
	Fields map[string]string
	// Refs are the ConfigMaps/Secrets the pod template uses.
	Refs []string
}

type rawContainer map[string]json.RawMessage

func decodeWorkload(raw json.RawMessage) (Meta, workload, error) {
	var obj struct {
		Metadata Meta                       `json:"metadata"`
		Spec     map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, workload{}, err
	}
	w := workload{Generation: obj.Metadata.Generation, Images: map[string]string{}, Fields: map[string]string{}}
	for k, v := range obj.Spec {
		switch k {
		case "replicas":
			var r int32
			if json.Unmarshal(v, &r) == nil {
				w.Replicas = &r
			}
		case "paused":
			_ = json.Unmarshal(v, &w.Paused)
		case "template":
			fingerprintTemplate(v, &w)
		case "revisionHistoryLimit", "progressDeadlineSeconds":
			// tuning knobs nobody cares about during an incident
		default:
			w.Fields["spec."+k] = hashBytes(v)
		}
	}
	return obj.Metadata, w, nil
}

func fingerprintTemplate(raw json.RawMessage, w *workload) {
	var tpl struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if json.Unmarshal(raw, &tpl) != nil {
		return
	}
	ann := map[string]string{}
	for k, v := range tpl.Metadata.Annotations {
		if k == restartedAtAnnotation {
			w.RestartedAt = v
			continue
		}
		ann[k] = v
	}
	if len(ann) > 0 {
		b, _ := json.Marshal(ann) // map keys are sorted by encoding/json
		w.Fields["template.annotations"] = hashBytes(b)
	}
	if len(tpl.Metadata.Labels) > 0 {
		b, _ := json.Marshal(tpl.Metadata.Labels)
		w.Fields["template.labels"] = hashBytes(b)
	}
	podSpec := map[string]any{}
	for k, v := range tpl.Spec {
		var anyV any
		if json.Unmarshal(v, &anyV) == nil {
			podSpec[k] = anyV
		}
	}
	w.Refs = workloadRefs(podSpec)
	for k, v := range tpl.Spec {
		if k != "containers" && k != "initContainers" {
			w.Fields["pod."+k] = hashBytes(v)
			continue
		}
		var cs []rawContainer
		if json.Unmarshal(v, &cs) != nil {
			continue
		}
		for _, c := range cs {
			var name, image string
			_ = json.Unmarshal(c["name"], &name)
			_ = json.Unmarshal(c["image"], &image)
			prefix := k + "[" + name + "]"
			w.Images[prefix] = image
			for field, fv := range c {
				if field == "name" || field == "image" {
					continue
				}
				w.Fields[prefix+"."+field] = hashBytes(fv)
			}
		}
	}
}

// containerLabel turns "containers[api]" into "api" and "initContainers[x]" into "x (init)".
func containerLabel(k string) string {
	name := k[strings.Index(k, "[")+1 : len(k)-1]
	if strings.HasPrefix(k, "initContainers") {
		return name + " (init)"
	}
	return name
}

// shortImageChange renders "repo:1.2 → 1.3" when only the tag/digest moved.
func shortImageChange(old, cur string) string {
	split := func(s string) (repo, sep, ref string) {
		if i := strings.Index(s, "@"); i > 0 {
			ref = s[i+1:]
			if alg, hex, ok := strings.Cut(ref, ":"); ok && len(hex) > 12 {
				ref = alg + ":" + hex[:12]
			}
			return s[:i], "@", ref
		}
		slash := strings.LastIndex(s, "/")
		if i := strings.LastIndex(s, ":"); i > slash {
			return s[:i], ":", s[i+1:]
		}
		return s, "", ""
	}
	or, osep, ot := split(old)
	cr, csep, ct := split(cur)
	if or == cr && osep == csep && ot != "" && ct != "" {
		return fmt.Sprintf("%s%s%s → %s", cr, csep, ot, ct)
	}
	return fmt.Sprintf("%s → %s", old, cur)
}

func imageList(imgs map[string]string) string {
	keys := sortedKeys(imgs)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.HasPrefix(k, "containers") {
			parts = append(parts, imgs[k])
		}
	}
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// diffKeys returns keys that changed, were added, or were removed.
func diffKeys(old, cur map[string]string) (changed, added, removed []string) {
	for k, v := range cur {
		ov, ok := old[k]
		switch {
		case !ok:
			added = append(added, k)
		case ov != v:
			changed = append(changed, k)
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(changed)
	sort.Strings(added)
	sort.Strings(removed)
	return
}

func limitList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" (+%d more)", len(items)-n)
}

func diffWorkload(e Emitter, old, cur workload) {
	if old.Generation != 0 && old.Generation == cur.Generation {
		return // status-only update
	}
	// Images
	var imgs []string
	for _, k := range sortedKeys(cur.Images) {
		if o, ok := old.Images[k]; ok && o != cur.Images[k] {
			imgs = append(imgs, containerLabel(k)+": "+shortImageChange(o, cur.Images[k]))
		}
	}
	if len(imgs) > 0 {
		e.Emit(change.ActionImage, change.ImpactHigh, strings.Join(imgs, "; "))
	}
	// Everything else in the template/spec
	changed, added, removed := diffKeys(old.Fields, cur.Fields)
	var parts []string
	if len(changed) > 0 {
		parts = append(parts, "changed "+limitList(changed, 4))
	}
	if len(added) > 0 {
		parts = append(parts, "added "+limitList(added, 3))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+limitList(removed, 3))
	}
	// Containers added/removed show up as image map changes
	for _, k := range sortedKeys(cur.Images) {
		if _, ok := old.Images[k]; !ok {
			parts = append(parts, "new container "+containerLabel(k))
		}
	}
	for _, k := range sortedKeys(old.Images) {
		if _, ok := cur.Images[k]; !ok {
			parts = append(parts, "removed container "+containerLabel(k))
		}
	}
	if len(parts) > 0 {
		e.Emit(change.ActionConfig, change.ImpactMedium, strings.Join(parts, "; "))
	}
	if old.RestartedAt != cur.RestartedAt && cur.RestartedAt != "" {
		e.Emit(change.ActionRestart, change.ImpactMedium, "rollout restart")
	}
	if old.Replicas != nil && cur.Replicas != nil && *old.Replicas != *cur.Replicas {
		e.Emit(change.ActionScale, change.ImpactLow, fmt.Sprintf("replicas %d → %d", *old.Replicas, *cur.Replicas))
	}
	if old.Paused != cur.Paused {
		if cur.Paused {
			e.Emit(change.ActionConfig, change.ImpactLow, "rollout paused")
		} else {
			e.Emit(change.ActionConfig, change.ImpactLow, "rollout resumed")
		}
	}
}

func workloadSpec(name, kind, path string) Spec[workload] {
	refs := func(e Emitter, m Meta, v workload) { e.Graph().SetRefs(m.Namespace, kind+"/"+m.Name, v.Refs) }
	return Spec[workload]{
		Name:      name,
		Kind:      kind,
		Source:    "kubernetes",
		Resources: []kube.Resource{{Path: path}},
		Decode:    decodeWorkload,
		Diffable:  true,
		Backfill:  refs,
		Added: func(e Emitter, m Meta, v workload) {
			refs(e, m, v)
			e.Emit(change.ActionCreate, change.ImpactMedium, "created ("+imageList(v.Images)+")", At(m.CreationTimestamp))
		},
		Updated: func(e Emitter, m Meta, old, cur workload) {
			refs(e, m, cur)
			diffWorkload(e, old, cur)
		},
		Deleted: func(e Emitter, m Meta, _ workload) {
			e.Graph().SetRefs(m.Namespace, kind+"/"+m.Name, nil)
			e.Emit(change.ActionDelete, change.ImpactMedium, "deleted")
		},
	}
}

// Deployments tracks apps/v1 Deployments.
func Deployments(env Env) Tracker {
	return New(workloadSpec("deployments", "Deployment", "/apis/apps/v1/deployments"), env)
}

// StatefulSets tracks apps/v1 StatefulSets.
func StatefulSets(env Env) Tracker {
	return New(workloadSpec("statefulsets", "StatefulSet", "/apis/apps/v1/statefulsets"), env)
}

// DaemonSets tracks apps/v1 DaemonSets.
func DaemonSets(env Env) Tracker {
	return New(workloadSpec("daemonsets", "DaemonSet", "/apis/apps/v1/daemonsets"), env)
}
