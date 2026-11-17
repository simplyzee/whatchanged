package grafana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

func TestPusher(t *testing.T) {
	got := make(chan Annotation, 4)
	var auth string
	gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/annotations" || r.Method != "POST" {
			http.Error(w, "nope", 404)
			return
		}
		auth = r.Header.Get("Authorization")
		var a Annotation
		json.NewDecoder(r.Body).Decode(&a)
		got <- a
	}))
	defer gs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &Pusher{URL: gs.URL, Token: "glsa_x", ExtraTags: []string{"cluster:prod"}}
	p.Start(ctx)
	base := change.Change{Time: time.UnixMilli(1763404200000), Source: "kubernetes", Kind: "Deployment", Namespace: "payments", Name: "api"}

	low := base
	low.Impact, low.Action = change.ImpactLow, "scale"
	warn := base
	warn.Impact, warn.Action = change.ImpactHigh, change.ActionWarning
	high := base
	high.Impact, high.Action, high.Summary, high.Actor = change.ImpactHigh, "image", "api: x:1 → <2>", "kubectl"
	p.Enqueue(low)
	p.Enqueue(warn)
	p.Enqueue(high)

	select {
	case a := <-got:
		if a.Time != 1763404200000 || auth != "Bearer glsa_x" {
			t.Errorf("annotation %+v auth %q", a, auth)
		}
		if !strings.Contains(a.Text, "&lt;2&gt;") || !strings.Contains(a.Text, "(by kubectl)") {
			t.Errorf("text not escaped/complete: %q", a.Text)
		}
		tags := strings.Join(a.Tags, ",")
		if !strings.Contains(tags, "ns:payments") || !strings.Contains(tags, "cluster:prod") {
			t.Errorf("tags = %v", a.Tags)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no annotation pushed")
	}
	select {
	case a := <-got:
		t.Errorf("only the high-impact change should be pushed, also got %+v", a)
	case <-time.After(200 * time.Millisecond):
	}
}
