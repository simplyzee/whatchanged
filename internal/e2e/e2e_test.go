// Package e2e runs the watcher, store and API together against a fake
// Kubernetes API server.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/api"
	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
	"github.com/simplyzee/whatchanged/internal/kube/kubetest"
	"github.com/simplyzee/whatchanged/internal/store"
	"github.com/simplyzee/whatchanged/internal/tracker"
)

const (
	deployPath  = "/apis/apps/v1/deployments"
	secretsPath = "/api/v1/secrets"
	eventsPath  = "/api/v1/events"
)

type obj = kubetest.Obj

func deploy(image string, gen int) obj {
	return obj{
		"metadata": obj{"name": "api", "namespace": "payments", "uid": "d1", "generation": gen},
		"spec": obj{"replicas": 2, "template": obj{"spec": obj{
			"containers": []obj{{"name": "api", "image": image}},
		}}},
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %s %s", url, resp.Status, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEnd(t *testing.T) {
	// Only core resources exist: every optional CRD returns 404.
	f := kubetest.New(deployPath, secretsPath, eventsPath)
	defer f.Close()
	f.Add(deployPath, deploy("ghcr.io/acme/api:1.0.0", 1))
	f.Add(secretsPath, obj{"metadata": obj{
		"name": "sh.helm.release.v1.api.v4", "namespace": "payments", "uid": "h4",
		"creationTimestamp": time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		"labels":            obj{"owner": "helm", "name": "api", "version": "4", "status": "deployed"},
	}})

	st, err := store.Open(store.Options{Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sink := func(c change.Change) { st.Add(c) }
	trackers, err := tracker.Select(nil, []string{"configmaps", "nodes", "networkpolicies", "statefulsets", "daemonsets"}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := kube.New(kube.Config{Server: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := &tracker.Runner{Client: kc, Logger: logger, Recheck: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx, trackers)

	srv := &api.Server{Store: st, IngestToken: "s3cret", Ready: runner.Ready, Status: func() any { return runner.Statuses() }, Logger: logger}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	eventually(t, "ready", func() bool {
		resp, err := http.Get(hs.URL + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	for _, s := range runner.Statuses() {
		switch s.Name {
		case "deployments", "helm-releases", "warning-events":
			if s.State != "synced" {
				t.Errorf("%s: state %s", s.Name, s.State)
			}
		default:
			if s.State != "not-installed" {
				t.Errorf("%s: state %s, want not-installed", s.Name, s.State)
			}
		}
	}
	// Helm metadata-only request
	if a := f.RequestsFor(secretsPath)[0].Header.Get("Accept"); !strings.Contains(a, "PartialObjectMetadata") {
		t.Errorf("helm tracker should request metadata only, got %q", a)
	}

	// Live changes: an image bump followed by a crashloop.
	eventually(t, "watch", func() bool { return f.Watchers(deployPath) == 1 && f.Watchers(eventsPath) == 1 })
	f.Emit(deployPath, "MODIFIED", deploy("ghcr.io/acme/api:1.1.0", 2))
	f.Emit(eventsPath, "ADDED", obj{
		"metadata":       obj{"name": "api.1", "namespace": "payments", "uid": "e1"},
		"involvedObject": obj{"kind": "Pod", "namespace": "payments", "name": "api-abc"},
		"type":           "Warning", "reason": "BackOff", "message": "Back-off restarting failed container",
		"lastTimestamp": time.Now().UTC().Format(time.RFC3339),
	})

	var list api.ChangesResponse
	eventually(t, "changes", func() bool {
		getJSON(t, hs.URL+"/api/v1/changes?since=2h&namespace=payments", &list)
		return list.Count == 3
	})
	got := map[string]string{}
	for _, c := range list.Changes {
		got[c.Action] = c.Summary
	}
	if got["image"] != "api: ghcr.io/acme/api:1.0.0 → 1.1.0" {
		t.Errorf("image change = %q", got["image"])
	}
	if got["release"] != "upgraded to revision 4 (deployed)" {
		t.Errorf("helm backfill = %q", got["release"])
	}

	// CI pushes a change too.
	body, _ := json.Marshal(change.Change{Source: "github-actions", Name: "api", Namespace: "payments", Summary: "deploy v1.1.0", Impact: "high"})
	req, _ := http.NewRequest("POST", hs.URL+"/api/v1/changes", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated push: %s", resp.Status)
	}
	req, _ = http.NewRequest("POST", hs.URL+"/api/v1/changes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push: %s", resp.Status)
	}

	// Suspects: the image change should top the list, with the BackOff as evidence.
	var sus api.SuspectsResponse
	getJSON(t, hs.URL+"/api/v1/suspects?namespace=payments&window=2h", &sus)
	if len(sus.Suspects) != 3 || sus.Warnings != 1 {
		t.Fatalf("suspects = %+v", sus)
	}
	top := sus.Suspects[0]
	if top.Action != "image" && top.Source != "github-actions" {
		t.Errorf("unexpected top suspect %+v", top)
	}

	// Grafana annotations exclude warnings and low impact.
	var ann []api.GrafanaAnnotation
	from := time.Now().Add(-2 * time.Hour).UnixMilli()
	getJSON(t, hs.URL+"/api/v1/grafana/annotations?from="+itoa(from)+"&to="+itoa(time.Now().Add(time.Minute).UnixMilli()), &ann)
	if len(ann) != 3 {
		t.Errorf("annotations = %+v", ann)
	}

	// Watch expiry: the next change still arrives after a relist.
	f.ExpireNextWatch(deployPath, 1)
	f.CloseClientConnections()
	eventually(t, "re-watch", func() bool { return f.Watchers(deployPath) == 1 })
	f.Emit(deployPath, "MODIFIED", deploy("ghcr.io/acme/api:1.2.0", 3))
	eventually(t, "post-relist change", func() bool {
		getJSON(t, hs.URL+"/api/v1/changes?since=2h&action=image", &list)
		return list.Count == 2
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
