package trackerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// do sends a keyed mutation and decodes a 2xx JSON body into out.
//
// Every call this client makes is a keyed mutation with a JSON body — there is
// no read path — so the body and the key are unconditional. Guarding them
// would be validation for a shape no caller has, and the day a read lands it
// will want its own helper rather than a branch inside this one.
func (c *Client) do(ctx context.Context, op, method, path, key string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)

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

// Thread is a sutra thread as read back.
type Thread struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Session string `json:"session"`
}

// ImportThread puts an agent transcript into the thread catalog.
//
// The key is the caller's and is persisted before the call: sutra returns the
// ORIGINAL thread for a replayed key rather than creating a second one, which
// is what makes exactly one thread exist per session across a crash.
func (c *Client) ImportThread(
	ctx context.Context, title string, transcript json.RawMessage,
	session, issue, actor, key string,
) (Thread, error) {
	payload := map[string]any{
		"title": title, "transcript": transcript, "actor": actor,
	}
	// Omitted rather than sent empty: sutra distinguishes an absent anchor
	// from a blank one, and a blank issue id is not a thread anchored nowhere.
	if session != "" {
		payload["session"] = session
	}
	if issue != "" {
		payload["issue"] = issue
	}
	var t Thread
	err := c.do(ctx, "importThread", http.MethodPost, "/threads", key, payload, &t)
	return t, err
}

// Review is a sutra review as read back.
type Review struct {
	ID       string `json:"id"`
	Issue    string `json:"issue"`
	State    string `json:"state"`
	Revision int    `json:"revision"`
	Session  string `json:"session"`
	Commit   string `json:"commit"`
	// LatestVerdictEvent identifies the verdict a resubmission answers. It is
	// a FENCE: a rework that named an older event would be answering a verdict
	// the human has since replaced.
	LatestVerdictEvent string `json:"latest_verdict_event"`
}

// CreateReview opens a review over a branch pinned at a commit.
//
// The key is the caller's and is persisted before the call. sutra returns the
// ORIGINAL review for a replayed key rather than opening a second one, which
// is what makes exactly one review exist per submission across a crash.
func (c *Client) CreateReview(
	ctx context.Context, issue, author, summary, branch, commit, session string,
	expectedBase, expectedDefaultHead, key string,
) (Review, error) {
	payload := map[string]any{
		"issue": issue, "author": author,
		"branch": branch, "commit": commit,
		// The FENCES. sutra resolves the branch's actual base and the default
		// branch's actual head, and rejects atomically when either differs —
		// so a diff whose base was never gated cannot reach a human at all.
		"expected_base_commit":  expectedBase,
		"expected_default_head": expectedDefaultHead,
	}
	// Omitted rather than sent empty: sutra rejects an explicit null for these
	// and distinguishes absent from blank.
	if summary != "" {
		payload["summary"] = summary
	}
	if session != "" {
		payload["session"] = session
	}
	var rv Review
	err := c.do(ctx, "createReview", http.MethodPost, "/reviews", key, payload, &rv)
	return rv, err
}

// GetReview reads a review's current state.
//
// Not a mutation, so it carries no idempotency key and no body — it needs its
// own path rather than a branch inside `do`.
func (c *Client) GetReview(ctx context.Context, id string) (Review, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/reviews/"+id, nil)
	if err != nil {
		return Review{}, fmt.Errorf("build getReview: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Review{}, fmt.Errorf("send getReview: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return Review{}, &APIError{Status: resp.StatusCode, Body: string(b), Op: "getReview"}
	}
	var rv Review
	if err := json.NewDecoder(resp.Body).Decode(&rv); err != nil {
		return Review{}, fmt.Errorf("decode getReview: %w", err)
	}
	return rv, nil
}

// ResubmitReview advances a review to a new revision after rework.
//
// expectedRevision and expectedVerdictEvent are FENCES: sutra refuses the call
// if the review has moved on, so a replay cannot advance a revision twice and
// a later verdict cannot be answered by an earlier rework.
func (c *Client) ResubmitReview(
	ctx context.Context, id, author, summary, branch, commit, session string,
	expectedRevision int, expectedVerdictEvent, expectedBase, expectedDefaultHead, key string,
) (Review, error) {
	payload := map[string]any{
		"author": author, "branch": branch, "commit": commit,
		"expected_revision": expectedRevision, "expected_verdict_event": expectedVerdictEvent,
		// The same base fences a fresh submission carries: rework is no more
		// entitled to put an ungated diff in front of a human.
		"expected_base_commit":  expectedBase,
		"expected_default_head": expectedDefaultHead,
	}
	if summary != "" {
		payload["summary"] = summary
	}
	if session != "" {
		payload["session"] = session
	}
	var rv Review
	err := c.do(ctx, "resubmitReview", http.MethodPost, "/reviews/"+id+"/resubmit", key, payload, &rv)
	return rv, err
}

// ConsumeApproval claims a review's approval so a merge may proceed.
//
// The fences are the same shape as a resubmission's: sutra refuses if the
// review has moved on. Once consumed, a verdict reversal is rejected — which
// is what makes "merges exactly once" hold across a human changing their mind
// mid-merge.
func (c *Client) ConsumeApproval(
	ctx context.Context, id, actor string, expectedRevision int,
	expectedVerdictEvent, key string,
) (Review, error) {
	payload := map[string]any{
		"actor": actor, "expected_revision": expectedRevision,
		"expected_verdict_event": expectedVerdictEvent,
	}
	var rv Review
	err := c.do(ctx, "consumeReviewApproval", http.MethodPost,
		"/reviews/"+id+"/consume", key, payload, &rv)
	return rv, err
}

// CompleteIssue transitions a ticket to complete, naming the review that
// approved it.
//
// All three of review, revision and verdict event travel: sutra stamps the
// review close-used against exactly them, so a stale revision rejects and the
// merge-time consumption — a different claim on the same approval — never
// blocks it.
func (c *Client) CompleteIssue(
	ctx context.Context, issue, review string, revision int,
	verdictEvent, actor, key string,
) error {
	return c.do(ctx, "completeIssue", http.MethodPost, "/issues/"+issue+"/status", key,
		map[string]any{
			"status": "complete", "review": review, "review_revision": revision,
			"review_verdict_event": verdictEvent, "actor": actor,
		}, nil)
}

// Popped is what a work-stack pop returns.
//
// Issue is empty when nothing is workable. That is NOT an error: idling is the
// ordinary state of a plan whose remaining tickets are blocked or in flight,
// and treating it as one would exit a build that is merely waiting.
type Popped struct {
	Issue         Issue `json:"issue"`
	FeedWatermark int64 `json:"feed_watermark"`
}

// Pop claims the next workable ticket for an identity.
func (c *Client) Pop(ctx context.Context, identity, key string) (Popped, error) {
	var p Popped
	err := c.do(ctx, "popWorkStack", http.MethodPost,
		"/identities/"+identity+"/work-stack/pop", key, map[string]any{}, &p)
	return p, err
}

// Event is one entry in sutra's feed.
type Event struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// EventPage is a cursor-bounded slice of the feed.
type EventPage struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor"`
	Drained    bool    `json:"drained"`
}

// Events reads the feed from a cursor.
//
// A read, so it carries no idempotency key. The CURSOR is what makes it
// resumable: kriya persists the one it has consumed to, and a restart picks up
// exactly where it left off rather than re-reading from the beginning or
// skipping what it never saw.
func (c *Client) Events(ctx context.Context, cursor, kind string, limit int) (EventPage, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/events?"+q.Encode(), nil)
	if err != nil {
		return EventPage{}, fmt.Errorf("build listEvents: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return EventPage{}, fmt.Errorf("send listEvents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return EventPage{}, &APIError{Status: resp.StatusCode, Body: string(b), Op: "listEvents"}
	}
	var page EventPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return EventPage{}, fmt.Errorf("decode listEvents: %w", err)
	}
	return page, nil
}
