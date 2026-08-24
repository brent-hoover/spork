//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/agent"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/reviewbridge"
)

// fakeRoborev records what kriya asked of roborev.
//
// A fake, because these scenarios are about kriya's protocol — what it
// enqueues, what it responds, what it refuses to touch — not about roborev's
// judgement, which has its own tests. Every call is recorded IN ORDER, since
// "the comment lands before the close" is an ordering claim.
type fakeRoborev struct {
	nextJob int
	calls   []string
	// verdicts is the report returned per poll, in order. Exhausted means
	// clean.
	reports []string
	closed  map[int]bool
}

func newFakeRoborev() *fakeRoborev {
	return &fakeRoborev{nextJob: 100, closed: map[int]bool{}}
}

func (f *fakeRoborev) Enqueue(_ context.Context, _, commit string) (int, error) {
	f.nextJob++
	f.calls = append(f.calls, fmt.Sprintf("enqueue %s -> %d", commit, f.nextJob))
	return f.nextJob, nil
}

func (f *fakeRoborev) Status(_ context.Context, _ string, jobID int) (string, string, error) {
	report := "No issues found."
	if len(f.reports) > 0 {
		report, f.reports = f.reports[0], f.reports[1:]
	}
	f.calls = append(f.calls, fmt.Sprintf("status %d", jobID))
	return "done", report, nil
}

func (f *fakeRoborev) Comment(_ context.Context, _ string, jobID int, message string) error {
	f.calls = append(f.calls, fmt.Sprintf("comment %d %s", jobID, message))
	return nil
}

func (f *fakeRoborev) Close(_ context.Context, _ string, jobID int) error {
	f.calls = append(f.calls, fmt.Sprintf("close %d", jobID))
	f.closed[jobID] = true
	return nil
}

func (f *fakeRoborev) Closed(_ context.Context, _ string, jobID int) (bool, error) {
	return f.closed[jobID], nil
}

// memRounds is the bridge's store.
type memRounds struct {
	rounds   []reviewbridge.Round
	attempts []reviewbridge.EnqueueAttempt
	// order records writes against calls, so "written ahead of its enqueue"
	// is checkable rather than assumed.
	writes []string
	rev    *fakeRoborev
}

func (m *memRounds) UpsertAttempt(_ context.Context, a reviewbridge.EnqueueAttempt) error {
	m.writes = append(m.writes, fmt.Sprintf("attempt %s %s at call %d", a.Round, a.State, len(m.rev.calls)))
	for i, existing := range m.attempts {
		if existing.Round == a.Round {
			m.attempts[i] = a
			return nil
		}
	}
	m.attempts = append(m.attempts, a)
	return nil
}

func (m *memRounds) UpsertRound(_ context.Context, r reviewbridge.Round) error {
	m.writes = append(m.writes, fmt.Sprintf("round %s %s at call %d", r.ID, r.State, len(m.rev.calls)))
	for i, existing := range m.rounds {
		if existing.ID == r.ID {
			m.rounds[i] = r
			return nil
		}
	}
	m.rounds = append(m.rounds, r)
	return nil
}

func (m *memRounds) Unresolved(context.Context) ([]reviewbridge.EnqueueAttempt, error) {
	var out []reviewbridge.EnqueueAttempt
	for _, a := range m.attempts {
		if a.State != reviewbridge.AttemptResolved {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *memRounds) Unsettled(context.Context) ([]reviewbridge.Round, error) {
	var out []reviewbridge.Round
	for _, r := range m.rounds {
		if r.State == reviewbridge.RoundCommenting || r.State == reviewbridge.RoundClosing {
			out = append(out, r)
		}
	}
	return out, nil
}

// seqCommitter names commits C1, C2, ... so the scenarios can talk about them.
type seqCommitter struct{ n int }

func (c *seqCommitter) Commit(context.Context, string, string) (string, error) {
	c.n++
	return fmt.Sprintf("C%d", c.n), nil
}

// callsAt reports the commits made, so a scenario can count rounds.
func (c *seqCommitter) callsAt() []int {
	out := make([]int, 0, c.n)
	for i := 1; i <= c.n; i++ {
		out = append(out, i)
	}
	return out
}

// pairWorld is one pair-loop scenario's state.
type pairWorld struct {
	rev       *fakeRoborev
	store     *memRounds
	bridge    reviewbridge.Bridge
	committer *seqCommitter
	agent     *fakes.Agent
	session   devloop.Session
	err       error
}

func (w *world) newPair() *pairWorld {
	rev := newFakeRoborev()
	store := &memRounds{rev: rev}
	p := &pairWorld{
		rev: rev, store: store, committer: &seqCommitter{},
		agent: &fakes.Agent{
			Replies: []agent.Result{{SessionID: "session-1", Model: "test-model"}},
			Repeat:  true,
		},
	}
	p.bridge = reviewbridge.Bridge{
		Repo: "/repo", Store: store, Rev: rev, Now: fakes.NewClock(time.Unix(0, 0)),
	}
	w.pair = p
	return p
}

func (p *pairWorld) run(maxRounds int) error {
	loop := devloop.Loop{
		Agent: p.agent, Store: &nullSessions{}, Commit: p.committer,
		Review: p.bridge, Now: fakes.NewClock(time.Unix(0, 0)), MaxRounds: maxRounds,
	}
	session, err := loop.Work(context.Background(), devloop.Request{
		Run: "run-1", Ticket: "KRI-1", Title: "Create a short link",
		Workspace: "/wt/kri-1",
	})
	p.session, p.err = session, err
	return err
}

// nullSessions accepts session writes; these scenarios are about the review
// protocol, and dev_session persistence has its own tests.
type nullSessions struct{}

func (nullSessions) Upsert(context.Context, devloop.Session) error { return nil }

func (nullSessions) Importing(context.Context) ([]devloop.Session, error) { return nil, nil }

func (nullSessions) Find(context.Context, string) (devloop.Session, bool, error) {
	return devloop.Session{}, false, nil
}

func registerPairLoop(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a dev agent working a ticket in its workspace$`, func() error {
		w.newPair()
		return nil
	})

	sc.Step(`^it lands two commits$`, func() error {
		// One findings round then a clean one: two commits, each reviewed.
		w.pair.rev.reports = []string{"- **Severity**: Low\n- **Location**: `a.go:1`"}
		return w.pair.run(5)
	})

	sc.Step(`^each commit has its own ReviewRound, written ahead of its enqueue$`, func() error {
		rounds := w.pair.store.rounds
		if len(rounds) != 2 {
			return fmt.Errorf("got %d rounds for two commits", len(rounds))
		}
		seen := map[string]bool{}
		for _, r := range rounds {
			if seen[r.Commit] {
				return fmt.Errorf("two rounds share commit %s", r.Commit)
			}
			seen[r.Commit] = true
		}
		// The attempt for each round is written while no enqueue has yet been
		// made for it — that is what "written ahead" means, and the write log
		// records the call count at the time of each write.
		for _, write := range w.pair.store.writes {
			if strings.HasPrefix(write, "attempt run-1-1 pending") &&
				!strings.HasSuffix(write, "at call 0") {
				return fmt.Errorf("the first attempt was written after %s", write)
			}
		}
		return nil
	})

	sc.Step(`^no end-stage review covering the accumulated branch replaces them$`, func() error {
		var enqueues []string
		for _, call := range w.pair.rev.calls {
			if strings.HasPrefix(call, "enqueue ") {
				enqueues = append(enqueues, call)
			}
		}
		if len(enqueues) != 2 {
			return fmt.Errorf("enqueued %d reviews for two commits: %v", len(enqueues), enqueues)
		}
		if !strings.Contains(enqueues[0], "C1") || !strings.Contains(enqueues[1], "C2") {
			return fmt.Errorf("reviews were not enqueued per commit: %v", enqueues)
		}
		return nil
	})

	sc.Step(`^a round returns findings for commit "([^"]*)"$`, func(commit string) error {
		p := w.newPair()
		p.rev.reports = []string{"- **Severity**: Medium\n- **Location**: `" + commit + ".go:42`"}
		return p.run(5)
	})

	sc.Step(`^the findings are handed to the dev agent$`, func() error {
		if len(w.pair.agent.Requests) < 2 {
			return fmt.Errorf("the agent was invoked %d times; no fix round ran",
				len(w.pair.agent.Requests))
		}
		if !strings.Contains(w.pair.agent.Requests[1].Prompt, "C1.go:42") {
			return errors.New("the findings did not reach the agent intact")
		}
		return nil
	})

	sc.Step(`^the agent fixes and lands commit "([^"]*)"$`, func(commit string) error {
		if len(w.pair.session.Commits) < 2 || w.pair.session.Commits[1] != commit {
			return fmt.Errorf("commits are %v, want a second one named %s",
				w.pair.session.Commits, commit)
		}
		return nil
	})

	sc.Step(`^a new round is enqueued for "([^"]*)"$`, func(commit string) error {
		for _, call := range w.pair.rev.calls {
			if strings.HasPrefix(call, "enqueue "+commit+" ") {
				return nil
			}
		}
		return fmt.Errorf("no review was enqueued for %s: %v", commit, w.pair.rev.calls)
	})

	sc.Step(`^the loop continues until a round on the branch head returns no findings$`, func() error {
		if w.pair.err != nil {
			return fmt.Errorf("the loop did not reach a clean pass: %w", w.pair.err)
		}
		last := w.pair.store.rounds[len(w.pair.store.rounds)-1]
		if last.Verdict != reviewbridge.VerdictClean {
			return fmt.Errorf("the loop exited on verdict %q", last.Verdict)
		}
		return nil
	})

	sc.Step(`^a round's findings have been addressed$`, func() error {
		p := w.newPair()
		p.rev.reports = []string{"- **Severity**: Low"}
		return p.run(5)
	})

	sc.Step(`^kriya closes the round$`, func() error {
		if w.pair.err != nil {
			return w.pair.err
		}
		return nil
	})

	sc.Step(`^the response lifecycle advances write-ahead around each call — commenting before the comment, closing before the close$`, func() error {
		return assertWriteAhead(w.pair)
	})

	sc.Step(`^the response is recorded on the job as a comment before the close$`, func() error {
		for i, call := range w.pair.rev.calls {
			if !strings.HasPrefix(call, "close ") {
				continue
			}
			job := strings.TrimPrefix(call, "close ")
			for _, earlier := range w.pair.rev.calls[:i] {
				if strings.HasPrefix(earlier, "comment "+job+" ") {
					return nil
				}
			}
			return fmt.Errorf("job %s was closed with nothing said on it", job)
		}
		return errors.New("no round was closed")
	})

	sc.Step(`^the job's history holds the full conversation$`, func() error {
		var comments int
		for _, call := range w.pair.rev.calls {
			if strings.HasPrefix(call, "comment ") {
				comments++
			}
		}
		if comments != len(w.pair.store.rounds) {
			return fmt.Errorf("%d comments for %d rounds", comments, len(w.pair.store.rounds))
		}
		return nil
	})

	sc.Step(`^a round crashed in state "commenting" with its prepared response payload persisted in the same transaction$`, func() error {
		p := w.newPair()
		p.store.rounds = []reviewbridge.Round{{
			ID: "run-1-1", Run: "run-1", Commit: "C1", JobID: 101,
			Verdict: reviewbridge.VerdictFindings,
			State:   reviewbridge.RoundCommenting, Response: "Addressed in C2.",
		}}
		return nil
	})

	// "recovery runs" is a phrase several features share, and godog matches
	// on the sentence alone. It dispatches on whichever world the scenario's
	// Given built: registering it twice would be ambiguous, and owning it
	// here would panic in every other feature that says it.
	sc.Step(`^recovery runs$`, func() error { return w.recover() })

	sc.Step(`^exactly the stored payload is re-issued as the comment — a rare duplicate is benign, a missing or differing response is not — and the close completes$`, func() error {
		want := "comment 101 Addressed in C2."
		for _, call := range w.pair.rev.calls {
			if call == want {
				if w.pair.rev.closed[101] {
					return nil
				}
				return errors.New("the comment was re-issued but the close never completed")
			}
		}
		return fmt.Errorf("calls were %v, want %q", w.pair.rev.calls, want)
	})

	sc.Step(`^a round crashed in state "closing"$`, func() error {
		p := w.newPair()
		p.store.rounds = []reviewbridge.Round{{
			ID: "run-1-2", Run: "run-1", Commit: "C2", JobID: 102,
			Verdict: reviewbridge.VerdictClean,
			State:   reviewbridge.RoundClosing, Response: "Clean pass at C2.",
		}}
		if _, err := p.bridge.RecoverRounds(context.Background()); err != nil {
			return err
		}
		for _, call := range p.rev.calls {
			if strings.HasPrefix(call, "comment ") {
				return errors.New("a round past commenting was commented on again")
			}
		}
		return nil
	})

	sc.Step(`^recovery re-issues the close, treating already-closed as success$`, func() error {
		if !w.pair.rev.closed[102] {
			return errors.New("the close was never re-issued")
		}
		// Already closed, and a second recovery must still succeed.
		if _, err := w.pair.bridge.RecoverRounds(context.Background()); err != nil {
			return fmt.Errorf("a re-issued close on a closed job failed: %w", err)
		}
		return nil
	})

	sc.Step(`^no round is ever left open by the crash window$`, func() error {
		open, err := w.pair.store.Unsettled(context.Background())
		if err != nil {
			return err
		}
		if len(open) != 0 {
			return fmt.Errorf("%d rounds are still mid-response after recovery", len(open))
		}
		return nil
	})
}

// assertWriteAhead checks that every state was persisted before the call that
// performs it.
func assertWriteAhead(p *pairWorld) error {
	commentAt, closeAt := -1, -1
	for i, call := range p.rev.calls {
		if strings.HasPrefix(call, "comment ") && commentAt < 0 {
			commentAt = i
		}
		if strings.HasPrefix(call, "close ") && closeAt < 0 {
			closeAt = i
		}
	}
	if commentAt < 0 || closeAt < 0 {
		return fmt.Errorf("the lifecycle did not run: %v", p.rev.calls)
	}
	if err := writeBefore(p, reviewbridge.RoundCommenting, commentAt); err != nil {
		return err
	}
	return writeBefore(p, reviewbridge.RoundClosing, closeAt)
}

// writeBefore requires a round to have reached state before call index n.
func writeBefore(p *pairWorld, state string, n int) error {
	want := fmt.Sprintf(" %s at call ", state)
	for _, write := range p.store.writes {
		if !strings.Contains(write, want) {
			continue
		}
		var at int
		if _, err := fmt.Sscanf(write[strings.LastIndex(write, "at call ")+8:], "%d", &at); err != nil {
			return err
		}
		if at <= n {
			return nil
		}
	}
	return fmt.Errorf("no write reached %q before call %d: %v", state, n, p.store.writes)
}
