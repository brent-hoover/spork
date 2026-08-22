package trackerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client speaks sutra's HTTP API. It is the only package that does.
//
// Every mutation carries an Idempotency-Key the caller supplies and persists
// BEFORE the call, so a crash in the window between sending and recording the
// outcome can be replayed under the same key and return the original result
// rather than acting twice.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client with a bounded timeout.
func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Identity is a sutra identity — the actor every mutation is recorded against.
type Identity struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
}

// Project is a sutra project.
type Project struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Issue is a sutra issue as read back.
type Issue struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// APIError is a non-2xx response, carrying enough to act on.
//
// Status is exposed because the difference between 409 and 500 is the
// difference between "already done" and "try again": a replayed mutation must
// be distinguishable from a tracker that is unwell.
type APIError struct {
	Status int
	Body   string
	Op     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sutra %s: status %d: %s", e.Op, e.Status, e.Body)
}

// do sends a request and decodes a 2xx JSON body into out.
func (c *Client) do(ctx context.Context, op, method, path, key string, body, out any) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", op, err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return fmt.Errorf("build %s: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("send %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return &APIError{Status: resp.StatusCode, Body: string(b), Op: op}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", op, err)
	}
	return nil
}

// CreateIdentity registers an actor. kind is "human" or "agent".
func (c *Client) CreateIdentity(ctx context.Context, handle, kind, displayName, key string) (Identity, error) {
	var id Identity
	err := c.do(ctx, "createIdentity", http.MethodPost, "/identities", key,
		map[string]string{"handle": handle, "kind": kind, "display_name": displayName}, &id)
	return id, err
}

// CreateProject creates a project.
func (c *Client) CreateProject(ctx context.Context, projectKey, name, actor, key string) (Project, error) {
	var p Project
	err := c.do(ctx, "createProject", http.MethodPost, "/projects", key,
		map[string]string{"key": projectKey, "name": name, "actor": actor}, &p)
	return p, err
}

// CreateIssue creates an issue in a project.
func (c *Client) CreateIssue(ctx context.Context, projectID, title, body, actor, key string) (Issue, error) {
	var i Issue
	payload := map[string]string{"title": title, "actor": actor}
	if body != "" {
		payload["body"] = body
	}
	err := c.do(ctx, "createIssue", http.MethodPost,
		"/projects/"+projectID+"/issues", key, payload, &i)
	return i, err
}

// AddRelation wires one issue to another. kind is "parent_of" or "blocks",
// and to is the issue on the receiving end.
func (c *Client) AddRelation(ctx context.Context, issueID, kind, to, actor, key string) error {
	return c.do(ctx, "addIssueRelation", http.MethodPost,
		"/issues/"+issueID+"/relations", key,
		map[string]string{"kind": kind, "to": to, "actor": actor}, nil)
}
