package tracker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

// step feeds an initial object then an update, returning what was emitted.
func step(t *testing.T, tr Tracker, c *collector, before, after obj) []change.Change {
	t.Helper()
	tr.Replace([]json.RawMessage{raw(t, before)}, true)
	c.take()
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, after)})
	return c.take()
}

func withMeta(rv string, o obj) obj {
	m, _ := o["metadata"].(obj)
	if m == nil {
		m = obj{}
	}
	m["uid"], m["resourceVersion"] = "uid-1", rv
	if m["name"] == nil {
		m["name"] = "thing"
	}
	if m["namespace"] == nil {
		m["namespace"] = "payments"
	}
	o["metadata"] = m
	return o
}

func one(t *testing.T, got []change.Change) change.Change {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("want 1 change, got %d: %+v", len(got), got)
	}
	return got[0]
}

func TestIngressHosts(t *testing.T) {
	var c collector
	ing := func(rv, backend string, ann string) obj {
		return withMeta(rv, obj{
			"metadata": obj{"annotations": obj{"konghq.com/plugins": ann, "kubectl.kubernetes.io/last-applied-configuration": rv}},
			"spec": obj{"ingressClassName": "kong", "rules": []obj{
				{"host": "api.example.com", "http": obj{"paths": []obj{{"path": "/", "backend": obj{"service": obj{"name": backend}}}}}},
				{"host": "www.example.com", "http": obj{"paths": []obj{}}},
			}},
		})
	}
	got := one(t, step(t, Ingresses(Env{Sink: c.sink, Now: now}), &c, ing("1", "api", "rl"), ing("2", "api-v2", "rl,auth")))
	if got.Impact != change.ImpactHigh || got.Summary != "changed host api.example.com; konghq.com/plugins annotation rl → rl,auth" &&
		got.Summary != "changed annotation konghq.com/plugins; changed host api.example.com" {
		if !strings.Contains(got.Summary, "host api.example.com") || !strings.Contains(got.Summary, "annotation konghq.com/plugins rl → rl,auth") {
			t.Errorf("summary = %q", got.Summary)
		}
	}
	if strings.Contains(got.Summary, "last-applied") {
		t.Error("kubectl bookkeeping annotations must be ignored")
	}
	if len(got.Diff) == 0 {
		t.Error("expected a diff")
	}
}

func TestServiceImpact(t *testing.T) {
	var c collector
	svc := func(rv string, sel string, ip string) obj {
		return withMeta(rv, obj{"spec": obj{"selector": obj{"app": sel}, "clusterIP": ip, "type": "ClusterIP",
			"ports": []obj{{"name": "http", "port": 80}}}})
	}
	got := one(t, step(t, Services(Env{Sink: c.sink, Now: now}), &c, svc("1", "api", "10.0.0.1"), svc("2", "api-canary", "10.0.0.1")))
	if got.Impact != change.ImpactHigh || got.Summary != "changed selector" {
		t.Errorf("got %s %q", got.Impact, got.Summary)
	}
	// clusterIP churn is ignored
	if g := step(t, Services(Env{Sink: c.sink, Now: now}), &c, svc("1", "api", "10.0.0.1"), svc("2", "api", "10.0.0.2")); len(g) != 0 {
		t.Errorf("clusterIP change should be ignored: %+v", g)
	}
}

func TestHPABoundsAndMaxSignal(t *testing.T) {
	var c collector
	hpa := func(rv string, min, max, cur int) obj {
		return withMeta(rv, obj{"spec": obj{"minReplicas": min, "maxReplicas": max}, "status": obj{"currentReplicas": cur}})
	}
	tr := HPAs(Env{Sink: c.sink, Now: now})
	got := one(t, step(t, tr, &c, hpa("1", 2, 10, 3), hpa("2", 3, 10, 3)))
	if got.Summary != "minReplicas 2 → 3" {
		t.Errorf("summary %q", got.Summary)
	}
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, hpa("3", 3, 10, 10))})
	w := one(t, c.take())
	if !w.IsSignal() || !strings.HasPrefix(w.Summary, "MaxReplicas: scaled to maxReplicas (10)") {
		t.Errorf("signal %+v", w)
	}
	tr.Apply(kube.Event{Type: "MODIFIED", Object: raw(t, hpa("4", 3, 10, 10))})
	if g := c.take(); len(g) != 0 {
		t.Errorf("staying at max should not re-signal: %+v", g)
	}
}

func TestWebhookIgnoresCABundleAndNotesFailClosed(t *testing.T) {
	var c collector
	wh := func(rv, ca, policy string) obj {
		return withMeta(rv, obj{"metadata": obj{"name": "kyverno", "namespace": ""}, "webhooks": []obj{{
			"name": "validate.kyverno.svc", "failurePolicy": policy,
			"clientConfig": obj{"caBundle": ca, "service": obj{"name": "kyverno"}},
		}}})
	}
	tr := Admission(Env{Sink: c.sink, Now: now}).(multi).parts[0]
	if g := step(t, tr, &c, wh("1", "AAA", "Ignore"), wh("2", "BBB", "Ignore")); len(g) != 0 {
		t.Fatalf("caBundle rotation should be ignored: %+v", g)
	}
	got := one(t, step(t, tr, &c, wh("1", "AAA", "Ignore"), wh("2", "AAA", "Fail")))
	if got.Impact != change.ImpactHigh || !strings.Contains(got.Summary, "failurePolicy Ignore → Fail") || !strings.Contains(got.Summary, "fails closed") {
		t.Errorf("got %q", got.Summary)
	}
	if strings.Contains(strings.Join(got.Diff, "\n"), "BBB") || strings.Contains(strings.Join(got.Diff, "\n"), "AAA\"") {
		t.Errorf("caBundle should be redacted in diffs: %v", got.Diff)
	}
}

func TestRBAC(t *testing.T) {
	var c collector
	parts := RBAC(Env{Sink: c.sink, Now: now}).(multi).parts
	role, binding := parts[0], parts[2]
	cr := func(rv string, rules ...obj) obj {
		return withMeta(rv, obj{"metadata": obj{"name": "ci", "namespace": ""}, "rules": rules})
	}
	got := one(t, step(t, role, &c,
		cr("1", obj{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get", "list"}}),
		cr("2", obj{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get", "list"}},
			obj{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"get"}})))
	if got.Impact != change.ImpactHigh || got.Summary != "added rule get secrets" || got.Source != "rbac" {
		t.Errorf("role change: %s %q", got.Impact, got.Summary)
	}
	crb := func(rv string, subjects ...obj) obj {
		return withMeta(rv, obj{"metadata": obj{"name": "ops", "namespace": ""},
			"roleRef": obj{"kind": "ClusterRole", "name": "cluster-admin"}, "subjects": subjects})
	}
	got = one(t, step(t, binding, &c, crb("1", obj{"kind": "User", "name": "alice"}),
		crb("2", obj{"kind": "User", "name": "alice"}, obj{"kind": "ServiceAccount", "namespace": "ci", "name": "deployer"})))
	if got.Impact != change.ImpactHigh || got.Summary != "granted to ServiceAccount:ci/deployer (ClusterRole/cluster-admin)" {
		t.Errorf("binding change: %s %q", got.Impact, got.Summary)
	}
}

func TestCRDServedVersionDropped(t *testing.T) {
	var c collector
	crd := func(rv string, v1beta1Served bool) obj {
		return withMeta(rv, obj{"metadata": obj{"name": "widgets.acme.io", "namespace": ""}, "spec": obj{"group": "acme.io", "versions": []obj{
			{"name": "v1beta1", "served": v1beta1Served, "storage": false, "schema": obj{"x": 1}},
			{"name": "v1", "served": true, "storage": true, "schema": obj{"x": 2}},
		}}})
	}
	got := one(t, step(t, CRDs(Env{Sink: c.sink, Now: now}), &c, crd("1", true), crd("2", false)))
	if got.Impact != change.ImpactHigh || got.Summary != "version v1beta1 served true → false" {
		t.Errorf("got %s %q", got.Impact, got.Summary)
	}
}

func TestKongPluginAndCertManager(t *testing.T) {
	var c collector
	kp := func(rv string, limit int) obj {
		return withMeta(rv, obj{"plugin": "rate-limiting", "config": obj{"minute": limit, "policy": "local"}})
	}
	tr := Kong(Env{Sink: c.sink, Now: now}).(multi).parts[0]
	got := one(t, step(t, tr, &c, kp("1", 100), kp("2", 10)))
	if got.Summary != "rate-limiting plugin: config.minute 100 → 10" || got.Source != "kong" {
		t.Errorf("kong: %q", got.Summary)
	}

	cert := func(rv string, rev int, ready string) obj {
		return withMeta(rv, obj{"spec": obj{"secretName": "tls"}, "status": obj{"revision": rev,
			"conditions": []obj{{"type": "Ready", "status": ready, "message": "Issuing certificate as Secret does not exist"}}}})
	}
	ct := CertManager(Env{Sink: c.sink, Now: now}).(multi).parts[0]
	got = one(t, step(t, ct, &c, cert("1", 3, "True"), cert("2", 4, "True")))
	if got.Action != "renew" || got.Summary != "certificate renewed (revision 4)" {
		t.Errorf("renew: %+v", got)
	}
	got = one(t, step(t, ct, &c, cert("1", 4, "True"), cert("2", 4, "False")))
	if !got.IsSignal() || !strings.HasPrefix(got.Summary, "CertificateNotReady: Issuing") {
		t.Errorf("not ready: %+v", got)
	}
}

func TestQuotaAndPriority(t *testing.T) {
	var c collector
	rq := func(rv, cpu string) obj {
		return withMeta(rv, obj{"spec": obj{"hard": obj{"limits.cpu": cpu, "pods": "50"}}})
	}
	got := one(t, step(t, ResourceQuotas(Env{Sink: c.sink, Now: now}), &c, rq("1", "10"), rq("2", "4")))
	if got.Summary != "hard limits.cpu 10 → 4" {
		t.Errorf("quota %q", got.Summary)
	}
	pc := func(rv string, v int) obj {
		return withMeta(rv, obj{"metadata": obj{"name": "high", "namespace": ""}, "value": v})
	}
	got = one(t, step(t, PriorityClasses(Env{Sink: c.sink, Now: now}), &c, pc("1", 1000), pc("2", 10)))
	if got.Summary != "value 1000 → 10" || got.Namespace != "" {
		t.Errorf("priority %q", got.Summary)
	}
}

func TestDiffRedaction(t *testing.T) {
	for _, mode := range []DiffMode{DiffsRedacted, DiffsFull, DiffsOff} {
		var c collector
		tr := Deployments(Env{Sink: c.sink, Now: now, Diffs: mode})
		got := step(t, tr, &c,
			deployment("1", 1, 3, "ghcr.io/acme/api:1.2.0", "hunter2", ""),
			deployment("2", 2, 3, "ghcr.io/acme/api:1.3.0", "s3cret", ""))
		diff := strings.Join(got[0].Diff, "\n")
		switch mode {
		case DiffsOff:
			if diff != "" {
				t.Errorf("off: %s", diff)
			}
		case DiffsRedacted:
			if !strings.Contains(diff, `+ spec.template.spec.containers[api].image: "ghcr.io/acme/api:1.3.0"`) {
				t.Errorf("redacted diff should show the image:\n%s", diff)
			}
			if strings.Contains(diff, "hunter2") || strings.Contains(diff, "s3cret") || !strings.Contains(diff, "env[MODE].value: <redacted ") {
				t.Errorf("env values must be redacted:\n%s", diff)
			}
			if strings.Contains(diff, "managedFields") || strings.Contains(diff, "resourceVersion") {
				t.Errorf("bookkeeping leaked into diff:\n%s", diff)
			}
		case DiffsFull:
			if !strings.Contains(diff, `+ spec.template.spec.containers[api].env[MODE].value: "s3cret"`) {
				t.Errorf("full diff should show values:\n%s", diff)
			}
		}
	}
	// ConfigMap data is redacted, Secrets never diff.
	var c collector
	got := step(t, ConfigMaps(Env{Sink: c.sink, Now: now, Diffs: DiffsRedacted}), &c,
		configMap("1", obj{"DB_HOST": "db-a"}), configMap("2", obj{"DB_HOST": "db-b"}))
	if d := strings.Join(got[0].Diff, "\n"); strings.Contains(d, "db-b") || !strings.Contains(d, "data.DB_HOST: <redacted") {
		t.Errorf("configmap diff: %s", d)
	}
	sec := func(rv, v string) obj {
		return obj{"metadata": obj{"name": "s", "namespace": "a", "uid": "s1", "resourceVersion": rv}, "type": "Opaque", "data": obj{"k": v}}
	}
	got = step(t, Secrets(Env{Sink: c.sink, Now: now, Diffs: DiffsFull}), &c, sec("1", "YQ=="), sec("2", "Yg=="))
	if len(got) != 1 || len(got[0].Diff) != 0 {
		t.Errorf("secrets must never carry diffs: %+v", got)
	}
}

func TestGraphCorrelation(t *testing.T) {
	var c collector
	env := Env{Sink: c.sink, Now: now}.withDefaults()
	// Deployment uses a ConfigMap; a Pod belongs to its ReplicaSet.
	d := deployment("1", 1, 2, "img:1", "a", "")
	tpl := d["spec"].(obj)["template"].(obj)["spec"].(obj)
	tpl["volumes"] = []obj{{"name": "cfg", "configMap": obj{"name": "api-config"}}}
	tpl["containers"].([]obj)[0]["envFrom"] = []obj{{"secretRef": obj{"name": "api-creds"}}}
	Deployments(env).Replace([]json.RawMessage{raw(t, d)}, true)

	idx := expand([]Tracker{OwnerIndex(env)})
	ctrl := true
	idx[1].Replace([]json.RawMessage{raw(t, obj{"metadata": obj{"name": "api-7d9f", "namespace": "payments", "uid": "rs1",
		"ownerReferences": []obj{{"kind": "Deployment", "name": "api", "controller": ctrl}}}})}, true)
	idx[0].Replace([]json.RawMessage{raw(t, obj{"metadata": obj{"name": "api-7d9f-x2x2", "namespace": "payments", "uid": "p1",
		"ownerReferences": []obj{{"kind": "ReplicaSet", "name": "api-7d9f", "controller": ctrl}}}})}, true)

	g := env.Graph
	if o := g.Owner("payments", "Pod", "api-7d9f-x2x2"); o != "Deployment/api" {
		t.Errorf("owner = %q", o)
	}
	if u := g.Users("payments", "ConfigMap", "api-config"); len(u) != 1 || u[0] != "Deployment/api" {
		t.Errorf("configmap users = %v", u)
	}
	if u := g.Related("payments", "Secret", "api-creds"); len(u) != 1 {
		t.Errorf("secret users = %v", u)
	}

	// Warning events get the owner label.
	ev := WarningEvents(env)
	ev.Apply(kube.Event{Type: "ADDED", Object: raw(t, obj{
		"metadata":       obj{"name": "e", "namespace": "payments", "uid": "e1", "resourceVersion": "1"},
		"involvedObject": obj{"kind": "Pod", "namespace": "payments", "name": "api-7d9f-x2x2"},
		"reason":         "BackOff", "message": "x", "lastTimestamp": "2026-11-17T18:58:00Z",
	})})
	w := c.take()
	if len(w) != 1 || w[0].Labels["owner"] != "Deployment/api" {
		t.Errorf("warning owner: %+v", w)
	}

	// Pod deleted: still resolves for a while.
	idx[0].Apply(kube.Event{Type: "DELETED", Object: raw(t, obj{"metadata": obj{"name": "api-7d9f-x2x2", "namespace": "payments", "uid": "p1"}})})
	if o := g.Owner("payments", "Pod", "api-7d9f-x2x2"); o != "Deployment/api" {
		t.Errorf("deleted pod should still resolve, got %q", o)
	}
}

func TestDiffFlatLimit(t *testing.T) {
	old, cur := map[string]string{}, map[string]string{}
	for i := 0; i < 100; i++ {
		k := "k" + strings.Repeat("x", i)
		old[k], cur[k] = "1", "2"
	}
	d := diffFlat(old, cur, 20)
	if len(d) != 21 || !strings.Contains(d[20], "90 more changed paths") {
		t.Errorf("got %d lines, last %q", len(d), d[len(d)-1])
	}
}
