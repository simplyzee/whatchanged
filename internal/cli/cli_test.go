package cli

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/api"
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
