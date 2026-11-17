package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/simplyzee/whatchanged/internal/kube"
)

// Constructor builds a tracker.
type Constructor func(Sink, func() time.Time) Tracker

// Registry lists every built-in tracker. Default reports whether it runs
// unless explicitly disabled.
var Registry = []struct {
	Name    string
	New     Constructor
	Default bool
}{
	{"deployments", Deployments, true},
	{"statefulsets", StatefulSets, true},
	{"daemonsets", DaemonSets, true},
	{"configmaps", ConfigMaps, true},
	{"secrets", Secrets, false}, // opt-in: needs read access to Secret data
	{"helm-releases", Helm, true},
	{"nodes", Nodes, true},
	{"networkpolicies", NetworkPolicies, true},
	{"cilium-networkpolicies", CiliumNetworkPolicies, true},
	{"cilium-clusterwidenetworkpolicies", CiliumClusterwideNetworkPolicies, true},
	{"argocd-applications", ArgoApplications, true},
	{"flux-kustomizations", FluxKustomizations, true},
	{"flux-helmreleases", FluxHelmReleases, true},
	{"warning-events", WarningEvents, true},
}

// Names returns all tracker names.
func Names() []string {
	out := make([]string, len(Registry))
	for i, r := range Registry {
		out[i] = r.Name
	}
	return out
}

// Select builds trackers: defaults, plus enable, minus disable.
func Select(enable, disable []string, sink Sink, now func() time.Time) ([]Tracker, error) {
	known := map[string]bool{}
	for _, r := range Registry {
		known[r.Name] = true
	}
	set := func(list []string) (map[string]bool, error) {
		m := map[string]bool{}
		for _, n := range list {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if !known[n] {
				return nil, fmt.Errorf("unknown tracker %q (known: %s)", n, strings.Join(Names(), ", "))
			}
			m[n] = true
		}
		return m, nil
	}
	en, err := set(enable)
	if err != nil {
		return nil, err
	}
	dis, err := set(disable)
	if err != nil {
		return nil, err
	}
	var out []Tracker
	for _, r := range Registry {
		if (r.Default || en[r.Name]) && !dis[r.Name] {
			out = append(out, r.New(sink, now))
		}
	}
	return out, nil
}

// Status is the state of one tracker, for /readyz and logs.
type Status struct {
	Name     string `json:"name"`
	State    string `json:"state"` // starting, synced, not-installed, forbidden
	Path     string `json:"path,omitempty"`
	Restarts int    `json:"restarts"`
}

// Runner runs trackers against an API server.
type Runner struct {
	Client  *kube.Client
	Logger  *slog.Logger
	Recheck time.Duration // how often to retry missing CRDs / RBAC

	mu     sync.Mutex
	status map[string]*Status
}

// Statuses returns a snapshot sorted by name.
func (r *Runner) Statuses() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Status, 0, len(r.status))
	for _, s := range r.status {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Ready reports whether every tracker has finished its first list (or has
// been found to be not installed / not permitted).
func (r *Runner) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == nil {
		return false
	}
	for _, s := range r.status {
		if s.State == "starting" {
			return false
		}
	}
	return true
}

func (r *Runner) set(name string, fn func(*Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.status[name])
}

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context, trackers []Tracker) {
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}
	if r.Recheck <= 0 {
		r.Recheck = 10 * time.Minute
	}
	r.mu.Lock()
	r.status = map[string]*Status{}
	for _, t := range trackers {
		r.status[t.Name()] = &Status{Name: t.Name(), State: "starting"}
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, t := range trackers {
		wg.Add(1)
		go func(t Tracker) {
			defer wg.Done()
			r.runOne(ctx, t, log.With("tracker", t.Name()))
		}(t)
	}
	wg.Wait()
}

func (r *Runner) runOne(ctx context.Context, t Tracker, log *slog.Logger) {
	for ctx.Err() == nil {
		var lastErr error
		for _, res := range t.Resources() {
			res := res
			err := kube.Reflect(ctx, r.Client, res, t, kube.ReflectOptions{
				Logger: log,
				OnSynced: func() {
					r.set(t.Name(), func(s *Status) { s.State, s.Path = "synced", res.Path })
					log.Info("synced", "path", res.Path)
				},
				OnRestart: func() { r.set(t.Name(), func(s *Status) { s.Restarts++ }) },
			})
			if err == nil {
				return // ctx cancelled
			}
			lastErr = err
			if kube.IsNotFound(err) {
				continue // try the next API version
			}
			break
		}
		switch {
		case kube.IsNotFound(lastErr):
			r.set(t.Name(), func(s *Status) { s.State = "not-installed" })
			if t.Optional() {
				log.Debug("resource not installed, will check again later")
			} else {
				log.Warn("resource not found", "err", lastErr)
			}
		case kube.IsForbidden(lastErr):
			r.set(t.Name(), func(s *Status) { s.State = "forbidden" })
			log.Warn("no permission to list resource; check the ClusterRole", "err", lastErr)
		default:
			log.Warn("tracker stopped", "err", lastErr)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.Recheck):
		}
	}
}
