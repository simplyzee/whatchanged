// Package client talks to the whatchanged HTTP API, over the network or
// in-process.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/simplyzee/whatchanged/internal/api"
	"github.com/simplyzee/whatchanged/internal/change"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
)

// Client calls the API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the API at base.
func New(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// Local returns a client that calls h in-process (no network).
func Local(h http.Handler, token string) *Client {
	return &Client{base: "http://local", token: token, http: &http.Client{Transport: handlerTransport{h}}}
}

// CanWrite reports whether the client has a token for pushing changes.
func (c *Client) CanWrite() bool { return c.token != "" }

// Base returns the API address.
func (c *Client) Base() string { return c.base }

type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// Do performs a request and decodes the JSON response into out.
func (c *Client) Do(method, path string, q url.Values, body any, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach whatchanged at %s: %w\n(set --server or WHATCHANGED_URL; for an in-cluster install try: kubectl -n whatchanged port-forward svc/whatchanged 8080)", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &HTTPError{resp.StatusCode, "server: " + e.Error}
		}
		return &HTTPError{resp.StatusCode, "server returned " + resp.Status}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Changes lists changes.
func (c *Client) Changes(q url.Values) (api.ChangesResponse, error) {
	var res api.ChangesResponse
	err := c.Do("GET", "/api/v1/changes", q, nil, &res)
	return res, err
}

// Suspects ranks suspects.
func (c *Client) Suspects(q url.Values) (api.SuspectsResponse, error) {
	var res api.SuspectsResponse
	err := c.Do("GET", "/api/v1/suspects", q, nil, &res)
	return res, err
}

// Change fetches one change (with its diff) by ID or ID prefix.
func (c *Client) Change(id string) (change.Change, error) {
	var res change.Change
	err := c.Do("GET", "/api/v1/changes/"+url.PathEscape(id), nil, nil, &res)
	return res, err
}

// Status returns server and tracker status.
func (c *Client) Status() (map[string]any, error) {
	var res map[string]any
	err := c.Do("GET", "/api/v1/status", nil, nil, &res)
	return res, err
}

// Push records a change.
func (c *Client) Push(ch change.Change) (api.IngestResponse, error) {
	var res api.IngestResponse
	err := c.Do("POST", "/api/v1/changes", nil, ch, &res)
	return res, err
}
