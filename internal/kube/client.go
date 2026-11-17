// Package kube is a small, dependency-free Kubernetes API client that knows
// how to list and watch resources. It deliberately avoids client-go so the
// binary stays tiny and the dependency graph stays empty.
package kube

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	saDir          = "/var/run/secrets/kubernetes.io/serviceaccount"
	acceptMetaList = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1,application/json"
	acceptMeta     = "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1,application/json"
)

// Client talks to a Kubernetes API server.
type Client struct {
	base      *url.URL
	http      *http.Client
	tokenFile string
	token     string

	mu        sync.Mutex
	cached    string
	cachedAt  time.Time
	userAgent string
}

// Config describes how to reach the API server.
type Config struct {
	// Server is the API server URL, e.g. https://10.0.0.1:443 or
	// http://127.0.0.1:8001 (kubectl proxy). Empty means in-cluster.
	Server string
	// TokenFile is read on every refresh so rotated tokens are picked up.
	TokenFile string
	// Token is a static bearer token (TokenFile wins if both are set).
	Token string
	// CAFile is a PEM bundle used to verify the server.
	CAFile string
	// Insecure skips TLS verification. Only for local development.
	Insecure  bool
	UserAgent string
}

// New builds a client. With an empty Server it uses the in-cluster
// service account.
func New(cfg Config) (*Client, error) {
	if cfg.Server == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("not running in a cluster: set --api-server (tip: run `kubectl proxy` and use http://127.0.0.1:8001)")
		}
		cfg.Server = "https://" + net.JoinHostPort(host, port)
		if cfg.TokenFile == "" && cfg.Token == "" {
			cfg.TokenFile = saDir + "/token"
		}
		if cfg.CAFile == "" {
			cfg.CAFile = saDir + "/ca.crt"
		}
	}
	u, err := url.Parse(strings.TrimRight(cfg.Server, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid api server url %q", cfg.Server)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.Insecure} //nolint:gosec // opt-in for dev
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil // the API server is reached directly
	ua := cfg.UserAgent
	if ua == "" {
		ua = "whatchanged"
	}
	return &Client{
		base:      u,
		http:      &http.Client{Transport: tr},
		tokenFile: cfg.TokenFile,
		token:     cfg.Token,
		userAgent: ua,
	}, nil
}

func (c *Client) bearer() (string, error) {
	if c.tokenFile == "" {
		return c.token, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" && time.Since(c.cachedAt) < time.Minute {
		return c.cached, nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	c.cached, c.cachedAt = strings.TrimSpace(string(b)), time.Now()
	return c.cached, nil
}

// Resource identifies a collection to list and watch.
type Resource struct {
	// Path is the collection path, e.g. /apis/apps/v1/deployments.
	Path          string
	LabelSelector string
	FieldSelector string
	// MetadataOnly asks the server for PartialObjectMetadata, so object
	// bodies (e.g. Secret data) are never transferred.
	MetadataOnly bool
}

// StatusError is a non-2xx response or a watch ERROR event.
type StatusError struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kubernetes api: %d %s: %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("kubernetes api: %d %s", e.Code, e.Reason)
}

func codeOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// IsNotFound reports a 404, typically a CRD that is not installed.
func IsNotFound(err error) bool { return codeOf(err) == http.StatusNotFound }

// IsGone reports a 410: the resourceVersion is too old and a relist is needed.
func IsGone(err error) bool { return codeOf(err) == http.StatusGone }

// IsForbidden reports a 403, usually missing RBAC.
func IsForbidden(err error) bool { return codeOf(err) == http.StatusForbidden }

func (c *Client) do(ctx context.Context, r Resource, q url.Values, accept string) (*http.Response, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + r.Path
	if r.LabelSelector != "" {
		q.Set("labelSelector", r.LabelSelector)
	}
	if r.FieldSelector != "" {
		q.Set("fieldSelector", r.FieldSelector)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.userAgent)
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		se := &StatusError{}
		if json.Unmarshal(body, se) != nil || se.Code == 0 {
			se = &StatusError{Message: strings.TrimSpace(string(body))}
		}
		se.Code = resp.StatusCode
		if se.Reason == "" {
			se.Reason = http.StatusText(resp.StatusCode)
		}
		return nil, se
	}
	return resp, nil
}

// ListResult is one page-merged list response.
type ListResult struct {
	Items           []json.RawMessage
	ResourceVersion string
}

// List returns every item in the collection, following continue tokens.
func (c *Client) List(ctx context.Context, r Resource) (ListResult, error) {
	accept := "application/json"
	if r.MetadataOnly {
		accept = acceptMetaList
	}
	var out ListResult
	cont := ""
	for {
		q := url.Values{"limit": {"500"}}
		if cont != "" {
			q.Set("continue", cont)
		}
		// A list should never take this long; don't let a stalled API
		// server wedge the tracker forever.
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		resp, err := c.do(pctx, r, q, accept)
		if err != nil {
			cancel()
			return ListResult{}, err
		}
		var page struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
				Continue        string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		cancel()
		if err != nil {
			return ListResult{}, fmt.Errorf("decode list %s: %w", r.Path, err)
		}
		out.Items = append(out.Items, page.Items...)
		out.ResourceVersion = page.Metadata.ResourceVersion
		if page.Metadata.Continue == "" {
			return out, nil
		}
		cont = page.Metadata.Continue
	}
}

// Event is one watch notification.
type Event struct {
	Type   string // ADDED, MODIFIED, DELETED, BOOKMARK
	Object json.RawMessage
}

// Watch streams events from resourceVersion until the server closes the
// stream, ctx ends, or fn returns an error. A server-side ERROR event is
// returned as a *StatusError (410 means: relist).
func (c *Client) Watch(ctx context.Context, r Resource, resourceVersion string, timeout time.Duration, fn func(Event) error) error {
	accept := "application/json"
	if r.MetadataOnly {
		accept = acceptMeta
	}
	q := url.Values{
		"watch":               {"true"},
		"allowWatchBookmarks": {"true"},
		"resourceVersion":     {resourceVersion},
	}
	if timeout > 0 {
		q.Set("timeoutSeconds", fmt.Sprint(int(timeout.Seconds())))
	}
	resp, err := c.do(ctx, r, q, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 64<<10))
	for {
		var ev Event
		var raw struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("watch %s: %w", r.Path, err)
		}
		if raw.Type == "ERROR" {
			se := &StatusError{}
			if json.Unmarshal(raw.Object, se) != nil || se.Code == 0 {
				se.Code = http.StatusInternalServerError
			}
			return se
		}
		ev.Type, ev.Object = raw.Type, raw.Object
		if err := fn(ev); err != nil {
			return err
		}
	}
}
