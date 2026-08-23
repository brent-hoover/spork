// Package reviewbridge is the roborev seam — enqueue, poll verdicts, respond,
// close. Buy-over-build lives here, in one place.
//
// Risk R1 settled the protocol and its constraints are load-bearing: roborev's
// enqueue accepts NO caller correlation key and does NOT dedupe by SHA, so two
// jobs can exist for one commit and nothing in the job identifies whose it is.
// Kriya therefore acts ONLY on ids recorded from its own enqueue returns — no
// adoption of jobs it did not create, no cancellation of unproven ones. Where
// an id cannot be established the attempt is recorded unresolved and surfaced
// to the operator rather than guessed at.
package reviewbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"kriya/internal/clock"
)

// Verdicts a round can reach.
const (
	VerdictPending  = "pending"
	VerdictClean    = "clean"
	VerdictFindings = "findings"
)

// Attempt states.
const (
	AttemptPending    = "pending"
	AttemptResolved   = "resolved"
	AttemptUnresolved = "unresolved"
)

// EnqueueAttempt records an enqueue across its crash window.
//
// Written BEFORE roborev is called, because the call has no key: if kriya
// crashes between sending and recording, nothing in roborev says which job was
// its own, and adopting one by SHA could adopt someone else's. The attempt is
// the only evidence the call happened at all.
type EnqueueAttempt struct {
	Round  string
	Run    string
	Commit string
	// JobID is roborev's id, once established. Zero while unresolved.
	JobID int
	State string
	// Note explains an unresolved attempt to the operator.
	Note string
}

// Round is one review cycle on one commit.
type Round struct {
	ID      string
	Run     string
	Commit  string
	JobID   int
	Verdict string
	// Findings is roborev's own report, handed back to the dev agent verbatim.
	Findings string
}

// Store persists rounds and attempts.
type Store interface {
	UpsertAttempt(ctx context.Context, a EnqueueAttempt) error
	UpsertRound(ctx context.Context, r Round) error
	Unresolved(ctx context.Context) ([]EnqueueAttempt, error)
}

// Roborev is the slice of the tool this package drives.
type Roborev interface {
	// Enqueue submits a commit and returns the job id it created.
	Enqueue(ctx context.Context, repo, commit string) (int, error)
	// Status reports a job's status and its report when finished.
	Status(ctx context.Context, repo string, jobID int) (status, report string, err error)
	// Close marks a job resolved.
	Close(ctx context.Context, repo string, jobID int) error
}

// Bridge drives roborev for one repository.
type Bridge struct {
	Repo  string
	Store Store
	Rev   Roborev
	Now   clock.Clock
}

// Submit enqueues a review and records the round.
func (b Bridge) Submit(ctx context.Context, run, roundID, commit string) (Round, error) {
	attempt := EnqueueAttempt{Round: roundID, Run: run, Commit: commit, State: AttemptPending}
	if err := b.Store.UpsertAttempt(ctx, attempt); err != nil {
		return Round{}, fmt.Errorf("record enqueue attempt: %w", err)
	}

	jobID, err := b.Rev.Enqueue(ctx, b.Repo, commit)
	if err != nil {
		// The call may or may not have created a job. Recording the attempt
		// unresolved is the honest state: kriya will not adopt a job by SHA,
		// and the operator decides.
		attempt.State = AttemptUnresolved
		attempt.Note = err.Error()
		if upsertErr := b.Store.UpsertAttempt(ctx, attempt); upsertErr != nil {
			return Round{}, upsertErr
		}
		return Round{}, fmt.Errorf("enqueue review for %s: %w", commit, err)
	}

	attempt.JobID = jobID
	attempt.State = AttemptResolved
	if err := b.Store.UpsertAttempt(ctx, attempt); err != nil {
		return Round{}, err
	}
	round := Round{ID: roundID, Run: run, Commit: commit, JobID: jobID, Verdict: VerdictPending}
	if err := b.Store.UpsertRound(ctx, round); err != nil {
		return Round{}, err
	}
	return round, nil
}

// Poll reads a round's current verdict.
//
// A job still running is not an error: waiting is the ordinary state, and the
// caller decides how long to wait.
func (b Bridge) Poll(ctx context.Context, round Round) (Round, error) {
	if round.JobID == 0 {
		return round, fmt.Errorf("round %s has no job id", round.ID)
	}
	status, report, err := b.Rev.Status(ctx, b.Repo, round.JobID)
	if err != nil {
		return round, fmt.Errorf("poll job %d: %w", round.JobID, err)
	}
	if status != "done" {
		round.Verdict = VerdictPending
		return round, nil
	}
	round.Findings = report
	// A report naming findings is changes-requested; a clean one is a pass.
	// The distinction is roborev's, read from its own output rather than
	// inferred from an exit code — a review that ran and found nothing and a
	// review that failed to run are different outcomes.
	if hasFindings(report) {
		round.Verdict = VerdictFindings
	} else {
		round.Verdict = VerdictClean
	}
	if err := b.Store.UpsertRound(ctx, round); err != nil {
		return round, err
	}
	return round, nil
}

// Settle closes a job kriya is done with.
//
// Called once a round has a terminal verdict: kriya has read the report and
// either moves on or fixes and enqueues a NEW review on a NEW commit, so the
// job is finished either way. Only jobs from kriya's own enqueue returns are
// ever closed — R1 forbids touching a job it cannot prove is its own.
func (b Bridge) Settle(ctx context.Context, round Round) error {
	if round.JobID == 0 {
		return fmt.Errorf("round %s has no job id", round.ID)
	}
	if round.Verdict == VerdictPending {
		return fmt.Errorf("round %s is still pending", round.ID)
	}
	if err := b.Rev.Close(ctx, b.Repo, round.JobID); err != nil {
		return fmt.Errorf("close job %d: %w", round.JobID, err)
	}
	return nil
}

// Recover reports enqueue attempts whose outcome kriya cannot establish.
//
// It resolves nothing: an attempt with no proven job id is exactly the case
// R1 forbids guessing at, because the job that matches by SHA may be someone
// else's. The operator decides. Returning them rather than erroring is
// deliberate — one ambiguous review must not block every future build.
func (b Bridge) Recover(ctx context.Context) ([]EnqueueAttempt, error) {
	return b.Store.Unresolved(ctx)
}

// findingMarker matches roborev's finding headers.
var findingMarker = regexp.MustCompile(`(?m)^-?\s*\*\*Severity\*\*`)

func hasFindings(report string) bool {
	if findingMarker.MatchString(report) {
		return true
	}
	// roborev says so in plain words when there is nothing.
	return !strings.Contains(report, "No issues found")
}

// CLI drives the roborev binary.
type CLI struct{ Bin string }

func (c CLI) bin() string {
	if c.Bin == "" {
		return "roborev"
	}
	return c.Bin
}

// jobIDPattern finds the id roborev prints when it enqueues.
var jobIDPattern = regexp.MustCompile(`\b(?:job|Job)\s*#?(\d+)`)

// Enqueue submits a commit and returns the job id.
func (c CLI) Enqueue(ctx context.Context, repo, commit string) (int, error) {
	cmd := exec.CommandContext(ctx, c.bin(), "review", commit)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("roborev review: %s", strings.TrimSpace(string(out)))
	}
	m := jobIDPattern.FindStringSubmatch(string(out))
	if m == nil {
		// No id means no way to act on this job without adopting one by SHA,
		// which R1 forbids. Failing here is what makes the attempt unresolved
		// rather than silently attached to a stranger's review.
		return 0, fmt.Errorf("roborev printed no job id: %s", strings.TrimSpace(string(out)))
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("parse job id %q: %w", m[1], err)
	}
	return id, nil
}

// Status reports a job's status and its report when finished.
func (c CLI) Status(ctx context.Context, repo string, jobID int) (string, string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), "list", "--json", "--limit", "200")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("roborev list: %w", err)
	}
	var jobs []struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &jobs); err != nil {
		return "", "", fmt.Errorf("parse roborev list: %w", err)
	}
	for _, j := range jobs {
		if j.ID != jobID {
			continue
		}
		if j.Status != "done" {
			return j.Status, "", nil
		}
		show := exec.CommandContext(ctx, c.bin(), "show", "--job", strconv.Itoa(jobID))
		show.Dir = repo
		report, err := show.Output()
		if err != nil {
			return "", "", fmt.Errorf("roborev show %d: %w", jobID, err)
		}
		return "done", string(report), nil
	}
	return "", "", fmt.Errorf("roborev knows no job %d", jobID)
}

// Close marks a job resolved.
func (c CLI) Close(ctx context.Context, repo string, jobID int) error {
	cmd := exec.CommandContext(ctx, c.bin(), "close", strconv.Itoa(jobID))
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("roborev close %d: %s", jobID, strings.TrimSpace(string(out)))
	}
	return nil
}
