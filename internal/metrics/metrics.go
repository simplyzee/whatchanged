// Package metrics is a tiny Prometheus text-format exporter, enough for a
// handful of counters and gauges without pulling in client_golang.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry holds metrics.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*counterVec
	gauges   map[string]gauge
	order    []string
}

type counterVec struct {
	help   string
	labels []string
	values map[string]float64 // joined label values -> value
}

type gauge struct {
	help string
	fn   func() float64
}

// New creates a registry.
func New() *Registry {
	return &Registry{counters: map[string]*counterVec{}, gauges: map[string]gauge{}}
}

// Counter registers a counter with the given label names.
func (r *Registry) Counter(name, help string, labels ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] = &counterVec{help: help, labels: labels, values: map[string]float64{}}
	r.order = append(r.order, name)
}

// Gauge registers a gauge whose value is read at scrape time.
func (r *Registry) Gauge(name, help string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = gauge{help, fn}
	r.order = append(r.order, name)
}

// Inc adds 1 to a counter. Label values must match the registered names.
func (r *Registry) Inc(name string, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok || len(labelValues) != len(c.labels) {
		return
	}
	c.values[strings.Join(labelValues, "\x00")]++
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// Write renders the Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range r.order {
		if c, ok := r.counters[name]; ok {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, c.help, name)
			keys := make([]string, 0, len(c.values))
			for k := range c.values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				vals := strings.Split(k, "\x00")
				var pairs []string
				for i, l := range c.labels {
					pairs = append(pairs, fmt.Sprintf(`%s="%s"`, l, escape(vals[i])))
				}
				if len(pairs) > 0 {
					fmt.Fprintf(w, "%s{%s} %g\n", name, strings.Join(pairs, ","), c.values[k])
				} else {
					fmt.Fprintf(w, "%s %g\n", name, c.values[k])
				}
			}
			continue
		}
		g := r.gauges[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, g.help, name, name, g.fn())
	}
}
