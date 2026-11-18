// Package mcp exposes whatchanged to AI agents as a Model Context Protocol
// server, over stdio (`whatchanged mcp`) or streamable HTTP (POST /mcp on
// the server).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/client"
	"github.com/simplyzee/whatchanged/internal/render"
)

// Supported protocol versions, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Server answers MCP requests using an API client.
type Server struct {
	Client  *client.Client
	Version string
	Logger  *slog.Logger
	Now     func() time.Time
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Handle processes one JSON-RPC message. It returns nil for notifications.
// allowWrite controls whether record_change is offered.
func (s *Server) Handle(ctx context.Context, msg []byte, allowWrite bool) []byte {
	var req request
	if err := json.Unmarshal(msg, &req); err != nil {
		return mustJSON(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
	}
	if len(req.ID) == 0 {
		return nil // notification (e.g. notifications/initialized)
	}
	res, rerr := s.dispatch(ctx, req, allowWrite)
	out := response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		out.Error = rerr
	} else {
		out.Result = res
	}
	return mustJSON(out)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) dispatch(_ context.Context, req request, allowWrite bool) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := protocolVersions[0]
		for _, pv := range protocolVersions {
			if pv == p.ProtocolVersion {
				v = pv
			}
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "whatchanged", "title": "whatchanged", "version": s.Version},
			"instructions": "whatchanged records everything that changed in a Kubernetes cluster (rollouts, config, Helm, GitOps syncs, " +
				"RBAC, network policies, nodes…) plus changes pushed from CI and other tools. During an incident, call suspects " +
				"with the affected namespace first, then what_changed for context and get_change for the diff of a specific change.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools(allowWrite)}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		return s.call(p.Name, p.Arguments, allowWrite), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

func tools(allowWrite bool) []tool {
	ts := []tool{
		{
			Name:  "suspects",
			Title: "Rank likely culprits",
			Description: "Rank the changes most likely to have caused a problem that started at a given time, with the reasons " +
				"for each score (impact, recency, whether the affected workload's pods then failed, risk flags such as manual or " +
				"off-hours changes). Start here during an incident.",
			InputSchema: obj(map[string]any{
				"namespace": str("Namespace(s) where the problem is, comma separated. Strongly recommended."),
				"at":        str("When the problem started: 'now' (default), a relative time like '10m' (ago), or RFC 3339."),
				"window":    str("How far before 'at' to look, e.g. '1h' (default), '30m', '6h'."),
				"limit":     map[string]any{"type": "integer", "description": "Maximum suspects (default 10)."},
			}),
			Annotations: readOnly,
		},
		{
			Name:  "what_changed",
			Title: "List recent changes",
			Description: "List changes (newest first) with optional filters. Includes Warning events as signals unless " +
				"signals=false. Use get_change for the full diff of one change.",
			InputSchema: obj(map[string]any{
				"since":     str("How far back: '30m', '2h', '1d' or RFC 3339. Default '1h'."),
				"until":     str("End of range (default now)."),
				"namespace": str("Namespace(s), comma separated."),
				"kind":      str("Kind(s), e.g. 'Deployment,ConfigMap'."),
				"source":    str("Source(s), e.g. 'kubernetes,helm,argocd,github-actions'."),
				"action":    str("Action(s), e.g. 'image,config,release,sync'."),
				"impact":    str("Minimum impact: low, medium or high."),
				"flag":      str("Only changes with these flags: manual, off-hours, freeze."),
				"query":     str("Free-text search over names, summaries and actors."),
				"signals":   map[string]any{"type": "boolean", "description": "Include Warning events (default true)."},
				"limit":     map[string]any{"type": "integer", "description": "Maximum results (default 50)."},
			}),
			Annotations: readOnly,
		},
		{
			Name:        "get_change",
			Title:       "Show one change",
			Description: "Get a single change by ID (or the 8-character ID prefix shown in lists), including its redacted diff.",
			InputSchema: obj(map[string]any{"id": str("Change ID or ID prefix.")}, "id"),
			Annotations: readOnly,
		},
		{
			Name:        "tracker_status",
			Title:       "Tracker status",
			Description: "Show which change sources are being watched (synced), not installed in this cluster, or forbidden by RBAC.",
			InputSchema: obj(map[string]any{}),
			Annotations: readOnly,
		},
	}
	if allowWrite {
		ts = append(ts, tool{
			Name:  "record_change",
			Title: "Record a change",
			Description: "Record a change you (the agent) or someone else made, so it appears on the timeline and in suspects. " +
				"Use it after taking an action such as restarting a workload, changing a flag or running a migration.",
			InputSchema: obj(map[string]any{
				"source":    str("Who/what made the change, e.g. 'claude', 'terraform', 'launchdarkly'."),
				"name":      str("What changed, e.g. a service, stack or flag name."),
				"summary":   str("One line describing the change."),
				"namespace": str("Namespace it affects, if any."),
				"kind":      str("Kind of object, e.g. 'Deployment', 'FeatureFlag'."),
				"action":    str("e.g. deploy, config, restart, flag, migration."),
				"impact":    str("low, medium (default) or high."),
				"actor":     str("Person or agent responsible."),
				"url":       str("Link to the PR, run or ticket."),
			}, "source", "summary"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		})
	}
	return ts
}

func argStr(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func textResult(text string, structured any, isErr bool) map[string]any {
	r := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	if structured != nil {
		r["structuredContent"] = structured
	}
	return r
}

func errResult(err error) map[string]any { return textResult("error: "+err.Error(), nil, true) }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) call(name string, a map[string]any, allowWrite bool) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	var buf bytes.Buffer
	switch name {
	case "suspects":
		q := url.Values{}
		for _, k := range []string{"namespace", "at", "window", "limit"} {
			if v := argStr(a, k); v != "" {
				q.Set(k, v)
			}
		}
		res, err := s.Client.Suspects(q)
		if err != nil {
			return errResult(err)
		}
		if len(res.Suspects) == 0 {
			return textResult(fmt.Sprintf("No changes in the %s before %s. This is probably not caused by a recorded change.", res.Window, res.At.Format(time.RFC3339)), res, false)
		}
		fmt.Fprintf(&buf, "Suspects for a problem at %s (window %s, %d warning event(s)):\n\n", res.At.Format(time.RFC3339), res.Window, res.Warnings)
		for i, sp := range res.Suspects {
			obj := sp.Object()
			if sp.Namespace != "" {
				obj = sp.Namespace + "/" + obj
			}
			fmt.Fprintf(&buf, "%d. [score %d] %s: %s\n   id=%s source=%s actor=%s %s ago\n   why: %s\n",
				i+1, sp.Score, obj, sp.Summary,
				render.ShortID(sp.ID), sp.Source, orDash(sp.Actor), render.Age(sp.Time, s.now()), strings.Join(sp.Reasons, "; "))
		}
		return textResult(buf.String(), res, false)
	case "what_changed":
		q := url.Values{"limit": {"50"}}
		for k, p := range map[string]string{"since": "since", "until": "until", "namespace": "namespace", "kind": "kind",
			"source": "source", "action": "action", "impact": "impact", "flag": "flag", "query": "q", "limit": "limit"} {
			if v := argStr(a, k); v != "" {
				q.Set(p, v)
			}
		}
		if argStr(a, "signals") == "false" {
			q.Set("signals", "false")
		}
		res, err := s.Client.Changes(q)
		if err != nil {
			return errResult(err)
		}
		if res.Count == 0 {
			return textResult("Nothing changed since "+res.Since.Format(time.RFC3339)+".", res, false)
		}
		fmt.Fprintf(&buf, "%d change(s) since %s:\n\n", res.Count, res.Since.Format(time.RFC3339))
		render.Changes(&buf, res.Changes, render.Options{Wide: true, Now: s.now()})
		return textResult(buf.String(), res, false)
	case "get_change":
		id := argStr(a, "id")
		if id == "" {
			return errResult(errors.New("id is required"))
		}
		c, err := s.Client.Change(id)
		if err != nil {
			return errResult(err)
		}
		render.Detail(&buf, c, false)
		return textResult(buf.String(), c, false)
	case "tracker_status":
		st, err := s.Client.Status()
		if err != nil {
			return errResult(err)
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		return textResult(string(b), st, false)
	case "record_change":
		if !allowWrite || !s.Client.CanWrite() {
			return errResult(errors.New("recording changes is not enabled for this connection"))
		}
		c := change.Change{
			Source: argStr(a, "source"), Name: argStr(a, "name"), Summary: argStr(a, "summary"),
			Namespace: argStr(a, "namespace"), Kind: argStr(a, "kind"), Action: argStr(a, "action"),
			Impact: change.Impact(argStr(a, "impact")), Actor: argStr(a, "actor"), URL: argStr(a, "url"),
		}
		res, err := s.Client.Push(c)
		if err != nil {
			return errResult(err)
		}
		msg := "recorded " + strings.Join(res.IDs, ", ")
		if res.Accepted == 0 {
			msg = "already recorded " + strings.Join(res.IDs, ", ")
		}
		return textResult(msg, res, false)
	}
	return errResult(fmt.Errorf("unknown tool %q", name))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ServeStdio runs the server over newline-delimited JSON on r/w until r ends.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var mu sync.Mutex
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		out := s.Handle(ctx, line, s.Client.CanWrite())
		if out == nil {
			continue
		}
		mu.Lock()
		_, err := w.Write(append(out, '\n'))
		mu.Unlock()
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

// HTTPHandler serves MCP over streamable HTTP (JSON responses, no SSE).
// record_change is only offered to callers presenting the ingest token.
func (s *Server) HTTPHandler(ingestToken string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
		case http.MethodGet:
			// We never push server-initiated messages.
			w.Header().Set("Allow", "POST")
			http.Error(w, "this MCP server does not offer an SSE stream", http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// DNS-rebinding protection: browsers send Origin; only same-host is OK.
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		allowWrite := false
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && ingestToken != "" {
			allowWrite = subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(ingestToken)) == 1
		}
		out := s.Handle(r.Context(), body, allowWrite)
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	})
}
