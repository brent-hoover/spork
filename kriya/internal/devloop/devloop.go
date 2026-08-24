// Package devloop runs one pair-programming session: a dev agent working in
// an isolated workspace on one ticket.
//
// The full loop — commit cadence, roborev rounds, fix, recommit — lands with
// the review bridge. What is here is the invocation itself: the agent seam,
// the per-ticket toolset, and the session record the transcript imports under.
package devloop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/architect"
	"kriya/internal/clock"
	kctx "kriya/internal/context"
	"kriya/internal/reviewbridge"
)

// Session is one dev-agent invocation against a ticket.
type Session struct {
	Run    string
	Ticket string
	// SessionID stamps the sutra thread the transcript imports into, so a
	// run's conversation is findable from its ticket.
	SessionID string
	Model     string
	// Commits records what the loop produced, so a review round and its
	// commit stay linked after the fact.
	Commits []string
	Rounds  int
	// SystemFile is where the assembled context was written, so a fix round
	// reads the same law the first round did.
	SystemFile string
	// TranscriptRef, ImportKey, ImportState and ThreadRef carry the transcript
	// import across its crash window. The first two are persisted TOGETHER
	// before sutra is called, so a replay reproduces the exact request.
	TranscriptRef string
	ImportKey     string
	ImportState   string
	ThreadRef     string
	// Turns is the conversation so far. Persisted because a pass that comes
	// back resumes this session, and turns held only in memory would leave
	// the eventual import carrying only what happened after the last resume.
	Turns []Exchange
	// Pending is the review round this session is waiting on. A resumed pass
	// POLLS it rather than committing again and submitting under the same
	// round id, which would replace the stored job and orphan the review.
	Pending reviewbridge.Round
	Ended   bool
}

// Store persists sessions.
type Store interface {
	Upsert(ctx context.Context, s Session) error
	// Importing lists sessions whose transcript import a crash left in
	// flight. Recovery replays them under their persisted key.
	Importing(ctx context.Context) ([]Session, error)
	// Find returns a run's session. A pass that came back — a review still
	// running, an architect that gave direction — resumes the one it left.
	Find(ctx context.Context, run string) (Session, bool, error)
}

// Committer commits the agent's work so a review has something to read.
//
// An interface this package declares rather than importing a git package:
// what the loop needs is "turn the workspace into a reviewable commit", and
// the composition root supplies it.
type Committer interface {
	Commit(ctx context.Context, dir, message string) (sha string, err error)
}

// Reviewer is the slice of the review bridge the loop drives.
type Reviewer interface {
	Submit(ctx context.Context, run, roundID, commit string) (reviewbridge.Round, error)
	Poll(ctx context.Context, round reviewbridge.Round) (reviewbridge.Round, error)
	Settle(ctx context.Context, round reviewbridge.Round, response string) error
}

// Contexts assembles what the dev agent is given.
type Contexts interface {
	Assemble(ctx context.Context, build string, spec kctx.Spec, ticket kctx.Ticket,
		instructions string) (kctx.Bundle, error)
}

// Architects resolve an impasse the pair loop cannot get past.
type Architects interface {
	Resolve(ctx context.Context, build, ticket, trigger string,
		findings []string, systemFile string) (architect.Intervention, error)
	Resume(ctx context.Context, build string) (architect.Intervention, bool, error)
	// MarkResumed closes the intervention, once its direction has actually
	// been put in front of the agent.
	MarkResumed(ctx context.Context, build string) error
}

// Learnings records what a correction taught.
type Learnings interface {
	Record(ctx context.Context, c kctx.Capture) error
}

// Loop drives a dev agent through pair-programming rounds.
type Loop struct {
	// Context assembles the ticket's law, contracts, and learnings. Nil skips
	// assembly, which is what a test of the loop's own protocol wants.
	Context Contexts
	// Threads imports finished transcripts into the tracker's catalog. Nil
	// skips capture.
	Threads Threads
	// Architect resolves an impasse the pair loop cannot get past. Nil makes
	// the round limit a plain stall.
	Architect Architects
	// Learnings records what a correction taught, at the moment of
	// correction. Nil records nothing.
	Learnings Learnings
	Agent     agent.Agent
	Store     Store
	Commit    Committer
	Review    Reviewer
	Now       clock.Clock
	// MaxRounds is the DEFAULT round limit. The one that governs a run is
	// snapshotted onto its Request at creation, so changing this affects only
	// future runs — a run mid-flight keeps the limit it started under.
	MaxRounds int
}

// Request is what the loop needs to work a ticket.
type Request struct {
	Run       string
	Ticket    string
	Title     string
	Body      string
	Criteria  []string
	Workspace string
	// Commands are the module's resolved gate commands. They travel as
	// CONTENT so the agent knows what its work will be judged by; permission
	// to run them is granted separately.
	Commands map[string]string
	// Spec is the pinned snapshot's law, for context assembly.
	Spec kctx.Spec
	// Instructions is the operator's hand-crafted project file.
	Instructions string
	// ProjectKey scopes a project-specific learning to its project.
	ProjectKey string
	// Issue is the tracker issue the transcript is tied to.
	Issue string
	// Actor is the identity every tracker mutation is recorded against.
	Actor string
	// Modules are the module ids this ticket touches.
	Modules []string
	// Patterns are the failure patterns this ticket is prone to.
	Patterns []string
	// Attempt is the gate-chain round this pass belongs to. It scopes the
	// round ids: a run re-enters the dev loop after a gate failure or a
	// rework, and ids that restarted at one upserted over the previous pass's
	// rows — replacing its job ids and verdicts while keeping its commit.
	Attempt int
	// RoundLimit is the limit snapshotted onto the BuildRun at its creation.
	// Zero falls back to the loop's default.
	RoundLimit int
	// AllowRules are the generated permission rules. No bare Bash: what an
	// agent may run is the ticket's business, and "tools irrelevant to the
	// ticket are absent" has to be mechanically true rather than requested.
	AllowRules []string
}

// ErrReviewPending reports that a review job is still running.
//
// A distinct error because the caller acts on it: the run has neither advanced
// nor failed, and it comes back rather than parking for an operator who can do
// nothing about a job that has not finished.
var ErrReviewPending = errors.New("the review round is still running")

// round produces one reviewed commit, or picks up the one being waited on.
//
// A session resumed with a pending round POLLS it. Committing again and
// submitting under the same round id would replace the stored job and orphan
// the review the previous pass was waiting for.
func (l Loop) round(
	ctx context.Context, req Request, session *Session,
	round int, answered *reviewbridge.Round,
) (reviewbridge.Round, string, error) {
	if session.Pending.ID != "" {
		waiting := session.Pending
		session.Pending = reviewbridge.Round{}
		reviewed, err := l.Review.Poll(ctx, waiting)
		return reviewed, waiting.Commit, err
	}

	sha, err := l.Commit.Commit(ctx, req.Workspace,
		fmt.Sprintf("%s (round %d)", req.Title, round+1))
	if err != nil {
		return reviewbridge.Round{}, "", fmt.Errorf("commit round %d: %w", round+1, err)
	}
	session.Commits = append(session.Commits, sha)

	if answered != nil {
		// The honest answer names the commit that addressed the findings, and
		// that commit does not exist until this round.
		if err := l.Review.Settle(ctx, *answered, "Addressed in "+sha+"."); err != nil {
			return reviewbridge.Round{}, "", err
		}
	}

	roundID := fmt.Sprintf("%s-%d-%d", req.Run, req.Attempt, round+1)
	reviewed, err := l.Review.Submit(ctx, req.Run, roundID, sha)
	if err != nil {
		return reviewbridge.Round{}, "", err
	}
	reviewed, err = l.Review.Poll(ctx, reviewed)
	return reviewed, sha, err
}

// fixRound hands one review's findings back to the agent that wrote the code.
func (l Loop) fixRound(
	ctx context.Context, req Request, session *Session, turns *[]Exchange,
	reviewed reviewbridge.Round, direction string, round int,
) (agent.Result, error) {
	fix := fixPrompt(req, reviewed.Findings, direction)
	if direction != "" {
		// Delivered. The intervention closes here, not when the direction was
		// looked up: a pass that read it and never reached a prompt would
		// otherwise close it with the direction given to nobody.
		if err := l.markDirectionDelivered(ctx, req.Run); err != nil {
			return agent.Result{}, err
		}
	}
	res, err := l.Agent.Run(ctx, agent.Request{
		Role:      agent.RoleDev,
		Prompt:    fix,
		Workspace: req.Workspace,
		// RESUMED, not restarted. The fix is a correction to work this agent
		// did: a fresh session re-reads its own findings with no memory of
		// what it wrote, and the transcript splits across session ids so only
		// the last one reaches the catalog.
		Resume:     session.SessionID,
		SystemFile: session.SystemFile,
		AllowRules: req.AllowRules,
	})
	if err != nil {
		return agent.Result{}, fmt.Errorf("dev agent fixing round %d: %w", round+1, err)
	}
	*turns = append(*turns, Exchange{
		Role: string(agent.RoleDev), Prompt: fix, Reply: res.Text, Model: res.Model,
	})
	session.SessionID = res.SessionID
	return res, nil
}

// open resumes the run's unfinished session, or starts one.
//
// A pass that came back — a review still running, an architect that gave
// direction — left a session with an id, a transcript and a round count. A new
// invocation would start a fresh conversation, commit the same work again, and
// submit it under an id nothing routes feedback to.
func (l Loop) open(ctx context.Context, req Request, direction string) (Session, error) {
	existing, found, err := l.Store.Find(ctx, req.Run)
	if err != nil {
		return Session{}, fmt.Errorf("read session: %w", err)
	}
	if found && !existing.Ended && existing.SessionID != "" {
		// Resumed. No new invocation: the agent already has the ticket, and
		// the pair loop picks up at the round it left with the conversation
		// it left.
		return existing, nil
	}

	session := Session{Run: req.Run, Ticket: req.Ticket}
	if err := l.Store.Upsert(ctx, session); err != nil {
		return Session{}, fmt.Errorf("record session: %w", err)
	}
	systemFile, err := l.assembleContext(ctx, req)
	if err != nil {
		return Session{}, err
	}
	session.SystemFile = systemFile

	implement := prompt(req, direction)
	if direction != "" {
		if err := l.markDirectionDelivered(ctx, req.Run); err != nil {
			return Session{}, err
		}
	}
	res, err := l.Agent.Run(ctx, agent.Request{
		Role:       agent.RoleDev,
		Prompt:     implement,
		Workspace:  req.Workspace,
		SystemFile: systemFile,
		AllowRules: req.AllowRules,
	})
	if err != nil {
		return Session{}, fmt.Errorf("dev agent on %s: %w", req.Ticket, err)
	}
	session.SessionID = res.SessionID
	session.Model = res.Model
	session.Turns = []Exchange{{
		Role: string(agent.RoleDev), Prompt: implement, Reply: res.Text, Model: res.Model,
	}}
	return session, nil
}

// stillRunning reports whether a pass ended mid-session.
//
// Only a pending review does. The session continues — the same conversation
// resumes and polls the same round — so importing a partial transcript under
// its own key would return that thread for every later import and lose
// everything after it.
//
// An architect handoff is NOT this. The impasse ENDS the session: its rounds
// are spent, and the next pass opens a fresh one whose first prompt carries
// the direction. Treating it as mid-session left a resumed session with no
// round budget, which did nothing at all and reported it as a clean pass.
func stillRunning(err error) bool {
	return errors.Is(err, ErrReviewPending)
}

// ErrArchitectDirected reports that the loop stalled and the architect gave
// direction for the next pass.
//
// A distinct error because a successful handoff is not a malfunction: as a
// plain one, the table read it as a failed dev-loop stage and parked the run
// in awaiting-operator — terminal, so the direction the architect had just
// recorded could never be consumed. The run comes back instead, and the next
// pass starts with that direction in the agent's prompt.
var ErrArchitectDirected = errors.New("the architect gave direction for the next pass")

// Work runs one session.
//
// The session is recorded BEFORE the agent is invoked and stamped ended after,
// so a crash mid-session leaves a row recovery can terminate and import the
// transcript from — AC-thread-no-loss requires every outcome to reach the
// thread catalog, crashed included.
func (l Loop) Work(ctx context.Context, req Request) (Session, error) {
	// Read BEFORE the first invocation, not only inside the pair loop. The
	// architect answered on a previous pass, and a pass whose first review
	// comes back clean never reaches a fix prompt — so the direction the
	// impasse was raised to get would never be given to anybody.
	direction, err := l.direction(ctx, req.Run)
	if err != nil {
		return Session{}, err
	}

	session, err := l.open(ctx, req, direction)
	if err != nil {
		return Session{}, err
	}
	turns := session.Turns

	var pairErr error
	if l.Commit != nil && l.Review != nil {
		pairErr = l.pair(ctx, req, &session, &turns, direction)
	}
	session.Turns = turns
	if stillRunning(pairErr) {
		// PERSISTED on the way out — the conversation, the round count and the
		// round still being waited on. A pass that comes back resumes from
		// exactly these, and anything held only in memory would leave the next
		// pass starting a fresh conversation and re-committing the same work.
		if err := l.Store.Upsert(ctx, session); err != nil {
			return Session{}, fmt.Errorf("record resuming session: %w", err)
		}
		// Not an outcome: the session continues, and importing a partial
		// transcript under its own key would return that thread for every
		// later import and lose everything after it.
		return Session{}, pairErr
	}

	// Captured before the session is stamped ended, and on a FAILED pass as
	// well as a clean one: AC-thread-no-loss wants every outcome in the
	// catalog, crashed included, and a session that died mid-loop with no
	// transcript ref is one recovery skips and nothing ever imports.
	if err := l.capture(ctx, req, &session, turns); err != nil {
		if pairErr != nil {
			return Session{}, errors.Join(pairErr, err)
		}
		return Session{}, err
	}
	session.Ended = true
	if err := l.Store.Upsert(ctx, session); err != nil {
		return Session{}, fmt.Errorf("record ended session: %w", err)
	}
	if pairErr != nil {
		// Stamped ended FIRST. A failed pass and an architect handoff both end
		// this session — the next one is new work with a new conversation —
		// and a row left unended would have that next pass resume a session
		// that already reported itself done.
		return Session{}, pairErr
	}
	return session, nil
}

// assembleContext produces the ticket's context and returns the file the
// agent is pointed at.
//
// Assembled ONCE per session and reused across fix rounds: the ticket's law
// does not change mid-session, and re-assembling would let the agent's view of
// the spec drift between rounds of the same piece of work.
func (l Loop) assembleContext(ctx context.Context, req Request) (string, error) {
	if l.Context == nil {
		return "", nil
	}
	bundle, err := l.Context.Assemble(ctx, req.Run, req.Spec, kctx.Ticket{
		Title:    req.Title,
		Criteria: req.Criteria,
		Modules:  req.Modules,
		Patterns: req.Patterns,
	}, req.Instructions)
	if err != nil {
		return "", fmt.Errorf("assemble context for %s: %w", req.Ticket, err)
	}
	path, err := kctx.SystemFile(req.Workspace, bundle)
	if err != nil {
		return "", fmt.Errorf("write context for %s: %w", req.Ticket, err)
	}
	return path, nil
}

// pair runs commit -> review -> fix -> recommit until the review is clean.
//
// Small commits with review latency in minutes, not an end-stage pull request
// (CON-pair-programming). The findings go back to the agent VERBATIM: they are
// the next instruction, and summarising them drops the file and line the fix
// needs.
func (l Loop) pair(
	ctx context.Context, req Request, session *Session, turns *[]Exchange, direction string,
) error {
	rounds := l.roundLimit(req)
	// Findings since the last clean pass. A clean pass resets it, which is
	// what makes the limit "consecutive rounds" rather than "rounds".
	var consecutive []string
	// answered is a round whose findings the agent has been given but whose
	// response cannot be written yet: the honest answer names the commit that
	// addressed it, and that commit does not exist until the next round.
	var answered *reviewbridge.Round
	// Continued from the round this session reached, not restarted. A resumed
	// pass beginning at zero would reuse the round ids the previous pass
	// already recorded, replacing their jobs and verdicts.
	//
	// A pending round is RE-ENTERED rather than followed: the count was
	// stamped when it was submitted, so the round still being waited on is
	// the last one counted.
	start := session.Rounds
	if session.Pending.ID != "" {
		start--
	}
	for round := start; round < rounds; round++ {
		reviewed, sha, err := l.round(ctx, req, session, round, answered)
		if err != nil {
			return err
		}
		session.Rounds = round + 1
		switch reviewed.Verdict {
		case reviewbridge.VerdictClean:
			// The counter would reset here, but a clean pass ENDS the loop, so
			// there is nothing left to count. It is reset where it matters:
			// nothing accumulates across a run that converged.
			return l.Review.Settle(ctx, reviewed, "Clean pass at "+sha+".")
		case reviewbridge.VerdictPending:
			// Still running. The caller polls again rather than the loop
			// blocking: a run waiting on a review is a state the TUI shows,
			// not a goroutine nobody can see.
			//
			// A DISTINCT error, not nil: reporting success stamped the session
			// ended and let the orchestrator advance the run to the gates with
			// a review job still running and nothing that would read it. The
			// round is recorded so the resumed pass POLLS it rather than
			// committing again and submitting under the same id.
			session.Pending = reviewed
			return ErrReviewPending
		}

		consecutive = append(consecutive, reviewed.Findings)
		res, err := l.fixRound(ctx, req, session, turns, reviewed, direction, round)
		if err != nil {
			return err
		}
		// Recorded only once the correction has actually been made. A lesson
		// written before the fixing agent ran would claim a correction that
		// may never have happened, and a retry would insert it twice.
		if err := l.learn(ctx, req, sha, res.SessionID, reviewed); err != nil {
			return err
		}
		answered = &reviewed
	}
	// Out of rounds with findings outstanding.
	return l.impasse(ctx, req, session, consecutive, rounds)
}

// roundLimit is the limit governing this run.
//
// The run's own, snapshotted at its creation, before the loop's default: a
// change to the configured limit affects only runs created after it.
func (l Loop) roundLimit(req Request) int {
	if req.RoundLimit > 0 {
		return req.RoundLimit
	}
	if l.MaxRounds > 0 {
		return l.MaxRounds
	}
	return 5
}

// direction reads any recorded architect direction for a run.
//
// From the ROW, never carried in memory: a run that restarted between the
// direction and the resume must still get it.
func (l Loop) direction(ctx context.Context, run string) (string, error) {
	if l.Architect == nil {
		return "", nil
	}
	resumed, found, err := l.Architect.Resume(ctx, run)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return resumed.Direction, nil
}

// markDirectionDelivered closes the intervention whose direction just reached
// a prompt.
func (l Loop) markDirectionDelivered(ctx context.Context, run string) error {
	if l.Architect == nil {
		return nil
	}
	if err := l.Architect.MarkResumed(ctx, run); err != nil {
		return fmt.Errorf("close intervention for %s: %w", run, err)
	}
	return nil
}

// impasse hands a stalled run to the architect.
//
// The last round stays OPEN — it is the evidence of what was asked for and
// never answered.
func (l Loop) impasse(ctx context.Context, req Request, session *Session,
	findings []string, rounds int) error {
	if l.Architect == nil {
		return fmt.Errorf("review still reporting findings after %d rounds", rounds)
	}
	in, err := l.Architect.Resolve(ctx, req.Run, req.Ticket,
		architect.TriggerRoundLimit, findings, session.SystemFile)
	if err != nil {
		return err
	}
	// The direction is a correction, and it teaches the same way a review
	// finding does. Recorded HERE rather than inside the architect because
	// MOD-architect may import nothing — and the loop is where the direction
	// is observed anyway.
	commit := ""
	if n := len(session.Commits); n > 0 {
		commit = session.Commits[n-1]
	}
	if err := l.record(ctx, kctx.Capture{
		Scope: kctx.ScopeProject, ProjectKey: req.ProjectKey,
		Lesson: in.Direction, Module: req.Ticket,
		Pattern: "sa-direction/" + in.Trigger, SourceKind: kctx.SourceSADirection,
		SourceRun: req.Run, SourceCommit: commit, SourceRef: in.Trigger,
	}); err != nil {
		return err
	}
	return fmt.Errorf("run %s paused for the architect after %d rounds: %w",
		req.Run, rounds, ErrArchitectDirected)
}

// learn records what a review finding taught.
//
// The TRIGGERING commit, not the fix: the lesson is about the code that was
// reviewed, and a later run matching this module wants to know what went wrong
// there rather than where it was patched.
func (l Loop) learn(
	ctx context.Context, req Request, commit, session string, round reviewbridge.Round,
) error {
	if l.Learnings == nil {
		return nil
	}
	module := req.Ticket
	if len(req.Modules) > 0 {
		module = req.Modules[0]
	}
	// ONE learning per finding. A whole report stored as a single lesson is
	// matched by whichever pattern the first finding happened to be about, and
	// every other finding in it is invisible to the feed-forward path.
	for n, finding := range splitFindings(round.Findings) {
		err := l.record(ctx, kctx.Capture{
			Scope: kctx.ScopeProject, ProjectKey: req.ProjectKey,
			Lesson: finding, Module: module, Pattern: patternOf(finding),
			SourceKind: kctx.SourceReviewFinding, SourceRun: req.Run,
			SourceCommit: commit,
			// The round AND the finding's position in it, so two lessons from
			// one report do not share a reference.
			SourceRef: fmt.Sprintf("%s#%d", round.ID, n), SourceSession: session,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// record writes one capture, skipping a lesson with nothing in it.
func (l Loop) record(ctx context.Context, c kctx.Capture) error {
	if l.Learnings == nil || c.Lesson == "" {
		return nil
	}
	if err := l.Learnings.Record(ctx, c); err != nil {
		return fmt.Errorf("record learning for %s: %w", c.Module, err)
	}
	return nil
}

// splitFindings breaks a review report into its individual findings.
//
// roborev separates them with a rule on its own line. A report with no rule is
// one finding, which is the ordinary single-finding case rather than an edge.
func splitFindings(report string) []string {
	var out []string
	for _, part := range strings.Split(report, "\n---\n") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// patternOf derives a finding's failure pattern from its own text.
//
// The severity is the one thing every finding states in a fixed form, so it is
// what a later run can match on. A richer classification would be kriya
// deciding what a reviewer meant, which is exactly the judgement the review
// exists to supply.
func patternOf(finding string) string {
	for _, severity := range []string{"Critical", "High", "Medium", "Low"} {
		if strings.Contains(finding, "**Severity**: "+severity) {
			return "review-finding/" + strings.ToLower(severity)
		}
	}
	return "review-finding"
}

// fixPrompt hands the findings back unaltered, under any architect direction.
//
// The direction comes FIRST because it is the instruction and the findings are
// what it applies to; a direction buried under a wall of findings reads as
// commentary.
func fixPrompt(req Request, findings, direction string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The review of your work on %q reported findings.\n\n", req.Title)
	if direction != "" {
		b.WriteString("The architect has given this direction:\n\n")
		b.WriteString(direction)
		b.WriteString("\n\n")
	}
	b.WriteString("Address every one, then stop. Do not restate them back.\n\n")
	b.WriteString(findings)
	return b.String()
}

// prompt states the ticket and the bar it will be judged by.
func prompt(req Request, direction string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement this ticket in the workspace.\n\nTitle: %s\n", req.Title)
	if direction != "" {
		fmt.Fprintf(&b, "\nThe architect has given this direction:\n\n%s\n", direction)
	}
	if req.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", req.Body)
	}
	if len(req.Criteria) > 0 {
		fmt.Fprintf(&b, "\nIt satisfies: %s\n", strings.Join(req.Criteria, ", "))
	}
	b.WriteString("\nWrite the test first: no behaviour is implemented before the test mapped\n")
	b.WriteString("to its acceptance criterion exists and fails.\n")
	if len(req.Commands) > 0 {
		b.WriteString("\nYour work is judged by these commands, in this order:\n")
		for _, name := range []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"} {
			if cmd := req.Commands[name]; cmd != "" {
				fmt.Fprintf(&b, "  %-9s %s\n", name+":", cmd)
			}
		}
	}
	return b.String()
}
