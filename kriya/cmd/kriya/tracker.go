package main

import (
	"context"
	"encoding/json"

	"kriya/internal/trackerclient"
)

// sutraTracker adapts the sutra client to the narrow interface planner
// declares. planner may import trackerclient, but the adapter keeps the
// coupling one-way and makes planner testable with no HTTP at all.
type sutraTracker struct{ c *trackerclient.Client }

func (t sutraTracker) CreateProject(ctx context.Context, key, name, actor, idem string) (string, error) {
	p, err := t.c.CreateProject(ctx, key, name, actor, idem)
	return p.ID, err
}

func (t sutraTracker) CreateIssue(ctx context.Context, projectID, title, body, actor, idem string) (string, error) {
	i, err := t.c.CreateIssue(ctx, projectID, title, body, actor, idem)
	return i.ID, err
}

func (t sutraTracker) AddRelation(ctx context.Context, issueID, kind, to, actor, idem string) error {
	return t.c.AddRelation(ctx, issueID, kind, to, actor, idem)
}

// sutraThreads adapts the sutra client to the dev loop's thread seam.
//
// Same reason as the tracker adapter above: MOD-dev-loop's declared boundary
// does not permit the tracker client, so the composition root is what connects
// them and the loop stays testable with no HTTP at all.
type sutraThreads struct{ c *trackerclient.Client }

func (t sutraThreads) Import(
	ctx context.Context, title string, transcript json.RawMessage,
	session, issue, actor, key string,
) (string, error) {
	thread, err := t.c.ImportThread(ctx, title, transcript, session, issue, actor, key)
	return thread.ID, err
}

// sutraReviews adapts the sutra client to the orchestrator's review seam.
type sutraReviews struct{ c *trackerclient.Client }

func (t sutraReviews) Create(
	ctx context.Context, issue, author, summary, branch, commit, session, key string,
) (string, error) {
	rv, err := t.c.CreateReview(ctx, issue, author, summary, branch, commit, session, key)
	return rv.ID, err
}

func (t sutraReviews) Resubmit(
	ctx context.Context, id, author, summary, branch, commit, session string,
	expectedRevision int, expectedVerdictEvent, key string,
) (int, error) {
	rv, err := t.c.ResubmitReview(ctx, id, author, summary, branch, commit, session,
		expectedRevision, expectedVerdictEvent, key)
	return rv.Revision, err
}
