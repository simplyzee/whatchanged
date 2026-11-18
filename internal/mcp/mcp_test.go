package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/api"
	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/client"
	"github.com/simplyzee/whatchanged/internal/store"
)

func setup(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	img := change.Change{ID: "aaaaaaaa1111", Time: now.Add(-10 * time.Minute), Source: "kubernetes", Kind: "Deployment",
		Namespace: "payments", Name: "api", Action: "image", Impact: change.ImpactHigh, Summary: "api: x:1 → 2",
		Actor: "kubectl-edit", Flags: []string{"manual"}, Diff: []string{`- spec.image: "x:1"`, `+ spec.image: "x:2"`}}
	warn := change.Change{ID: "bbbbbbbb2222", Time: now.Add(-8 * time.Minute), Source: "event", Kind: "Pod", Namespace: "payments",
		Name: "api-1", Action: change.ActionWarning, Impact: change.ImpactLow, Summary: "BackOff: crash"}
	st.Add(img)
	st.Add(warn)
	return st, (&api.Server{Store: st, IngestToken: "tok"}).Handler()
}

func rpc(t *testing.T, s *Server, allowWrite bool, method string, params any) map[string]any {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	out := s.Handle(context.Background(), msg, allowWrite)
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("%s: %v (%s)", method, err, out)
	}
	if resp["error"] != nil {
		t.Fatalf("%s: rpc error %v", method, resp["error"])
	}
	return resp["result"].(map[string]any)
}

func callText(t *testing.T, s *Server, allowWrite bool, name string, args map[string]any) (string, bool) {
	t.Helper()
	res := rpc(t, s, allowWrite, "tools/call", map[string]any{"name": name, "arguments": args})
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"].(bool)
}

func toolNames(res map[string]any) []string {
	var names []string
	for _, x := range res["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	return names
}

func TestToolsOverLocalClient(t *testing.T) {
	_, h := setup(t)
	s := &Server{Client: client.Local(h, "tok"), Version: "test"}

	init := rpc(t, s, false, "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("version negotiation: %v", init["protocolVersion"])
	}
	if names := toolNames(rpc(t, s, false, "tools/list", nil)); strings.Contains(strings.Join(names, ","), "record_change") {
		t.Errorf("record_change must be hidden without write access: %v", names)
	}
	if names := toolNames(rpc(t, s, true, "tools/list", nil)); !strings.Contains(strings.Join(names, ","), "record_change") {
		t.Errorf("record_change should be offered with write access: %v", names)
	}

	text, isErr := callText(t, s, false, "suspects", map[string]any{"namespace": "payments"})
	if isErr || !strings.Contains(text, "[score") || !strings.Contains(text, "Deployment/api: api: x:1 → 2") || !strings.Contains(text, "manual change (kubectl-edit)") {
		t.Errorf("suspects:\n%s", text)
	}
	text, _ = callText(t, s, false, "what_changed", map[string]any{"since": "1h", "signals": false})
	if !strings.Contains(text, "1 change(s)") || strings.Contains(text, "BackOff") || !strings.Contains(text, "aaaaaaaa") {
		t.Errorf("what_changed:\n%s", text)
	}
	text, _ = callText(t, s, false, "get_change", map[string]any{"id": "aaaaaaaa"})
	if !strings.Contains(text, `+ spec.image: "x:2"`) {
		t.Errorf("get_change:\n%s", text)
	}
	if text, isErr = callText(t, s, false, "get_change", map[string]any{"id": "nope-nope"}); !isErr {
		t.Errorf("missing change should be a tool error: %s", text)
	}
	if text, isErr = callText(t, s, false, "record_change", map[string]any{"source": "claude", "summary": "restarted api"}); !isErr {
		t.Errorf("record_change without write access must fail: %s", text)
	}
	text, isErr = callText(t, s, true, "record_change", map[string]any{"source": "claude", "summary": "restarted api", "namespace": "payments"})
	if isErr || !strings.HasPrefix(text, "recorded ") {
		t.Errorf("record_change: %s", text)
	}
	if out := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), false); out != nil {
		t.Errorf("notifications get no response, got %s", out)
	}
	if out := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":7,"method":"nope"}`), false); !strings.Contains(string(out), "-32601") {
		t.Errorf("unknown method: %s", out)
	}
}

func TestHTTPTransport(t *testing.T) {
	_, h := setup(t)
	s := &Server{Client: client.Local(h, "tok"), Version: "test"}
	hs := httptest.NewServer(s.HTTPHandler("tok"))
	defer hs.Close()

	post := func(body, token, origin string) (int, string) {
		req, _ := http.NewRequest("POST", hs.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if code, body := post(list, "", ""); code != 200 || strings.Contains(body, "record_change") {
		t.Errorf("anonymous list: %d %s", code, body)
	}
	if code, body := post(list, "tok", ""); code != 200 || !strings.Contains(body, "record_change") {
		t.Errorf("authenticated list: %d", code)
	}
	if code, _ := post(list, "", "https://evil.example.com"); code != http.StatusForbidden {
		t.Errorf("cross-origin request should be refused, got %d", code)
	}
	if code, _ := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", ""); code != http.StatusAccepted {
		t.Errorf("notification: %d", code)
	}
	resp, _ := http.Get(hs.URL)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}
}

func TestStdio(t *testing.T) {
	_, h := setup(t)
	s := &Server{Client: client.Local(h, ""), Version: "test"}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}

{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`)
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) || !strings.Contains(lines[1], `"suspects"`) {
		t.Errorf("stdio output:\n%s", out.String())
	}
	if strings.Contains(lines[1], "record_change") {
		t.Error("no token: record_change must not be listed")
	}
}
