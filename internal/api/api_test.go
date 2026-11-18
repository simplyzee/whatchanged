package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/store"
)

func newServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	st, err := store.Open(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, IngestToken: token, Now: func() time.Time { return time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC) }}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return hs
}

func post(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/api/v1/changes", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestIngest(t *testing.T) {
	off := newServer(t, "")
	if code, _ := post(t, off.URL, "anything", `{"source":"ci","name":"x"}`); code != http.StatusForbidden {
		t.Errorf("ingest without configured token should be disabled, got %d", code)
	}

	hs := newServer(t, "tok")
	cases := []struct {
		token, body string
		code        int
		contains    string
	}{
		{"", `{"source":"ci","name":"x"}`, 401, "bearer"},
		{"wrong", `{"source":"ci","name":"x"}`, 401, ""},
		{"tok", `{"name":"x"}`, 400, "source is required"},
		{"tok", `{"source":"ci","name":"x","impact":"catastrophic"}`, 400, "unknown impact"},
		{"tok", `{"source":"ci","name":"x","time":"2030-01-01T00:00:00Z"}`, 400, "future"},
		{"tok", `not json`, 400, "invalid JSON"},
		{"tok", `[]`, 400, "between 1 and"},
		{"tok", `{"source":"terraform","name":"vpc","summary":"apply: 3 changed","time":"2026-11-17T18:50:00Z"}`, 201, `"accepted": 1`},
		{"tok", `{"source":"terraform","name":"vpc","summary":"apply: 3 changed","time":"2026-11-17T18:50:00Z"}`, 201, `"accepted": 0`},
		{"tok", `[{"source":"flags","name":"new-checkout","action":"flag"},{"source":"ci","summary":"deploy"}]`, 201, `"accepted": 2`},
	}
	for i, c := range cases {
		code, body := post(t, hs.URL, c.token, c.body)
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("case %d: got %d %s, want %d containing %q", i, code, body, c.code, c.contains)
		}
	}
	resp, _ := http.Get(hs.URL + "/api/v1/changes?since=1h&source=terraform")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"count": 1`) || !strings.Contains(string(b), `"impact": "medium"`) {
		t.Errorf("list after ingest: %s", b)
	}
}

func TestBadQueries(t *testing.T) {
	hs := newServer(t, "")
	for _, q := range []string{
		"/api/v1/changes?since=yesterday",
		"/api/v1/changes?limit=0",
		"/api/v1/changes?impact=huge",
		"/api/v1/suspects?window=-1h",
		"/api/v1/suspects?at=tomorrow",
	} {
		resp, err := http.Get(hs.URL + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: got %d, want 400", q, resp.StatusCode)
		}
	}
	for _, p := range []string{"/", "/healthz", "/readyz", "/metrics", "/api/v1/status"} {
		resp, err := http.Get(hs.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestGetChangeAndDiffOmission(t *testing.T) {
	st, _ := store.Open(store.Options{})
	now := time.Now().UTC()
	st.Add(change.Change{ID: "abcdef999", Time: now.Add(-time.Minute), Source: "kubernetes", Name: "api", Action: "image",
		Impact: change.ImpactHigh, Flags: []string{"manual"}, Diff: []string{"- a: 1", "+ a: 2"}})
	hs := httptest.NewServer((&Server{Store: st}).Handler())
	defer hs.Close()
	get := func(p string) (int, string) {
		resp, err := http.Get(hs.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/api/v1/changes?since=1h"); code != 200 || strings.Contains(body, `"diff"`) {
		t.Errorf("list should omit diffs: %s", body)
	}
	if _, body := get("/api/v1/changes?since=1h&diff=true"); !strings.Contains(body, `"+ a: 2"`) {
		t.Errorf("diff=true should include diffs: %s", body)
	}
	if code, body := get("/api/v1/changes/abcdef"); code != 200 || !strings.Contains(body, `"+ a: 2"`) {
		t.Errorf("get by prefix: %d %s", code, body)
	}
	if code, _ := get("/api/v1/changes/zzzzzzzz"); code != 404 {
		t.Errorf("missing: %d", code)
	}
	if _, body := get("/api/v1/changes?since=1h&flag=freeze"); !strings.Contains(body, `"count": 0`) {
		t.Errorf("flag filter: %s", body)
	}
}
