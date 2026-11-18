// Package api serves the whatchanged HTTP API and web UI.
package api

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/metrics"
	"github.com/simplyzee/whatchanged/internal/store"
	"github.com/simplyzee/whatchanged/internal/suspects"
	"github.com/simplyzee/whatchanged/internal/timeparse"
)

//go:embed ui/index.html
var uiFS embed.FS

const (
	defaultLimit  = 200
	maxLimit      = 5000
	maxIngestBody = 1 << 20
	maxIngestList = 100
)

// Server wires the HTTP API to the store.
type Server struct {
	Store       *store.Store
	IngestToken string
	Metrics     *metrics.Registry
	Logger      *slog.Logger
	// Ready reports whether initial sync has completed.
	Ready func() bool
	// Status returns extra info for /api/v1/status (e.g. tracker states).
	Status func() any
	Now    func() time.Time
	// Relations links Warning events to the changes that caused them.
	Relations suspects.Relations
	// Annotate adds risk flags to pushed changes.
	Annotate func(*change.Change)
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/changes", s.listChanges)
	mux.HandleFunc("GET /api/v1/changes/{id}", s.getChange)
	mux.HandleFunc("POST /api/v1/changes", s.ingest)
	mux.HandleFunc("GET /api/v1/suspects", s.suspects)
	mux.HandleFunc("GET /api/v1/grafana/annotations", s.annotations)
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if s.Metrics != nil {
			s.Metrics.Write(w)
		}
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		b, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		w.Write(b)
	})
	return mux
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, apiError{err.Error()})
}

// list reads a query parameter that may be repeated and/or comma separated.
func list(q map[string][]string, key string) []string {
	var out []string
	for _, v := range q[key] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// ParseFilter builds a store.Filter from query parameters.
func ParseFilter(r *http.Request, now time.Time) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{
		Namespaces: list(q, "namespace"),
		Kinds:      list(q, "kind"),
		Sources:    list(q, "source"),
		Actions:    list(q, "action"),
		Flags:      list(q, "flag"),
		Text:       q.Get("q"),
		Limit:      defaultLimit,
	}
	since := q.Get("since")
	if since == "" {
		since = "1h"
	}
	var err error
	if f.Since, err = timeparse.Point(since, now); err != nil {
		return f, err
	}
	if u := q.Get("until"); u != "" {
		if f.Until, err = timeparse.Point(u, now); err != nil {
			return f, err
		}
	}
	if v := q.Get("impact"); v != "" {
		if f.MinImpact, err = change.ParseImpact(v); err != nil {
			return f, err
		}
	}
	if v := q.Get("signals"); v == "false" || v == "0" {
		f.NoSignals = true
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return f, fmt.Errorf("invalid limit %q", v)
		}
		f.Limit = min(n, maxLimit)
	}
	return f, nil
}

// ChangesResponse is returned by GET /api/v1/changes.
type ChangesResponse struct {
	Since   time.Time       `json:"since"`
	Until   time.Time       `json:"until"`
	Count   int             `json:"count"`
	Changes []change.Change `json:"changes"`
}

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	now := s.Now()
	f, err := ParseFilter(r, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	res := s.Store.Query(f)
	if q := r.URL.Query().Get("diff"); q != "true" && q != "1" {
		for i := range res {
			res[i].Diff = nil // keep list payloads small; fetch one change for its diff
		}
	}
	until := f.Until
	if until.IsZero() {
		until = now
	}
	writeJSON(w, http.StatusOK, ChangesResponse{Since: f.Since.UTC(), Until: until.UTC(), Count: len(res), Changes: res})
}

// SuspectsResponse is returned by GET /api/v1/suspects.
type SuspectsResponse struct {
	At       time.Time          `json:"at"`
	Window   string             `json:"window"`
	Suspects []suspects.Suspect `json:"suspects"`
	Warnings int                `json:"warnings"`
}

func (s *Server) suspects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.Now()
	at, err := timeparse.Point(q.Get("at"), now)
	if err != nil {
		badRequest(w, err)
		return
	}
	window := time.Hour
	if v := q.Get("window"); v != "" {
		if window, err = timeparse.Duration(v); err != nil || window <= 0 {
			badRequest(w, fmt.Errorf("invalid window %q", v))
			return
		}
	}
	limit := 10
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			badRequest(w, fmt.Errorf("invalid limit %q", v))
			return
		}
	}
	// Look a little past `at` too, so we can see symptoms that followed.
	records := s.Store.Query(store.Filter{Since: at.Add(-window), Until: at.Add(15 * time.Minute)})
	warnings := 0
	for _, c := range records {
		if c.IsSignal() && !c.Time.After(at.Add(15*time.Minute)) {
			warnings++
		}
	}
	res := suspects.Rank(records, suspects.Query{At: at, Window: window, Namespaces: list(q, "namespace"), Limit: limit, Relations: s.Relations})
	if res == nil {
		res = []suspects.Suspect{}
	}
	writeJSON(w, http.StatusOK, SuspectsResponse{At: at.UTC(), Window: window.String(), Suspects: res, Warnings: warnings})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.IngestToken == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(s.IngestToken)) == 1
}

// IngestResponse is returned by POST /api/v1/changes.
type IngestResponse struct {
	Accepted int      `json:"accepted"`
	IDs      []string `json:"ids"`
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if s.IngestToken == "" {
		writeJSON(w, http.StatusForbidden, apiError{"ingest is disabled: start the server with --ingest-token-file or WHATCHANGED_INGEST_TOKEN"})
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="whatchanged"`)
		writeJSON(w, http.StatusUnauthorized, apiError{"missing or invalid bearer token"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIngestBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, apiError{"body too large"})
		return
	}
	var batch []change.Change
	trimmed := strings.TrimSpace(string(body))
	switch {
	case strings.HasPrefix(trimmed, "["):
		err = json.Unmarshal(body, &batch)
	default:
		var one change.Change
		err = json.Unmarshal(body, &one)
		batch = []change.Change{one}
	}
	if err != nil {
		badRequest(w, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	if len(batch) == 0 || len(batch) > maxIngestList {
		badRequest(w, fmt.Errorf("send between 1 and %d changes", maxIngestList))
		return
	}
	now := s.Now()
	resp := IngestResponse{IDs: []string{}}
	for i := range batch {
		c := &batch[i]
		if err := c.Normalize(now); err != nil {
			badRequest(w, fmt.Errorf("change %d: %w", i, err))
			return
		}
		if c.Time.After(now.Add(5 * time.Minute)) {
			badRequest(w, fmt.Errorf("change %d: time is in the future", i))
			return
		}
		if s.Annotate != nil {
			s.Annotate(c)
		}
	}
	for _, c := range batch {
		added, err := s.Store.Add(c)
		if err != nil {
			s.Logger.Error("persist change", "err", err)
		}
		if added {
			resp.Accepted++
			if s.Metrics != nil {
				s.Metrics.Inc("whatchanged_ingested_total", c.Source)
			}
		}
		resp.IDs = append(resp.IDs, c.ID)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// GrafanaAnnotation matches what the Grafana Infinity / JSON API datasources
// expect for annotation queries.
type GrafanaAnnotation struct {
	Time    int64    `json:"time"`
	TimeEnd int64    `json:"timeEnd"`
	Title   string   `json:"title"`
	Text    string   `json:"text"`
	Tags    []string `json:"tags"`
}

func (s *Server) annotations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.Now()
	// Grafana passes ${__from} / ${__to} as unix milliseconds.
	if q.Get("since") == "" && q.Get("from") != "" {
		q.Set("since", q.Get("from"))
	}
	if q.Get("until") == "" && q.Get("to") != "" {
		q.Set("until", q.Get("to"))
	}
	if q.Get("impact") == "" {
		q.Set("impact", "medium")
	}
	q.Set("signals", "false")
	r.URL.RawQuery = q.Encode()
	f, err := ParseFilter(r, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	if q.Get("limit") == "" {
		f.Limit = 1000
	}
	out := []GrafanaAnnotation{}
	for _, c := range s.Store.Query(f) {
		tags := []string{c.Source, c.Action}
		if c.Namespace != "" {
			tags = append(tags, c.Namespace)
		}
		text := c.Summary
		if c.Actor != "" {
			text += " (by " + c.Actor + ")"
		}
		ms := c.Time.UnixMilli()
		out = append(out, GrafanaAnnotation{Time: ms, TimeEnd: ms, Title: c.Object(), Text: text, Tags: tags})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	c, ok, err := s.Store.Get(r.PathValue("id"))
	switch {
	case err != nil:
		badRequest(w, err)
	case !ok:
		writeJSON(w, http.StatusNotFound, apiError{"no change with that id"})
	default:
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.Ready != nil && !s.Ready() {
		http.Error(w, "syncing", http.StatusServiceUnavailable)
		return
	}
	io.WriteString(w, "ok\n")
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"changes": s.Store.Len(), "ingest": s.IngestToken != ""}
	if s.Ready != nil {
		out["ready"] = s.Ready()
	}
	if s.Status != nil {
		out["trackers"] = s.Status()
	}
	writeJSON(w, http.StatusOK, out)
}
