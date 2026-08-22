package main

import (
	"context"

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
