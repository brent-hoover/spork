package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// Findings renders a spike's documented answer to its risk.
type Findings interface {
	// Research produces the finding. It reads; it writes no code, which is
	// what makes a spike exempt from the gate chain.
	Research(ctx context.Context, run BuildRun) (string, error)
}

// FindingDocs is the slice of the tracker a finding needs.
type FindingDocs interface {
	// Create returns the document and the version it points at. A replayed
	// key returns the ORIGINAL of both.
	Create(ctx context.Context, projectID, title, issue, content, key string) (doc, version string, err error)
}

// FindingReviews opens the review a human approves to retire a risk.
type FindingReviews interface {
	Create(ctx context.Context, issue, summary, docVersion, key string) (id string, revision int, err error)
	// Resubmit advances a revised finding to its next revision, fenced on the
	// revision it expects to find and the verdict it answers.
	Resubmit(ctx context.Context, id, summary, docVersion string,
		expectedRevision int, expectedVerdictEvent, key string) (int, error)
}

// FindingKey is deterministic per (run, review revision).
//
// The REVISION is in it because a revised finding is a new document version: a
// key that did not rotate would replay the previous version, and the human
// would approve the finding they had already rejected.
func FindingKey(run string, revision int) string {
	sum := sha256.Sum256([]byte("kriya-finding-doc:" + run + ":" + strconv.Itoa(revision)))
	return hex.EncodeToString(sum[:])
}

// Researcher runs a spike to a submitted finding.
//
// The whole path: research, then the keyed document, then the keyed review.
// Write-ahead at each step, so recovery from any point replays exactly what
// was promised — the same protocol shape as a code submission, over a document
// deliverable rather than a diff.
type Researcher struct {
	Store    Store
	Findings Findings
	Docs     FindingDocs
	Reviews  FindingReviews
	// Tickets closes a spike on its approved finding. Nil never retires one,
	// which is what a module-level test of the submission path wants.
	Tickets SpikeTickets
}

// Submit researches the risk and opens the finding review.
func (r Researcher) Submit(
	ctx context.Context, run BuildRun, projectID string,
) (BuildRun, error) {
	if run.Issue == "" {
		// The review hangs off the spike's own issue. Without it there is
		// nothing for a human to approve against.
		return BuildRun{}, fmt.Errorf("spike run %s names no issue", run.ID)
	}
	finding, err := r.Findings.Research(ctx, run)
	if err != nil {
		return BuildRun{}, fmt.Errorf("research %s: %w", run.Ticket, err)
	}
	if finding == "" {
		// A review with nothing to read is a human asked to approve a blank
		// page — and a risk retired on no evidence at all.
		return BuildRun{}, fmt.Errorf("the finding for %s is empty", run.Ticket)
	}

	run.PendingFinding = finding
	run.FindingKey = FindingKey(run.ID, run.ReviewRevision)
	run.ReviewState = SubmitSubmitting
	if err := r.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record pending finding: %w", err)
	}
	return r.finishFinding(ctx, run, projectID)
}

// finishFinding performs the calls the write-ahead row promised.
//
// Shared with recovery, so a replay takes exactly the same path as a first
// attempt — two implementations of one protocol would leave only one tested.
func (r Researcher) finishFinding(
	ctx context.Context, run BuildRun, projectID string,
) (BuildRun, error) {
	// Replayed UNCONDITIONALLY rather than skipped when a version is already
	// recorded: the key is what makes it safe, and trusting kriya's own record
	// instead means a row written before the call landed can never self-heal.
	doc, version, err := r.Docs.Create(ctx, projectID,
		"Finding: "+run.Ticket, run.Issue, run.PendingFinding, run.FindingKey)
	if err != nil {
		return BuildRun{}, fmt.Errorf("create finding document for %s: %w", run.ID, err)
	}
	if version == "" {
		return BuildRun{}, fmt.Errorf("the finding for %s has no version to review", run.ID)
	}
	run.FindingDoc, run.FindingVersion = doc, version
	if err := r.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record finding document: %w", err)
	}

	if run.ReviewID != "" {
		return r.resubmit(ctx, run)
	}
	id, revision, err := r.Reviews.Create(ctx, run.Issue,
		"Finding: "+run.Ticket, run.FindingVersion, run.ID)
	if err != nil {
		return BuildRun{}, fmt.Errorf("submit finding review for %s: %w", run.ID, err)
	}
	run.ReviewID, run.ReviewRevision = id, revision
	run.ReviewState = SubmitSubmitted
	if err := r.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record submitted finding: %w", err)
	}
	return run, nil
}

// resubmit advances a revised finding to its next revision.
//
// It rides the same write-ahead machinery a code resubmission does: the
// expected revision and the answered verdict event are fences sutra enforces,
// so a replay cannot advance a revision twice and a later verdict cannot be
// answered by an earlier revision.
func (r Researcher) resubmit(ctx context.Context, run BuildRun) (BuildRun, error) {
	if run.ReviewVerdictEvent == "" {
		// Without it sutra cannot fence the call, and a revision could answer
		// a verdict the human has since replaced.
		return BuildRun{}, fmt.Errorf("run %s names no verdict event to answer", run.ID)
	}
	revision, err := r.Reviews.Resubmit(ctx, run.ReviewID, "Finding: "+run.Ticket,
		run.FindingVersion, run.ReviewRevision, run.ReviewVerdictEvent, run.FindingKey)
	if err != nil {
		return BuildRun{}, fmt.Errorf("resubmit finding for %s: %w", run.ID, err)
	}
	if revision != run.ReviewRevision+1 {
		// sutra advances by exactly one. Anything else means the review moved
		// under the fence, which is what the fence exists to catch.
		return BuildRun{}, fmt.Errorf(
			"finding review %s advanced to revision %d, expected %d",
			run.ReviewID, revision, run.ReviewRevision+1)
	}
	run.ReviewRevision = revision
	// CLEARED: the verdict has been answered. Its presence is what says a
	// revision is outstanding.
	run.ReviewVerdictEvent = ""
	run.ReviewState = SubmitSubmitted
	if err := r.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record resubmitted finding: %w", err)
	}
	return run, nil
}

// SpikeTickets closes a spike on its approved finding.
type SpikeTickets interface {
	// Complete closes the ticket through the tracker's own approved-review
	// gate, naming the review at its approved revision.
	Complete(ctx context.Context, issue, review string, revision int,
		verdictEvent, key string) error
}

// Retire closes a spike whose finding a human approved.
//
// The close is what retires the risk: sutra releases the blocking relations
// when the blocker closes, so kriya asks for no unblocking of its own. What it
// must get right is the KEY — write-ahead, so a close that landed replays to
// its original success rather than a close-used conflict.
func (r Researcher) Retire(
	ctx context.Context, run BuildRun, approvalEvent string,
) (BuildRun, error) {
	if run.ReviewID == "" || run.FindingVersion == "" {
		// A risk retired without documented evidence is the one thing
		// risk-first exists to prevent.
		return BuildRun{}, fmt.Errorf("spike %s has no approved finding to close on", run.ID)
	}
	if approvalEvent == "" {
		return BuildRun{}, fmt.Errorf("the approval for %s names no verdict event", run.ID)
	}
	if run.CompletionState != CompleteCompleting || run.ReviewVerdictEvent != approvalEvent {
		run.ReviewVerdictEvent = approvalEvent
		run.CloseKey = SpikeCloseKey(run.ID, approvalEvent)
		run.CompletionState = CompleteCompleting
		if err := r.Store.Upsert(ctx, run); err != nil {
			return BuildRun{}, fmt.Errorf("record closing spike: %w", err)
		}
	}
	return r.finishRetire(ctx, run)
}

// finishRetire performs the close the write-ahead row promised.
func (r Researcher) finishRetire(ctx context.Context, run BuildRun) (BuildRun, error) {
	if err := r.Tickets.Complete(ctx, run.Issue, run.ReviewID, run.ReviewRevision,
		run.ReviewVerdictEvent, run.CloseKey); err != nil {
		return BuildRun{}, fmt.Errorf("close spike %s: %w", run.Issue, err)
	}
	run.CompletionState = CompleteClosed
	run.State = StateClosed
	if err := r.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record closed spike: %w", err)
	}
	return run, nil
}

// RecoverRetirements replays spike closes a crash left in flight.
func (r Researcher) RecoverRetirements(ctx context.Context) (int, error) {
	if r.Store == nil {
		return 0, nil
	}
	pending, err := r.Store.Completing(ctx)
	if err != nil {
		return 0, fmt.Errorf("list completing runs: %w", err)
	}
	var replayed int
	for _, run := range pending {
		if run.Kind != KindSpike {
			// A merged ticket's close. Its own recovery replays it.
			continue
		}
		if _, err := r.finishRetire(ctx, run); err != nil {
			return 0, err
		}
		replayed++
	}
	return replayed, nil
}

// SpikeCloseKey is deterministic per (run, approval event).
//
// The EVENT is in it so a reapproval after a reversal-induced conflict issues
// under a fresh key rather than replaying the cached conflict forever.
func SpikeCloseKey(run, approvalEvent string) string {
	sum := sha256.Sum256([]byte("kriya-spike-close:" + run + ":" + approvalEvent))
	return hex.EncodeToString(sum[:])
}

// RecoverFindings replays finding submissions a crash left in flight.
func (r Researcher) RecoverFindings(
	ctx context.Context, project func(BuildRun) (string, error),
) (int, error) {
	if r.Store == nil {
		// No store, nothing to replay. Nil is what a module-level test of the
		// recovery ordering wants, and a build with no spikes never wires one.
		return 0, nil
	}
	pending, err := r.Store.Submitting(ctx)
	if err != nil {
		return 0, fmt.Errorf("list submitting runs: %w", err)
	}
	var replayed int
	for _, run := range pending {
		if run.Kind != KindSpike {
			// A code submission. Its own recovery replays it, under its own
			// keys and against a branch rather than a document.
			continue
		}
		projectID, err := project(run)
		if err != nil {
			return 0, err
		}
		if _, err := r.finishFinding(ctx, run, projectID); err != nil {
			return 0, err
		}
		replayed++
	}
	return replayed, nil
}

// KindSpike is the ticket kind whose deliverable is a documented finding.
//
// Declared here rather than imported so the run's own routing does not depend
// on the planner: what this package needs is one string, and importing a module
// for it would be the tail wagging the dog.
const KindSpike = "spike"

// ErrNoFinding reports a spike that produced no evidence.
var ErrNoFinding = errors.New("the spike produced no finding")
