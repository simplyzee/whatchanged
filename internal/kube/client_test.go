package kube_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/kube"
	"github.com/simplyzee/whatchanged/internal/kube/kubetest"
)

const path = "/api/v1/configmaps"

func newClient(t *testing.T, f *kubetest.Fake) *kube.Client {
	t.Helper()
	c, err := kube.New(kube.Config{Server: f.URL, Token: "t0ken"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListPaginatesAndAuthenticates(t *testing.T) {
	f := kubetest.New(path)
	defer f.Close()
	for i := 0; i < 1203; i++ {
		f.Add(path, kubetest.Obj{"metadata": kubetest.Obj{"name": fmt.Sprint("cm-", i), "uid": fmt.Sprint(i)}})
	}
	res, err := newClient(t, f).List(context.Background(), kube.Resource{Path: path, LabelSelector: "a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1203 || res.ResourceVersion != "1203" {
		t.Fatalf("got %d items rv %s", len(res.Items), res.ResourceVersion)
	}
	reqs := f.RequestsFor(path)
	if len(reqs) != 3 {
		t.Errorf("expected 3 pages, got %d requests", len(reqs))
	}
	if h := reqs[0].Header.Get("Authorization"); h != "Bearer t0ken" {
		t.Errorf("auth header = %q", h)
	}
	if q := reqs[0].URL.Query().Get("labelSelector"); q != "a=b" {
		t.Errorf("labelSelector = %q", q)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	f := kubetest.New()
	defer f.Close()
	_, err := newClient(t, f).List(context.Background(), kube.Resource{Path: "/apis/nope/v1/things"})
	if !kube.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestMetadataOnlyAcceptHeader(t *testing.T) {
	f := kubetest.New(path)
	defer f.Close()
	_, err := newClient(t, f).List(context.Background(), kube.Resource{Path: path, MetadataOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if a := f.RequestsFor(path)[0].Header.Get("Accept"); !strings.Contains(a, "as=PartialObjectMetadataList") {
		t.Errorf("accept = %q", a)
	}
}

type recorder struct {
	mu       sync.Mutex
	replaces []int
	events   []string
}

func (r *recorder) Replace(items []json.RawMessage, initial bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replaces = append(r.replaces, len(items))
}

func (r *recorder) Apply(ev kube.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var m struct {
		Metadata struct{ Name string } `json:"metadata"`
	}
	json.Unmarshal(ev.Object, &m)
	r.events = append(r.events, ev.Type+":"+m.Metadata.Name)
}

func (r *recorder) snapshot() ([]int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.replaces...), append([]string(nil), r.events...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReflectWatchesAndRelistsOnGone(t *testing.T) {
	f := kubetest.New(path)
	defer f.Close()
	f.Add(path, kubetest.Obj{"metadata": kubetest.Obj{"name": "one", "uid": "1"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &recorder{}
	synced := make(chan struct{})
	restarts := 0
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- kube.Reflect(ctx, newClient(t, f), kube.Resource{Path: path}, rec, kube.ReflectOptions{
			OnSynced:  func() { close(synced) },
			OnRestart: func() { mu.Lock(); restarts++; mu.Unlock() },
			MinWatch:  time.Minute,
		})
	}()
	<-synced
	eventually(t, "watch", func() bool { return f.Watchers(path) == 1 })
	f.Emit(path, "ADDED", kubetest.Obj{"metadata": kubetest.Obj{"name": "two", "uid": "2"}})
	eventually(t, "event", func() bool { _, ev := rec.snapshot(); return len(ev) == 1 })

	// Expire the next watch, then drop the current one: the reflector must relist.
	f.ExpireNextWatch(path, 1)
	f.CloseClientConnections()
	eventually(t, "relist", func() bool { r, _ := rec.snapshot(); return len(r) == 2 })
	reps, evs := rec.snapshot()
	if reps[0] != 1 || reps[1] != 2 {
		t.Errorf("replaces = %v, want [1 2]", reps)
	}
	if evs[0] != "ADDED:two" {
		t.Errorf("events = %v", evs)
	}
	eventually(t, "re-watch", func() bool { return f.Watchers(path) == 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Reflect returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reflect did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if restarts == 0 {
		t.Error("expected a restart to be reported")
	}
}

func TestReflectReturnsNotFoundForMissingCRD(t *testing.T) {
	f := kubetest.New()
	defer f.Close()
	err := kube.Reflect(context.Background(), newClient(t, f), kube.Resource{Path: "/apis/cilium.io/v2/ciliumnetworkpolicies"}, &recorder{}, kube.ReflectOptions{})
	if !kube.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}
