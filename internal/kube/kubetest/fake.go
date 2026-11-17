// Package kubetest provides a fake Kubernetes API server that supports just
// enough list/watch behaviour to test whatchanged end to end.
package kubetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

// Obj is a loosely typed Kubernetes object.
type Obj = map[string]any

type watcher struct {
	ch chan []byte
}

// Fake is an in-memory API server.
type Fake struct {
	*httptest.Server

	mu       sync.Mutex
	rv       int
	objs     map[string][]Obj // path -> objects
	known    map[string]bool  // paths that "exist"
	watchers map[string][]*watcher
	gone     map[string]int // path -> number of upcoming watches to fail with 410
	// Requests records method+URL of every request, for assertions.
	Requests []*http.Request
}

// New starts a fake server serving the given collection paths.
func New(paths ...string) *Fake {
	f := &Fake{
		objs:     map[string][]Obj{},
		known:    map[string]bool{},
		watchers: map[string][]*watcher{},
		gone:     map[string]int{},
	}
	for _, p := range paths {
		f.known[p] = true
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func meta(o Obj) Obj {
	m, ok := o["metadata"].(Obj)
	if !ok {
		m = Obj{}
		o["metadata"] = m
	}
	return m
}

// Add stores an object without notifying watchers (use before the client
// starts, to simulate pre-existing state).
func (f *Fake) Add(path string, o Obj) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	meta(o)["resourceVersion"] = strconv.Itoa(f.rv)
	f.objs[path] = append(f.objs[path], o)
}

// Emit applies an event (ADDED/MODIFIED/DELETED) and sends it to watchers.
func (f *Fake) Emit(path, typ string, o Obj) {
	f.mu.Lock()
	f.rv++
	meta(o)["resourceVersion"] = strconv.Itoa(f.rv)
	uid := meta(o)["uid"]
	list := f.objs[path]
	idx := -1
	for i, e := range list {
		if meta(e)["uid"] == uid {
			idx = i
		}
	}
	switch {
	case typ == "DELETED" && idx >= 0:
		list = append(list[:idx], list[idx+1:]...)
	case idx >= 0:
		list[idx] = o
	case typ != "DELETED":
		list = append(list, o)
	}
	f.objs[path] = list
	b, _ := json.Marshal(map[string]any{"type": typ, "object": o})
	ws := append([]*watcher(nil), f.watchers[path]...)
	f.mu.Unlock()
	for _, w := range ws {
		select {
		case w.ch <- b:
		case <-time.After(2 * time.Second):
		}
	}
}

// ExpireNextWatch makes the next n watches on path fail with 410 Gone.
func (f *Fake) ExpireNextWatch(path string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gone[path] += n
}

// Watchers returns how many watches are open on path.
func (f *Fake) Watchers(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers[path])
}

// RequestsFor returns recorded requests for a path.
func (f *Fake) RequestsFor(path string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.Requests {
		if r.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Requests = append(f.Requests, r.Clone(r.Context()))
	known := f.known[r.URL.Path]
	f.mu.Unlock()
	if !known {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","code":404,"reason":"NotFound","message":"the server could not find the requested resource"}`)
		return
	}
	if r.URL.Query().Get("watch") == "true" {
		f.watch(w, r)
		return
	}
	f.list(w, r)
}

func (f *Fake) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	items := append([]Obj(nil), f.objs[r.URL.Path]...)
	rv := f.rv
	f.mu.Unlock()
	start, _ := strconv.Atoi(r.URL.Query().Get("continue"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = len(items) + 1
	}
	end := min(start+limit, len(items))
	if start > len(items) {
		start = len(items)
	}
	cont := ""
	if end < len(items) {
		cont = strconv.Itoa(end)
	}
	page := items[start:end]
	if page == nil {
		page = []Obj{}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"metadata": map[string]any{"resourceVersion": strconv.Itoa(rv), "continue": cont},
		"items":    page,
	})
}

func (f *Fake) watch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.mu.Lock()
	if f.gone[path] > 0 {
		f.gone[path]--
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"type": "ERROR", "object": map[string]any{
			"kind": "Status", "code": 410, "reason": "Expired", "message": "too old resource version",
		}})
		w.Write(append(b, '\n'))
		return
	}
	wt := &watcher{ch: make(chan []byte, 64)}
	f.watchers[path] = append(f.watchers[path], wt)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		ws := f.watchers[path]
		for i, x := range ws {
			if x == wt {
				f.watchers[path] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-wt.ch:
			w.Write(append(b, '\n'))
			if fl != nil {
				fl.Flush()
			}
		}
	}
}
