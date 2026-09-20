// Package client talks to a remote OpenMind server over its REST API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/store"
)

// Client is a thin REST client.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// New returns a client for base URL (scheme://host[:port]).
func New(base, token string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: http.DefaultClient}
}

// Do performs a JSON request against the API.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, e.Error)
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(data)
		return nil
	}
	return json.Unmarshal(data, out)
}

// Put creates or updates a note.
func (c *Client) Put(ctx context.Context, n *note.Note) (*note.Note, error) {
	var out note.Note
	if err := c.Do(ctx, "POST", "/api/v1/notes", n, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get fetches one note.
func (c *Client) Get(ctx context.Context, id string) (*note.Note, error) {
	var out note.Note
	if err := c.Do(ctx, "GET", "/api/v1/notes/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func query(f store.Filter, q string) string {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("q", q)
	set("project", f.Project)
	set("type", f.Type)
	set("scope", f.Scope)
	set("status", f.Status)
	set("tags", strings.Join(f.Tags, ","))
	if f.Limit > 0 {
		v.Set("limit", fmt.Sprint(f.Limit))
	}
	if !f.Since.IsZero() {
		v.Set("since", f.Since.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return v.Encode()
}

// List returns notes matching the filter.
func (c *Client) List(ctx context.Context, f store.Filter) ([]*note.Note, error) {
	var out []*note.Note
	err := c.Do(ctx, "GET", "/api/v1/notes?"+query(f, ""), nil, &out)
	return out, err
}

// Search returns ranked hits.
func (c *Client) Search(ctx context.Context, q string, f store.Filter) ([]store.Hit, error) {
	var out []store.Hit
	err := c.Do(ctx, "GET", "/api/v1/notes?"+query(f, q), nil, &out)
	return out, err
}

// Context returns the project digest.
func (c *Client) Context(ctx context.Context, project string) (string, error) {
	var out string
	err := c.Do(ctx, "GET", "/api/v1/context?project="+url.QueryEscape(project), nil, &out)
	return out, err
}

// Whoami returns the user the token maps to.
func (c *Client) Whoami(ctx context.Context) (string, error) {
	var out struct{ User string }
	err := c.Do(ctx, "GET", "/api/v1/whoami", nil, &out)
	return out.User, err
}
