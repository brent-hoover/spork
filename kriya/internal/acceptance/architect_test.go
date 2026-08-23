//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/agent"
	"kriya/internal/architect"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
	"kriya/internal/reviewbridge"
)

// interventions is the architect's store for these scenarios.
type interventions struct {
	rows map[string]architect.Intervention
}

func (i *interventions) Upsert(_ context.Context, in architect.Intervention) error {
	i.rows[in.Build] = in
	return nil
}

func (i *interventions) Find(_ context.Context, build string) (architect.Intervention, bool, error) {
	in, ok := i.rows[build]
	return in, ok, nil
}

// saWorld is one architect scenario's state.
type saWorld struct {
	store     *interventions
	sa        architect.Architect
	saAgent   *fakes.Agent
	devAgent  *fakes.Agent
	sessions  *sessionStore
	rev       *fakeRoborev
	bridge    reviewbridge.Bridge
	committer *seqCommitter
	work      string
	run       orchestrator.BuildRun
	// configured is the limit the operator has since changed it to.
	configured int
	err        error
	before     string
}

func (w *world) newSA(direction string) (*saWorld, error) {
	dir, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	store := &interventions{rows: map[string]architect.Intervention{}}
	saAgent := &fakes.Agent{
		Replies: []agent.Result{{SessionID: "sa-1", Model: "test-model", Text: direction}},
		Repeat:  true,
	}
	rev := newFakeRoborev()
	s := &saWorld{
		store: store, saAgent: saAgent, work: dir, rev: rev,
		bridge: reviewbridge.Bridge{
			Repo: "/repo", Store: &memRounds{rev: rev}, Rev: rev,
			Now: fakes.NewClock(time.Unix(0, 0)),
		},
		sessions:  &sessionStore{rows: map[string]devloop.Session{}},
		committer: &seqCommitter{},
		devAgent: &fakes.Agent{
			Replies: []agent.Result{{SessionID: "dev-1", Model: "test-model", Text: "done"}},
			Repeat:  true,
		},
		sa: architect.Architect{
			Agent: saAgent, Store: store, Now: fakes.NewClock(time.Unix(0, 0)),
		},
	}
	w.sa = s
	return s, nil
}

// loop builds the dev loop as the composition root would, with the run's own
// snapshotted limit rather than any configured default.
func (s *saWorld) loop() devloop.Loop {
	return devloop.Loop{
		Agent: s.devAgent, Store: s.sessions, Commit: s.committer,
		Review: s.bridge, Architect: s.sa, MaxRounds: s.configured,
		Now: fakes.NewClock(time.Unix(0, 0)),
	}
}

func (s *saWorld) work2() devloop.Request {
	return devloop.Request{
		Run: s.run.ID, Ticket: s.run.Ticket, Title: s.run.Ticket,
		Workspace: s.work, RoundLimit: s.run.RoundLimit,
	}
}

func registerArchitect(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a round limit of (\d+) was snapshotted onto the BuildRun at its creation$`,
		func(limit int) error {
			s, err := w.newSA("Split the handler; the store is not the problem.")
			if err != nil {
				return err
			}
			s.run = orchestrator.BuildRun{
				ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateDevLoop,
				RoundLimit: limit,
			}
			return nil
		})

	sc.Step(`^the operator has since changed the configured limit to (\d+)$`, func(limit int) error {
		w.sa.configured = limit
		return nil
	})

	sc.Step(`^kriya restarts and recovers the run$`, func() error {
		// A restart reads the run back from its row. Nothing about the
		// configured limit travels with it.
		s := w.sa
		db := &memOrchStore{rows: map[string]orchestrator.BuildRun{}}
		if err := db.Upsert(context.Background(), s.run); err != nil {
			return err
		}
		got, found, err := db.Find(context.Background(), "run-1")
		if err != nil || !found {
			return fmt.Errorf("find: %v found=%v", err, found)
		}
		s.run = got
		return nil
	})

	sc.Step(`^the persisted limit of (\d+) still governs it — the config change affects only future runs$`,
		func(limit int) error {
			if w.sa.run.RoundLimit != limit {
				return fmt.Errorf("the run carries a limit of %d", w.sa.run.RoundLimit)
			}
			if w.sa.configured == limit {
				return errors.New("the configured limit was never changed, so the claim is untested")
			}
			return nil
		})

	sc.Step(`^three consecutive rounds have returned findings since the last clean pass$`,
		func() error {
			w.sa.rev.reports = findingReports(6, "- **Severity**: High\n- **Location**: `a.go:1`")
			return nil
		})

	sc.Step(`^the fourth consecutive round returns findings$`, func() error {
		s := w.sa
		_, s.err = s.loop().Work(context.Background(), s.work2())
		return nil
	})

	sc.Step(`^the run pauses and the impasse reaches the SA agent$`, func() error {
		s := w.sa
		if s.err == nil {
			return errors.New("the run did not pause")
		}
		in, found := s.store.rows["run-1"]
		if !found {
			return errors.New("no intervention was recorded")
		}
		if in.Trigger != architect.TriggerRoundLimit {
			return fmt.Errorf("the trigger is %q", in.Trigger)
		}
		if len(s.committer.callsAt()) != 4 {
			return fmt.Errorf("ran %d rounds, want the snapshotted 4", len(s.committer.callsAt()))
		}
		return nil
	})

	sc.Step(`^the SA receives the findings history, the ticket's acceptance criteria, and the bound snapshot$`,
		func() error {
			s := w.sa
			if len(s.saAgent.Requests) == 0 {
				return errors.New("the SA was never invoked")
			}
			prompt := s.saAgent.Requests[0].Prompt
			if !strings.Contains(prompt, "a.go:1") {
				return fmt.Errorf("the findings history is not in the prompt:\n%s", prompt)
			}
			var handed []string
			if err := json.Unmarshal(s.store.rows["run-1"].Findings, &handed); err != nil {
				return fmt.Errorf("the recorded history is unreadable: %w", err)
			}
			if len(handed) != 4 {
				return fmt.Errorf("handed over %d findings", len(handed))
			}
			// The criteria and the snapshot travel in the system file, which
			// is the assembled context bundle — the same one the dev agent
			// read, so the SA sees exactly what the dev agent was told.
			if s.saAgent.Requests[0].SystemFile != s.sessions.rows["run-1"].SystemFile {
				return errors.New("the SA was given a different context than the dev agent")
			}
			return nil
		})

	sc.Step(`^only two consecutive rounds have returned findings$`, func() error {
		s, err := w.newSA("direction")
		if err != nil {
			return err
		}
		s.run = orchestrator.BuildRun{ID: "run-1", Ticket: "KRI-1", RoundLimit: 4}
		s.rev.reports = findingReports(2, "- **Severity**: Low")
		_, s.err = s.loop().Work(context.Background(), s.work2())
		return nil
	})

	sc.Step(`^the loop continues without pausing$`, func() error {
		s := w.sa
		if s.err != nil {
			return fmt.Errorf("the loop stopped: %w", s.err)
		}
		if len(s.store.rows) != 0 {
			return errors.New("an intervention was recorded below the limit")
		}
		return nil
	})

	sc.Step(`^a clean pass lands mid-count$`, func() error {
		s, err := w.newSA("direction")
		if err != nil {
			return err
		}
		s.run = orchestrator.BuildRun{ID: "run-1", Ticket: "KRI-1", RoundLimit: 3}
		// Two findings, a clean pass: the counter must not carry the two past
		// the pass, or a converging run would pause.
		s.rev.reports = findingReports(2, "- **Severity**: Low")
		_, s.err = s.loop().Work(context.Background(), s.work2())
		return nil
	})

	sc.Step(`^the counter resets$`, func() error {
		s := w.sa
		if s.err != nil {
			return fmt.Errorf("a run that reached a clean pass stopped: %w", s.err)
		}
		if len(s.store.rows) != 0 {
			return errors.New("a run that reached a clean pass was sent to the architect")
		}
		return nil
	})

	sc.Step(`^a dev agent declares an impasse before the limit$`, func() error {
		s, err := w.newSA("direction")
		if err != nil {
			return err
		}
		s.run = orchestrator.BuildRun{ID: "run-2", Ticket: "KRI-2", RoundLimit: 9}
		// Declared, not counted: the architect is reached on the agent's own
		// say-so, through the same recorded path.
		_, err = s.sa.Resolve(context.Background(), "run-2", "KRI-2",
			architect.TriggerAgentDeclared, []string{"I cannot reconcile these two ACs"}, "")
		return err
	})

	sc.Step(`^the same pause and handoff occur$`, func() error {
		in, found := w.sa.store.rows["run-2"]
		if !found {
			return errors.New("no intervention was recorded for a declared impasse")
		}
		if in.Trigger != architect.TriggerAgentDeclared {
			return fmt.Errorf("the trigger is %q", in.Trigger)
		}
		if in.State != architect.StateDirected {
			return fmt.Errorf("the intervention is in state %q", in.State)
		}
		return nil
	})

	sc.Step(`^an impasse before the SA agent$`, func() error {
		s, err := w.newSA("Split the handler.")
		if err != nil {
			return err
		}
		s.run = orchestrator.BuildRun{ID: "run-1", Ticket: "KRI-1", RoundLimit: 2}
		s.rev.reports = findingReports(4, "- **Severity**: High")
		// Snapshot the worktree so "identical before and after" is decidable.
		if err := os.WriteFile(filepath.Join(s.work, "source.go"), []byte("package p\n"), 0o600); err != nil {
			return err
		}
		s.before = digestOf(s.work)
		return nil
	})

	sc.Step(`^the SA resolves it$`, func() error {
		s := w.sa
		_, s.err = s.loop().Work(context.Background(), s.work2())
		if s.err == nil {
			return errors.New("the run did not pause")
		}
		return nil
	})

	sc.Step(`^a durable Intervention row records the trigger, the findings handed over, the direction, and its lifecycle state$`,
		func() error {
			in, found := w.sa.store.rows["run-1"]
			if !found {
				return errors.New("no intervention was recorded")
			}
			if in.Trigger == "" || len(in.Findings) == 0 ||
				in.Direction == "" || in.State == "" {
				return fmt.Errorf("the row is incomplete: %+v", in)
			}
			return nil
		})

	sc.Step(`^resume and recovery read that row, never transient state$`, func() error {
		// A fresh Architect over the same store — nothing carried in memory.
		s := w.sa
		fresh := architect.Architect{
			Agent: s.saAgent, Store: s.store, Now: fakes.NewClock(time.Unix(0, 0)),
		}
		got, found, err := fresh.Resume(context.Background(), "run-1")
		if err != nil || !found {
			return fmt.Errorf("resume: %v found=%v", err, found)
		}
		if got.Direction != "Split the handler." {
			return fmt.Errorf("resumed with %q", got.Direction)
		}
		return nil
	})

	sc.Step(`^the SA's toolset contains no workspace-mutation tools$`, func() error {
		rules := strings.Join(w.sa.saAgent.Requests[0].AllowRules, ",")
		if rules == "" {
			return errors.New("no rules were passed, so the agent gets defaults")
		}
		for _, mutating := range []string{"Write", "Edit", "Bash", "MultiEdit", "NotebookEdit"} {
			if strings.Contains(rules, mutating) {
				return fmt.Errorf("the SA was granted %s", mutating)
			}
		}
		return nil
	})

	sc.Step(`^the worktree contents and branch head are identical before and after the SA's invocation$`,
		func() error {
			s := w.sa
			if s.before == "" {
				return errors.New("nothing was snapshotted before the invocation")
			}
			if got := digestOf(s.work); got != s.before {
				return errors.New("the worktree changed across the SA's invocation")
			}
			return nil
		})

	sc.Step(`^the SA recorded a direction for a paused run$`, func() error {
		s, err := w.newSA("Split the handler; the store is not the problem.")
		if err != nil {
			return err
		}
		s.run = orchestrator.BuildRun{ID: "run-1", Ticket: "KRI-1", RoundLimit: 4}
		return s.store.Upsert(context.Background(), architect.Intervention{
			Build: "run-1", Trigger: architect.TriggerRoundLimit,
			Findings:  json.RawMessage(`["- **Severity**: High"]`),
			Direction: "Split the handler; the store is not the problem.",
			State:     architect.StateDirected,
		})
	})

	sc.Step(`^the pair loop continues with the direction in the dev agent's context$`, func() error {
		s := w.sa
		if len(s.devAgent.Requests) < 2 {
			return fmt.Errorf("the dev agent ran %d times", len(s.devAgent.Requests))
		}
		if !strings.Contains(s.devAgent.Requests[1].Prompt, "Split the handler") {
			return fmt.Errorf("the direction is not in the prompt:\n%s",
				s.devAgent.Requests[1].Prompt)
		}
		return nil
	})

	sc.Step(`^the Intervention row preserves the trigger, direction, and eventual outcome through resume, restart, and recovery$`,
		func() error {
			s := w.sa
			in, found := s.store.rows["run-1"]
			if !found {
				return errors.New("the intervention row is gone after resume")
			}
			if in.Trigger != architect.TriggerRoundLimit {
				return fmt.Errorf("the trigger became %q", in.Trigger)
			}
			if in.Direction == "" {
				return errors.New("the direction was lost on resume")
			}
			if in.State != architect.StateResumed {
				return fmt.Errorf("the lifecycle is at %q", in.State)
			}
			return nil
		})
}

// resume continues a paused run under the architect's recorded direction.
func (s *saWorld) resume() error {
	s.rev.reports = findingReports(1, "- **Severity**: High")
	_, s.err = s.loop().Work(context.Background(), s.work2())
	return s.err
}

// findingReports scripts n rounds of findings, after which the fake roborev
// falls through to its clean default.
func findingReports(n int, body string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = body
	}
	return out
}

// memOrchStore round-trips a run, standing in for a restart.
//
// Deliberately NOT an orchestrator.Store: this scenario only needs a run to
// survive a write and a read, and implementing the whole interface would add
// a method nothing calls.
type memOrchStore struct {
	rows map[string]orchestrator.BuildRun
}

func (m *memOrchStore) Upsert(_ context.Context, r orchestrator.BuildRun) error {
	m.rows[r.ID] = r
	return nil
}

func (m *memOrchStore) Find(_ context.Context, id string) (orchestrator.BuildRun, bool, error) {
	r, ok := m.rows[id]
	return r, ok, nil
}

// digestOf summarises a directory's contents, so "identical before and after"
// is a fact rather than a claim.
func digestOf(dir string) string {
	var b strings.Builder
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fmt.Fprintf(&b, "%s:%d:%x\n", path, len(body), body)
		return nil
	})
	return b.String()
}
