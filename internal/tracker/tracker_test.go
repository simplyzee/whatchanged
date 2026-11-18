package tracker

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

type collector struct {
	mu  sync.Mutex
	got []change.Change
}

func (c *collector) sink(ch change.Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, ch)
}

func (c *collector) take() []change.Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.got
	c.got = nil
	return out
}

var fixedNow = time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)

func now() time.Time { return fixedNow }

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type obj = map[string]any

func deployment(rv string, gen int, replicas int, image string, env string, restartedAt string) obj {
	ann := obj{}
	if restartedAt != "" {
		ann[restartedAtAnnotation] = restartedAt
	}
	return obj{
		"metadata": obj{
			"name": "api", "namespace": "payments", "uid": "u-api", "resourceVersion": rv, "generation": gen,
			"creationTimestamp": "2026-11-17T10:00:00Z",
			"managedFields": []obj{
				{"manager": "argocd-controller", "operation": "Apply", "time": "2026-11-17T18:00:00Z"},
				{"manager": "kubectl-edit", "operation": "Update", "time": "2026-11-17T18:55:00Z"},
				{"manager": "kube-controller-manager", "operation": "Update", "subresource": "status", "time": "2026-11-17T18:59:00Z"},
			},
		},
		"spec": obj{
			"replicas": replicas,
			"strategy": obj{"type": "RollingUpdate"},
			"template": obj{
				"metadata": obj{"labels": obj{"app": "api"}, "annotations": ann},
				"spec": obj{
					"containers": []obj{{"name": "api", "image": image, "env": []obj{{"name": "MODE", "value": env}}}},
				},
			},
		},
	}
}

func TestDeploymentDiffs(t *testing.T) {
	var c collector
	tr := Deployments(Env{Sink: c.sink, Now: now})
	tr.Replace([]json.RawMessage{raw(t, deployment("1", 1, 3, "ghcr.io/acme/api:1.2.0", "a", ""))}, true)
	if got := c.take(); len(got) != 0 {
		t.Fatalf("initial sync must be silent, got %v", got)
	}

	// status-only update (same generation): nothing
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, deployment("2", 1, 3, "ghcr.io/acme/api:1.2.0", "a", ""))})
	if got := c.take(); len(got) != 0 {
		t.Fatalf("status update should be ignored, got %v", got)
	}

	// image + env + scale in one go
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, deployment("3", 2, 5, "ghcr.io/acme/api:1.3.0", "b", ""))})
	got := c.take()
	byAction := map[string]change.Change{}
	for _, g := range got {
		byAction[g.Action] = g
	}
	img, ok := byAction[change.ActionImage]
	if !ok {
		t.Fatalf("expected image change, got %+v", got)
	}
	if img.Summary != "api: ghcr.io/acme/api:1.2.0 → 1.3.0" || img.Impact != change.ImpactHigh {
		t.Errorf("image summary = %q impact %s", img.Summary, img.Impact)
	}
	if img.Actor != "kubectl-edit" {
		t.Errorf("actor = %q, want kubectl-edit (latest non-status manager)", img.Actor)
	}
	if img.Namespace != "payments" || img.Kind != "Deployment" || img.Name != "api" {
		t.Errorf("object = %s %s/%s", img.Kind, img.Namespace, img.Name)
	}
	cfg := byAction[change.ActionConfig]
	if !strings.Contains(cfg.Summary, "containers[api].env") {
		t.Errorf("config summary should name the env change, got %q", cfg.Summary)
	}
	if s := byAction[change.ActionScale].Summary; s != "replicas 3 → 5" {
		t.Errorf("scale summary = %q", s)
	}

	// rollout restart only
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, deployment("4", 3, 5, "ghcr.io/acme/api:1.3.0", "b", "2026-11-17T19:00:00Z"))})
	got = c.take()
	if len(got) != 1 || got[0].Action != change.ActionRestart {
		t.Fatalf("expected a single restart, got %+v", got)
	}

	// replayed event with the same resourceVersion: nothing
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, deployment("4", 3, 5, "ghcr.io/acme/api:1.3.0", "b", "2026-11-17T19:00:00Z"))})
	if got := c.take(); len(got) != 0 {
		t.Fatalf("replay should be ignored, got %v", got)
	}

	tr.Apply(kube.Event{Type: "DELETED", Object: raw(t, deployment("5", 3, 5, "ghcr.io/acme/api:1.3.0", "b", ""))})
	got = c.take()
	if len(got) != 1 || got[0].Action != change.ActionDelete {
		t.Fatalf("expected delete, got %+v", got)
	}
}

func TestRelistDetectsMissedChanges(t *testing.T) {
	var c collector
	tr := Deployments(Env{Sink: c.sink, Now: now})
	tr.Replace([]json.RawMessage{raw(t, deployment("1", 1, 3, "img:1", "a", ""))}, true)

	other := deployment("7", 1, 1, "worker:1", "a", "")
	other["metadata"].(obj)["uid"] = "u-worker"
	other["metadata"].(obj)["name"] = "worker"
	// After a watch expiry the relist shows: api changed, worker appeared.
	tr.Replace([]json.RawMessage{raw(t, deployment("6", 2, 3, "img:2", "a", "")), raw(t, other)}, false)
	got := c.take()
	actions := map[string]bool{}
	for _, g := range got {
		actions[g.Name+"/"+g.Action] = true
	}
	if !actions["api/image"] || !actions["worker/create"] {
		t.Fatalf("relist should detect update and create, got %+v", got)
	}

	// And a relist without worker means it was deleted.
	tr.Replace([]json.RawMessage{raw(t, deployment("6", 2, 3, "img:2", "a", ""))}, false)
	got = c.take()
	if len(got) != 1 || got[0].Name != "worker" || got[0].Action != change.ActionDelete {
		t.Fatalf("expected worker delete, got %+v", got)
	}
}

func configMap(rv string, data obj) obj {
	return obj{
		"metadata": obj{"name": "api-config", "namespace": "payments", "uid": "u-cm", "resourceVersion": rv},
		"data":     data,
	}
}

func TestConfigMapKeys(t *testing.T) {
	var c collector
	tr := ConfigMaps(Env{Sink: c.sink, Now: now})
	tr.Replace([]json.RawMessage{raw(t, configMap("1", obj{"a": "1", "b": "2", "c": "3"}))}, true)
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, configMap("2", obj{"a": "1", "b": "changed", "d": "4"}))})
	got := c.take()
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	want := "data keys changed b; added d; removed c"
	if got[0].Summary != want {
		t.Errorf("summary = %q, want %q", got[0].Summary, want)
	}
	// label-only change: no data diff, nothing emitted
	cm := configMap("3", obj{"a": "1", "b": "changed", "d": "4"})
	cm["metadata"].(obj)["labels"] = obj{"x": "y"}
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, cm)})
	if got := c.take(); len(got) != 0 {
		t.Errorf("metadata-only change should be ignored, got %+v", got)
	}
}

func helmSecret(uid, rv, release string, rev int, status, created string) obj {
	return obj{"metadata": obj{
		"name": "sh.helm.release.v1." + release + ".v" + itoa(rev), "namespace": "payments", "uid": uid,
		"resourceVersion": rv, "creationTimestamp": created,
		"labels": obj{"owner": "helm", "name": release, "version": itoa(rev), "status": status},
	}}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestHelmLifecycleAndBackfillDedupe(t *testing.T) {
	var c collector
	tr := Helm(Env{Sink: c.sink, Now: now})
	// Backfill: rev 1 superseded, rev 2 deployed
	tr.Replace([]json.RawMessage{
		raw(t, helmSecret("h1", "1", "api", 1, "superseded", "2026-11-16T10:00:00Z")),
		raw(t, helmSecret("h2", "2", "api", 2, "deployed", "2026-11-17T10:00:00Z")),
	}, true)
	back := c.take()
	if len(back) != 2 {
		t.Fatalf("expected 2 backfilled releases, got %+v", back)
	}
	if back[1].Summary != "upgraded to revision 2 (deployed)" || !back[1].Time.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("backfill = %+v", back[1])
	}
	if back[0].Summary != "installed to revision 1" {
		t.Errorf("superseded summary = %q", back[0].Summary)
	}

	// Live upgrade: pending first (silent), then deployed.
	tr.Apply(kube.Event{Type: "ADDED", Object: raw(t, helmSecret("h3", "3", "api", 3, "pending-upgrade", "2026-11-17T18:50:00Z"))})
	if got := c.take(); len(got) != 0 {
		t.Fatalf("pending release should wait, got %+v", got)
	}
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, helmSecret("h3", "4", "api", 3, "failed", "2026-11-17T18:50:00Z"))})
	got := c.take()
	if len(got) != 1 || got[0].Summary != "upgraded to revision 3 (failed)" || got[0].Kind != "HelmRelease" || got[0].Name != "api" {
		t.Fatalf("got %+v", got)
	}

	// The same revision seen again after a restart has the same ID.
	var c2 collector
	tr2 := Helm(Env{Sink: c2.sink, Now: now})
	tr2.Replace([]json.RawMessage{raw(t, helmSecret("h3", "9", "api", 3, "failed", "2026-11-17T18:50:00Z"))}, true)
	again := c2.take()
	if len(again) != 1 || again[0].ID != got[0].ID {
		t.Fatalf("backfill should reuse the live ID: %+v vs %+v", again, got)
	}
}

func TestNodeTransitions(t *testing.T) {
	var c collector
	tr := Nodes(Env{Sink: c.sink, Now: now})
	n := func(rv string, unsched bool, ready, kubelet string) json.RawMessage {
		return raw(t, obj{
			"metadata": obj{"name": "node-a", "uid": "n1", "resourceVersion": rv},
			"spec":     obj{"unschedulable": unsched},
			"status": obj{
				"nodeInfo":   obj{"kubeletVersion": kubelet},
				"conditions": []obj{{"type": "Ready", "status": ready}},
			},
		})
	}
	tr.Replace([]json.RawMessage{n("1", false, "True", "v1.30.1")}, true)
	tr.Apply(kube.Event{Type: "MODIFIED", Object: n("2", true, "True", "v1.30.1")})
	tr.Apply(kube.Event{Type: "MODIFIED", Object: n("3", true, "Unknown", "v1.30.1")})
	tr.Apply(kube.Event{Type: "MODIFIED", Object: n("4", true, "True", "v1.31.0")})
	got := c.take()
	var sums []string
	for _, g := range got {
		sums = append(sums, g.Summary)
	}
	want := []string{"cordoned", "became NotReady (Ready=Unknown)", "became Ready", "kubelet v1.30.1 → v1.31.0"}
	if strings.Join(sums, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", sums, want)
	}
	if got[1].Impact != change.ImpactHigh || got[1].Namespace != "" {
		t.Errorf("NotReady should be high impact and cluster-scoped: %+v", got[1])
	}
}

func TestWarningEventsBackfillAndLive(t *testing.T) {
	var c collector
	tr := WarningEvents(Env{Sink: c.sink, Now: now})
	ev := func(uid, reason, ts string) json.RawMessage {
		return raw(t, obj{
			"metadata":       obj{"name": "x." + uid, "namespace": "payments", "uid": uid, "resourceVersion": "1"},
			"involvedObject": obj{"kind": "Pod", "namespace": "payments", "name": "api-7d9f"},
			"reason":         reason, "message": "Back-off restarting failed container", "lastTimestamp": ts, "type": "Warning",
		})
	}
	tr.Replace([]json.RawMessage{ev("e1", "BackOff", "2026-11-17T18:40:00Z")}, true)
	tr.Apply(kube.Event{Type: "ADDED", Object: ev("e2", "OOMKilling", "2026-11-17T18:58:00Z")})
	got := c.take()
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if !got[0].IsSignal() || got[0].Object() != "Pod/api-7d9f" || !strings.HasPrefix(got[0].Summary, "BackOff: ") {
		t.Errorf("bad warning: %+v", got[0])
	}
}

func TestArgoSync(t *testing.T) {
	var c collector
	tr := ArgoApplications(Env{Sink: c.sink, Now: now})
	app := func(rv, rev, health string) json.RawMessage {
		return raw(t, obj{
			"metadata": obj{"name": "payments", "namespace": "argocd", "uid": "a1", "resourceVersion": rv},
			"status": obj{
				"sync":           obj{"revision": rev},
				"health":         obj{"status": health},
				"operationState": obj{"finishedAt": "2026-11-17T18:30:00Z", "syncResult": obj{"revision": rev}},
			},
		})
	}
	sha1 := "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee"
	sha2 := "1111111122222222333333334444444455555555"
	tr.Replace([]json.RawMessage{app("1", sha1, "Healthy")}, true)
	if got := c.take(); len(got) != 1 || got[0].Summary != "synced to aaaaaaaa" {
		t.Fatalf("backfill: %+v", got)
	}
	tr.Apply(kube.Event{Type: "MODIFIED", Object: app("2", sha2, "Degraded")})
	got := c.take()
	if len(got) != 2 || got[0].Summary != "synced aaaaaaaa → 11111111" || got[1].Action != change.ActionWarning {
		t.Fatalf("got %+v", got)
	}
}

func TestSelect(t *testing.T) {
	ts, err := Select([]string{"secrets"}, []string{"warning-events"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tr := range ts {
		names[tr.Name()] = true
	}
	if !names["secrets"] || names["warning-events"] || !names["deployments"] {
		t.Errorf("unexpected selection: %v", names)
	}
	if _, err := Select([]string{"nope"}, nil, Env{}); err == nil {
		t.Error("unknown tracker should error")
	}
}

func TestShortImageChange(t *testing.T) {
	cases := map[[2]string]string{
		{"nginx:1.25", "nginx:1.27"}:                            "nginx:1.25 → 1.27",
		{"localhost:5000/app:v1", "localhost:5000/app:v2"}:      "localhost:5000/app:v1 → v2",
		{"repo/app@sha256:aaa", "repo/app@sha256:bbb"}:          "repo/app@sha256:aaa → sha256:bbb",
		{"docker.io/library/redis:7", "ghcr.io/acme/redis:7.2"}: "docker.io/library/redis:7 → ghcr.io/acme/redis:7.2",
		{"localhost:5000/app", "localhost:5000/app:v2"}:         "localhost:5000/app → localhost:5000/app:v2",
	}
	for in, want := range cases {
		if got := shortImageChange(in[0], in[1]); got != want {
			t.Errorf("shortImageChange(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
