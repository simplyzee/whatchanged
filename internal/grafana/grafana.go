// Package grafana pushes changes to Grafana as annotations, so they appear
// on every time-series panel without any datasource setup.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

// Pusher sends annotations asynchronously. It never blocks the caller: if
// Grafana is slow or down, annotations are dropped and counted.
type Pusher struct {
	URL       string
	Token     string
	MinImpact change.Impact
	// ExtraTags are added to every annotation (e.g. the cluster name).
	ExtraTags []string
	Logger    *slog.Logger
	Client    *http.Client
	// OnResult is called after every attempt (for metrics).
	OnResult func(ok bool)

	queue chan change.Change
}

// Start launches the background sender.
func (p *Pusher) Start(ctx context.Context) {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	if p.MinImpact == "" {
		p.MinImpact = change.ImpactMedium
	}
	p.queue = make(chan change.Change, 1024)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-p.queue:
				err := p.send(ctx, c)
				if err != nil {
					p.Logger.Warn("grafana annotation failed", "err", err)
				}
				if p.OnResult != nil {
					p.OnResult(err == nil)
				}
			}
		}
	}()
}

// Enqueue queues a change if it is important enough. Safe to use as a
// store.OnAdd hook.
func (p *Pusher) Enqueue(c change.Change) {
	if p.queue == nil || c.IsSignal() || !c.Impact.AtLeast(p.MinImpact) {
		return
	}
	select {
	case p.queue <- c:
	default:
		if p.OnResult != nil {
			p.OnResult(false)
		}
	}
}

// Annotation is the Grafana annotation API payload.
type Annotation struct {
	Time int64    `json:"time"`
	Tags []string `json:"tags"`
	Text string   `json:"text"`
}

// FromChange renders a change as an annotation.
func FromChange(c change.Change, extra []string) Annotation {
	tags := []string{"whatchanged", c.Source, c.Action, string(c.Impact)}
	if c.Namespace != "" {
		tags = append(tags, "ns:"+c.Namespace)
	}
	tags = append(tags, extra...)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>", htmlEscape(c.Object()))
	if c.Namespace != "" {
		fmt.Fprintf(&b, " in %s", htmlEscape(c.Namespace))
	}
	fmt.Fprintf(&b, ": %s", htmlEscape(c.Summary))
	if c.Actor != "" {
		fmt.Fprintf(&b, " (by %s)", htmlEscape(c.Actor))
	}
	if c.URL != "" && (strings.HasPrefix(c.URL, "https://") || strings.HasPrefix(c.URL, "http://")) {
		fmt.Fprintf(&b, ` <a href="%s">link</a>`, htmlEscape(c.URL))
	}
	return Annotation{Time: c.Time.UnixMilli(), Tags: tags, Text: b.String()}
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func (p *Pusher) send(ctx context.Context, c change.Change) error {
	body, err := json.Marshal(FromChange(c, p.ExtraTags))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/api/annotations", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("grafana returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
