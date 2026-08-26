package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
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
	ctx context.Context, issue, author, summary, branch, commit, session string,
	expectedBase, expectedDefaultHead, key string,
) (string, error) {
	rv, err := t.c.CreateReview(ctx, issue, author, summary, branch, commit, session,
		expectedBase, expectedDefaultHead, key)
	return rv.ID, err
}

func (t sutraReviews) Resubmit(
	ctx context.Context, id, author, summary, branch, commit, session string,
	expectedRevision int, expectedVerdictEvent, expectedBase, expectedDefaultHead, key string,
) (int, error) {
	rv, err := t.c.ResubmitReview(ctx, id, author, summary, branch, commit, session,
		expectedRevision, expectedVerdictEvent, expectedBase, expectedDefaultHead, key)
	return rv.Revision, err
}

// sutraApprovals adapts the sutra client to the merge queue's approval seam.
type sutraApprovals struct{ c *trackerclient.Client }

func (t sutraApprovals) Consume(
	ctx context.Context, review, actor string, expectedRevision int,
	expectedVerdictEvent, key string,
) error {
	_, err := t.c.ConsumeApproval(ctx, review, actor, expectedRevision,
		expectedVerdictEvent, key)
	return err
}

// sutraTickets adapts the sutra client to the completer's ticket seam.
type sutraTickets struct {
	c     *trackerclient.Client
	actor string
}

func (t sutraTickets) Complete(
	ctx context.Context, issue, review string, revision int, verdictEvent, key string,
) error {
	return t.c.CompleteIssue(ctx, issue, review, revision, verdictEvent, t.actor, key)
}

// sutraPops adapts the sutra client to the pop loop's seam.
type sutraPops struct {
	c        *trackerclient.Client
	identity string
}

func (t sutraPops) Pop(ctx context.Context, key string) (string, string, error) {
	got, err := t.c.Pop(ctx, t.identity, key)
	if err != nil {
		return "", "", err
	}
	return got.Issue.ID, got.Issue.Title, nil
}

// sutraIssues adapts sutra's issue listing to completion detection's seam.
//
// The ACTIVE statuses only. A long-lived project's completed history is not
// what "is this build finished" is asking about, and paging it to find the
// few outstanding rows is work neither side needs to do.
type sutraIssues struct{ c *trackerclient.Client }

func (s sutraIssues) Active(
	ctx context.Context, projectID string,
) ([]planner.LiveIssue, string, error) {
	listing, err := s.c.ListIssues(ctx, projectID,
		[]string{"open", "queued", "in-progress", "blocked"})
	if err != nil {
		return nil, "", err
	}
	out := make([]planner.LiveIssue, 0, len(listing.Issues))
	for _, i := range listing.Issues {
		out = append(out, planner.LiveIssue{
			ID: i.ID, Status: i.Status, SubtreeRevision: i.SubtreeRevision,
		})
	}
	return out, listing.Watermark, nil
}

// sutraDocs adapts sutra's document API to the completion claim's seam.
type sutraDocs struct {
	c     *trackerclient.Client
	actor string
}

func (d sutraDocs) Create(
	ctx context.Context, projectID, title, issue, content, key string,
) (string, string, error) {
	doc, err := d.c.CreateDocument(ctx, projectID, title, issue, content, d.actor, key)
	if err != nil {
		return "", "", err
	}
	if doc.CurrentVersion == nil {
		// The review's deliverable IS the version. A document pointing at
		// nothing would open a review over nothing.
		return doc.ID, "", nil
	}
	return doc.ID, *doc.CurrentVersion, nil
}

// sutraCompletionReviews opens the review a human approves to finish a build.
type sutraCompletionReviews struct {
	c     *trackerclient.Client
	actor string
}

func (r sutraCompletionReviews) Create(
	ctx context.Context, issue, summary, docVersion, key string,
) (string, int, error) {
	rv, err := r.c.CreateDocReview(ctx, issue, r.actor, summary, docVersion, key)
	if err != nil {
		return "", 0, err
	}
	return rv.ID, rv.Revision, nil
}

// sutraEpics adapts sutra's issue-status API to the completion close's seam.
type sutraEpics struct {
	c     *trackerclient.Client
	actor string
}

func (e sutraEpics) Close(
	ctx context.Context, epic, review string, revision int,
	verdictEvent string, expectedSubtreeRevision int64, key string,
) error {
	return e.c.CloseEpic(ctx, epic, review, revision, verdictEvent, e.actor,
		expectedSubtreeRevision, key)
}

// sutraWorkFeed adapts sutra's event feed to the work watcher's seam.
//
// UNFILTERED at the source, and filtered here, for the same reason the verdict
// feed is: sutra's kind filter takes one value and the watcher cares about
// more than one.
type sutraWorkFeed struct{ c *trackerclient.Client }

func (f sutraWorkFeed) Since(
	ctx context.Context, cursor string,
) ([]planner.WorkEvent, string, error) {
	page, err := f.c.Events(ctx, cursor, "", 200)
	if err != nil {
		return nil, "", err
	}
	out := make([]planner.WorkEvent, 0, len(page.Events))
	for _, e := range page.Events {
		switch e.Kind {
		case planner.KindStatusChanged, planner.KindCreated:
			out = append(out, planner.WorkEvent{
				ID: e.ID, Kind: e.Kind, Subject: e.Subject,
			})
		}
	}
	return out, page.NextCursor, nil
}

// sutraFindingReviews opens and advances the review over a spike's finding.
type sutraFindingReviews struct {
	c     *trackerclient.Client
	actor string
}

func (r sutraFindingReviews) Create(
	ctx context.Context, issue, summary, docVersion, key string,
) (string, int, error) {
	rv, err := r.c.CreateDocReview(ctx, issue, r.actor, summary, docVersion, key)
	if err != nil {
		return "", 0, err
	}
	return rv.ID, rv.Revision, nil
}

func (r sutraFindingReviews) Resubmit(
	ctx context.Context, id, summary, docVersion string,
	expectedRevision int, expectedVerdictEvent, key string,
) (int, error) {
	return r.c.ResubmitDocReview(ctx, id, r.actor, summary, docVersion,
		expectedRevision, expectedVerdictEvent, key)
}

// sutraSpikeTickets closes a spike through sutra's approved-review gate.
type sutraSpikeTickets struct {
	c     *trackerclient.Client
	actor string
}

func (t sutraSpikeTickets) Complete(
	ctx context.Context, issue, review string, revision int,
	verdictEvent, key string,
) error {
	return t.c.CompleteIssue(ctx, issue, review, revision, verdictEvent, t.actor, key)
}

// sutraFeed adapts sutra's event feed to the verdict router's seam.
//
// It reads only review verdicts. The feed carries every event sutra emits, and
// filtering at the source is what keeps kriya from paging through work it has
// no opinion about.
type sutraFeed struct{ c *trackerclient.Client }

func (f sutraFeed) Since(
	ctx context.Context, cursor string,
) ([]orchestrator.VerdictEvent, string, error) {
	// UNFILTERED, and filtered here. sutra's kind filter takes one value, and
	// a verdict is two kinds — approved and changes-requested — so a filtered
	// read would silently see only half of them. Reading the whole feed and
	// keeping what kriya recognises is the only way to see both while the
	// cursor still advances past everything else.
	page, err := f.c.Events(ctx, cursor, "", 100)
	if err != nil {
		return nil, "", err
	}
	var out []orchestrator.VerdictEvent
	for _, e := range page.Events {
		if e.Kind != orchestrator.KindApproved && e.Kind != orchestrator.KindChangesRequested {
			continue
		}
		v, err := orchestrator.ParseVerdict(e.ID, e.Kind, e.Payload)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
	}
	return out, page.NextCursor, nil
}

// sutraRevisions reads a review's current revision.
type sutraRevisions struct{ c *trackerclient.Client }

func (r sutraRevisions) Revision(ctx context.Context, review string) (int, error) {
	rv, err := r.c.GetReview(ctx, review)
	if err != nil {
		return 0, err
	}
	return rv.Revision, nil
}

func (t sutraTracker) AssignIssue(ctx context.Context, issueID, assignee, actor, idem string) error {
	return t.c.AssignIssue(ctx, issueID, assignee, actor, idem)
}

// sutraDeferrer is planner's retirement port onto sutra.
type sutraDeferrer struct{ c *trackerclient.Client }

func (d sutraDeferrer) Status(ctx context.Context, issue string) (string, error) {
	got, err := d.c.GetIssue(ctx, issue)
	if err != nil {
		return "", err
	}
	return got.Status, nil
}

func (d sutraDeferrer) Defer(ctx context.Context, issue, expect, actor, key string) error {
	return d.c.DeferIssue(ctx, issue, expect, actor, key)
}

// liveRuns answers which tickets a LIVE build currently holds.
//
// A live build is one in a non-terminal state. The distinction is the whole
// point of the "bound" disposition: a popped build finishes under its pinned
// snapshot, so deferring its ticket would cancel work in progress — while a
// run that has already merged, failed or been CANCELLED holds nothing, and
// treating its ticket as bound would leave dropped work open forever, holding
// the epic with it.
type liveRuns struct{ db *sql.DB }

// terminalRunStates are the states in which a run holds nothing.
//
// Cancelled is here deliberately: a ticket whose only matching run is
// terminally cancelled is RELEASED work, already deferred by the cancellation
// protocol, and its row must retire rather than be assumed still in flight.
var terminalRunStates = []orchestrator.State{
	orchestrator.StateMerged, orchestrator.StateFailed,
	orchestrator.StateCancelled, orchestrator.StateClosed,
	orchestrator.StateNoWork,
}

func (l liveRuns) Claimed(ctx context.Context, issues []string) (map[string]bool, error) {
	if len(issues) == 0 {
		return map[string]bool{}, nil
	}
	args := make([]any, 0, len(issues)+len(terminalRunStates))
	placeholders := make([]string, len(issues))
	for n, issue := range issues {
		placeholders[n] = "?"
		args = append(args, issue)
	}
	terminal := make([]string, len(terminalRunStates))
	for n, state := range terminalRunStates {
		terminal[n] = "?"
		args = append(args, string(state))
	}

	// build_run.ISSUE, never .ticket. The ticket column holds the ticket's
	// TITLE — IssueMigration exists because a previous version substituted
	// one for the other, and querying titles with issue ids matches nothing,
	// so every live build looks unclaimed and retirement defers its work.
	rows, err := l.db.QueryContext(ctx,
		`SELECT DISTINCT issue FROM build_run
		   WHERE issue IN (`+strings.Join(placeholders, ",")+`)
		     AND state NOT IN (`+strings.Join(terminal, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read live runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	claimed := map[string]bool{}
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			return nil, fmt.Errorf("scan live run: %w", err)
		}
		claimed[issue] = true
	}
	// Checked: a short list here reads as "nothing is live", and retirement
	// would defer a ticket a build is working.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live runs: %w", err)
	}
	return claimed, nil
}
