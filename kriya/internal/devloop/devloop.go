// Package devloop runs one pair-programming session: a dev agent working in
// an isolated workspace on one ticket.
//
// The full loop — commit cadence, roborev rounds, fix, recommit — lands with
// the review bridge. What is here is the invocation itself: the agent seam,
// the per-ticket toolset, and the session record the transcript imports under.
package devloop

import (
	"context"
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
	Ended         bool
}

// Store persists sessions.
type Store interface {
	Upsert(ctx context.Context, s Session) error
	// Importing lists sessions whose transcript import a crash left in
	// flight. Recovery replays them under their persisted key.
	Importing(ctx context.Context) ([]Session, error)
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
	// RoundLimit is the limit snapshotted onto the BuildRun at its creation.
	// Zero falls back to the loop's default.
	RoundLimit int
	// AllowRules are the generated permission rules. No bare Bash: what an
	// agent may run is the ticket's business, and "tools irrelevant to the
	// ticket are absent" has to be mechanically true rather than requested.
	AllowRules []string
}

// Work runs one session.
//
// The session is recorded BEFORE the agent is invoked and stamped ended after,
// so a crash mid-session leaves a row recovery can terminate and import the
// transcript from — AC-thread-no-loss requires every outcome to reach the
// thread catalog, crashed included.
func (l Loop) Work(ctx context.Context, req Request) (Session, error) {
	session := Session{Run: req.Run, Ticket: req.Ticket}
	if err := l.Store.Upsert(ctx, session); err != nil {
		return Session{}, fmt.Errorf("record session: %w", err)
	}

	systemFile, err := l.assembleContext(ctx, req)
	if err != nil {
		return Session{}, err
	}
	session.SystemFile = systemFile

	implement := prompt(req)
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
	turns := []Exchange{{
		Role: string(agent.RoleDev), Prompt: implement, Reply: res.Text, Model: res.Model,
	}}

	session.SessionID = res.SessionID
	session.Model = res.Model

	if l.Commit != nil && l.Review != nil {
		if err := l.pair(ctx, req, &session, &turns); err != nil {
			return Session{}, err
		}
	}

	// Captured before the session is stamped ended: AC-thread-no-loss wants
	// every outcome in the catalog, and a row stamped ended with no import
	// would look settled while its conversation was nowhere.
	if err := l.capture(ctx, req, &session, turns); err != nil {
		return Session{}, err
	}

	session.Ended = true
	if err := l.Store.Upsert(ctx, session); err != nil {
		return Session{}, fmt.Errorf("record ended session: %w", err)
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
func (l Loop) pair(ctx context.Context, req Request, session *Session, turns *[]Exchange) error {
	rounds := l.roundLimit(req)
	direction, err := l.direction(ctx, req.Run)
	if err != nil {
		return err
	}
	// Findings since the last clean pass. A clean pass resets it, which is
	// what makes the limit "consecutive rounds" rather than "rounds".
	var consecutive []string
	// answered is a round whose findings the agent has been given but whose
	// response cannot be written yet: the honest answer names the commit that
	// addressed it, and that commit does not exist until the next round.
	var answered *reviewbridge.Round
	for round := range rounds {
		sha, err := l.Commit.Commit(ctx, req.Workspace,
			fmt.Sprintf("%s (round %d)", req.Title, round+1))
		if err != nil {
			return fmt.Errorf("commit round %d: %w", round+1, err)
		}
		session.Commits = append(session.Commits, sha)

		if answered != nil {
			// Not cleared afterwards: every path from here either returns or
			// assigns the round this pass produced.
			if err := l.Review.Settle(ctx, *answered, "Addressed in "+sha+"."); err != nil {
				return err
			}
		}

		roundID := fmt.Sprintf("%s-%d", req.Run, round+1)
		reviewed, err := l.Review.Submit(ctx, req.Run, roundID, sha)
		if err != nil {
			return err
		}
		reviewed, err = l.Review.Poll(ctx, reviewed)
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
			return nil
		}

		consecutive = append(consecutive, reviewed.Findings)
		// Captured HERE, at the moment of correction — not reconstructed
		// afterwards, when what actually went wrong is a guess.
		if err := l.learn(ctx, req, sha, reviewed); err != nil {
			return err
		}
		fix := fixPrompt(req, reviewed.Findings, direction)
		res, err := l.Agent.Run(ctx, agent.Request{
			Role:       agent.RoleDev,
			Prompt:     fix,
			Workspace:  req.Workspace,
			SystemFile: session.SystemFile,
			AllowRules: req.AllowRules,
		})
		if err != nil {
			return fmt.Errorf("dev agent fixing round %d: %w", round+1, err)
		}
		*turns = append(*turns, Exchange{
			Role: string(agent.RoleDev), Prompt: fix, Reply: res.Text, Model: res.Model,
		})
		session.SessionID = res.SessionID
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

// impasse hands a stalled run to the architect.
//
// The last round stays OPEN — it is the evidence of what was asked for and
// never answered.
func (l Loop) impasse(ctx context.Context, req Request, session *Session,
	findings []string, rounds int) error {
	if l.Architect == nil {
		return fmt.Errorf("review still reporting findings after %d rounds", rounds)
	}
	if _, err := l.Architect.Resolve(ctx, req.Run, req.Ticket,
		architect.TriggerRoundLimit, findings, session.SystemFile); err != nil {
		return err
	}
	return fmt.Errorf("run %s paused for the architect after %d rounds", req.Run, rounds)
}

// learn records what a review finding taught.
//
// The TRIGGERING commit, not the fix: the lesson is about the code that was
// reviewed, and a later run matching this module wants to know what went wrong
// there rather than where it was patched.
func (l Loop) learn(ctx context.Context, req Request, commit string, round reviewbridge.Round) error {
	if l.Learnings == nil {
		return nil
	}
	module := req.Ticket
	if len(req.Modules) > 0 {
		module = req.Modules[0]
	}
	err := l.Learnings.Record(ctx, kctx.Capture{
		Scope: kctx.ScopeProject, ProjectKey: req.ProjectKey,
		Lesson: round.Findings, Module: module, Pattern: "review-finding",
		SourceKind: kctx.SourceReviewFinding, SourceRun: req.Run,
		SourceCommit: commit, SourceRef: round.ID, SourceSession: req.Run,
	})
	if err != nil {
		return fmt.Errorf("record learning for %s: %w", req.Ticket, err)
	}
	return nil
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
func prompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement this ticket in the workspace.\n\nTitle: %s\n", req.Title)
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
