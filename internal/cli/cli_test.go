package cli

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/api"
	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/store"
)

func TestPushListSuspects(t *testing.T) {
	st, _ := store.Open(store.Options{})
	hs := httptest.NewServer((&api.Server{Store: st, IngestToken: "tok"}).Handler())
	defer hs.Close()
	t.Setenv("WHATCHANGED_URL", hs.URL)
	t.Setenv("WHATCHANGED_TOKEN", "tok")
	t.Setenv("GITHUB_ACTOR", "")

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb)
		return code, out.String(), errb.String()
	}

	at := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	code, out, errs := run("push", "--source", "github-actions", "--name", "api", "-n", "payments",
		"--summary", "deploy v1.4.2", "--impact", "high", "--actor", "zee", "--label", "sha=abc123", "--at", at)
	if code != 0 || !strings.HasPrefix(out, "recorded:") {
		t.Fatalf("push: %d %q %q", code, out, errs)
	}
	if code, out, _ = run("push", "--source", "github-actions", "--name", "api", "-n", "payments",
		"--summary", "deploy v1.4.2", "--impact", "high", "--actor", "zee", "--at", at); code != 0 || !strings.HasPrefix(out, "already recorded") {
		t.Errorf("duplicate push: %d %q", code, out)
	}

	code, out, _ = run("--since", "30m", "-n", "payments")
	if code != 0 || !strings.Contains(out, "deploy v1.4.2") || !strings.Contains(out, "OBJECT") {
		t.Errorf("list: %d\n%s", code, out)
	}
	code, out, _ = run("list", "-n", "other")
	if code != 0 || !strings.Contains(out, "Nothing changed") {
		t.Errorf("empty list: %d %q", code, out)
	}
	code, out, _ = run("suspects", "-n", "payments")
	if code != 0 || !strings.Contains(out, "Most likely culprits") || !strings.Contains(out, "#1 api") {
		t.Errorf("suspects: %d\n%s", code, out)
	}
	code, out, _ = run("list", "-o", "json")
	if code != 0 || !strings.Contains(out, `"actor": "zee"`) {
		t.Errorf("json: %d %s", code, out)
	}
}

func TestErrorsAndHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("unknown command exit = %d", code)
	}
	t.Setenv("WHATCHANGED_TOKEN", "")
	errb.Reset()
	if code := Main([]string{"push", "--source", "ci", "--name", "x"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "token is required") {
		t.Errorf("push without token: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := Main([]string{"list", "--server", "http://127.0.0.1:1"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "cannot reach") {
		t.Errorf("unreachable: %d %q", code, errb.String())
	}
	out.Reset()
	if code := Main([]string{"trackers"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "secrets") {
		t.Errorf("trackers: %q", out.String())
	}
}

func TestPortForwardShowAndMCP(t *testing.T) {
	st, _ := store.Open(store.Options{})
	st.Add(change.Change{ID: "abcdef0123456789", Time: time.Now().Add(-time.Minute), Source: "kubernetes", Kind: "Deployment",
		Namespace: "payments", Name: "api", Action: "image", Impact: change.ImpactHigh, Summary: "api: x:1 → 2",
		Diff: []string{`- spec.image: "x:1"`, `+ spec.image: "x:2"`}})
	hs := httptest.NewServer((&api.Server{Store: st}).Handler())
	defer hs.Close()
	port := hs.URL[strings.LastIndex(hs.URL, ":")+1:]

	// A fake kubectl that records its args and pretends to port-forward.
	dir := t.TempDir()
	script := filepath.Join(dir, "kubectl")
	argsFile := filepath.Join(dir, "args")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\necho \"Forwarding from 127.0.0.1:"+port+" -> 8080\"\nexec sleep 30\n"), 0o755)
	t.Setenv("KUBECTL", script)
	t.Setenv("WHATCHANGED_URL", "")

	Plugin = true
	defer func() { Plugin = false }()
	var out, errb bytes.Buffer
	if code := Main([]string{"--context", "prod", "-n", "payments", "-o", "wide"}, &out, &errb); code != 0 {
		t.Fatalf("plugin list: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "abcdef01") || !strings.Contains(out.String(), "ID") {
		t.Errorf("wide list should show short IDs:\n%s", out.String())
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "port-forward --address 127.0.0.1 -n whatchanged svc/whatchanged :8080 --context prod") {
		t.Errorf("kubectl args: %s", args)
	}

	out.Reset()
	if code := Main([]string{"show", "abcdef01"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `+ spec.image: "x:2"`) {
		t.Errorf("show: %d\n%s%s", code, out.String(), errb.String())
	}
	out.Reset()
	Main([]string{"help"}, &out, &errb)
	if !strings.Contains(out.String(), "kubectl whatchanged suspects") {
		t.Errorf("plugin usage should say kubectl whatchanged:\n%s", out.String())
	}

	// mcp over stdio through the same port-forward
	Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_change","arguments":{"id":"abcdef01"}}}` + "\n")
	defer func() { Stdin = os.Stdin }()
	out.Reset()
	if code := Main([]string{"mcp"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `x:2`) {
		t.Errorf("mcp: %d %s %s", code, out.String(), errb.String())
	}

	// A failing port-forward explains itself.
	os.WriteFile(script, []byte("#!/bin/sh\necho 'Error from server (NotFound): services \"whatchanged\" not found' >&2\nexit 1\n"), 0o755)
	errb.Reset()
	if code := Main([]string{"list"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "is whatchanged installed?") {
		t.Errorf("failed port-forward: %d %s", code, errb.String())
	}
}
