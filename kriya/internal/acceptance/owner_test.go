//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/agent"
	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
	"kriya/internal/owner"
)

// validations is the PO's store for these scenarios.
type validations struct{ rows map[int]owner.Validation }

func (v *validations) Upsert(_ context.Context, got owner.Validation) error {
	v.rows[got.Attempt] = got
	return nil
}

func (v *validations) Find(_ context.Context, _ string, attempt int) (owner.Validation, bool, error) {
	got, ok := v.rows[attempt]
	return got, ok, nil
}

// chainResults answers the precondition question from a set of gates that
// passed, so a scenario can remove exactly one.
type chainResults struct {
	// missing is the gate that has not passed. Empty means all of them have.
	missing string
	asked   int
}

func (c *chainResults) AllPassed(
	context.Context, string, string, string, int,
) (bool, string, error) {
	c.asked++
	if c.missing != "" {
		return false, c.missing, nil
	}
	return true, "", nil
}

// poWorld is one PO scenario's state.
type poWorld struct {
	store  *validations
	gates  *chainResults
	agent  *fakes.Agent
	po     owner.Owner
	got    owner.Validation
	err    error
	ticket owner.Request
}

func (w *world) newPO(verdict, notes string) *poWorld {
	store := &validations{rows: map[int]owner.Validation{}}
	gates := &chainResults{}
	ag := &fakes.Agent{Replies: poVerdict(verdict, notes), Repeat: true}
	p := &poWorld{
		store: store, gates: gates, agent: ag,
		po: owner.Owner{
			Agent: ag, Store: store, Gates: gates, Now: fakes.NewClock(time.Unix(0, 0)),
		},
		ticket: owner.Request{
			Build: "run-1", Module: "MOD-a", Ticket: "KRI-1", Commit: "C2", Attempt: 1,
			Criteria: []string{"AC-one"},
		},
	}
	w.po = p
	return p
}

// poVerdict builds the PO agent's structured reply.
func poVerdict(verdict, notes string) []agent.Result {
	body, err := json.Marshal(map[string]string{"verdict": verdict, "notes": notes})
	if err != nil {
		panic(err)
	}
	return []agent.Result{{SessionID: "po-1", Model: "test-model", Structured: body}}
}

func (p *poWorld) validate() {
	p.got, p.err = p.po.Validate(context.Background(), p.ticket)
}

func registerOwner(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a run whose head commit has passed every machine gate — review, test, structure, typing, arch, branch-coverage, and mutation$`,
		func() error {
			w.newPO(owner.VerdictSatisfied, "every AC has a test that exercises it")
			return nil
		})

	sc.Step(`^PO validation begins$`, func() error {
		p := w.po
		p.validate()
		if p.err != nil {
			return fmt.Errorf("validation refused a fully gated run: %w", p.err)
		}
		if p.gates.asked == 0 {
			return errors.New("the gates were never consulted")
		}
		return nil
	})

	sc.Step(`^a run missing any of those gates at the head commit$`, func() error {
		p := w.newPO(owner.VerdictSatisfied, "fine")
		p.gates.missing = "mutation"
		return nil
	})

	sc.Step(`^PO validation does not run and cannot be reordered around the gap$`, func() error {
		p := w.po
		p.validate()
		if p.err == nil {
			return errors.New("validation ran with a gate outstanding")
		}
		if !strings.Contains(p.err.Error(), "mutation") {
			return fmt.Errorf("the refusal does not name the gap: %v", p.err)
		}
		// Not reorderable: the refusal happens inside the owner, so a caller
		// that skipped the check cannot produce a verdict anyway.
		if len(p.agent.Requests) != 0 {
			return errors.New("the product owner was invoked for ungated code")
		}
		if len(p.store.rows) != 0 {
			return errors.New("a verdict was recorded for ungated code")
		}
		return nil
	})

	sc.Step(`^a ticket with three acceptance criteria$`, func() error {
		p := w.newPO(owner.VerdictSatisfied, "each AC is exercised by a real test")
		p.ticket.Criteria = []string{"AC-one", "AC-two", "AC-three"}
		return nil
	})

	sc.Step(`^the PO validates the run$`, func() error {
		w.po.validate()
		return nil
	})

	sc.Step(`^each AC is matched to a test that genuinely exercises it$`, func() error {
		prompt := w.po.agent.Requests[0].Prompt
		for _, id := range w.po.ticket.Criteria {
			if !strings.Contains(prompt, id) {
				return fmt.Errorf("%s was not put to the product owner:\n%s", id, prompt)
			}
		}
		if !strings.Contains(prompt, "genuinely exercises it") {
			return errors.New("the prompt does not ask whether the test exercises the AC")
		}
		return nil
	})

	sc.Step(`^the implementation is judged against the AC's intent within the whole spec$`, func() error {
		p := w.po
		prompt := p.agent.Requests[0].Prompt
		if !strings.Contains(prompt, "INTENT") || !strings.Contains(prompt, "whole spec") {
			return fmt.Errorf("the prompt does not ask about intent:\n%s", prompt)
		}
		// The whole spec's law travels in the system file — the same bundle
		// the dev agent read, which is what makes the PO able to judge intent
		// rather than only wording.
		if p.agent.Requests[0].SystemFile != p.ticket.SystemFile {
			return errors.New("the product owner was given a different context")
		}
		return nil
	})

	sc.Step(`^an AC satisfied by letter but not intent fails validation$`, func() error {
		p := w.newPO(owner.VerdictNotSatisfied,
			"AC-two is met by the wording but the redirect never fires")
		p.ticket.Criteria = []string{"AC-one", "AC-two", "AC-three"}
		p.validate()
		if p.err != nil {
			return p.err
		}
		if p.got.Passed() {
			return errors.New("a letter-only AC passed validation")
		}
		return nil
	})

	sc.Step(`^tests that assert tautologies, or an implementation special-casing test inputs, or assertions weakened since an earlier commit$`,
		func() error {
			w.newPO(owner.VerdictTestsGamed,
				"test_valid_url asserts True == True; shorten() special-cases \"http://x\"")
			return nil
		})

	sc.Step(`^validation fails naming the gaming evidence found$`, func() error {
		p := w.po
		if p.err != nil {
			return p.err
		}
		if p.got.Passed() {
			return errors.New("a gamed run passed validation")
		}
		if p.got.Verdict != owner.VerdictTestsGamed {
			return fmt.Errorf("the verdict is %q", p.got.Verdict)
		}
		if !strings.Contains(p.store.rows[1].Notes, "special-cases") {
			return fmt.Errorf("the evidence was not recorded: %q", p.store.rows[1].Notes)
		}
		// Named explicitly in the prompt, not left to judgement: a PO asked
		// only "is this good" reliably answers yes to a tautology.
		prompt := p.agent.Requests[0].Prompt
		for _, pattern := range []string{"tautolog", "special-cases test inputs", "weakened or deleted"} {
			if !strings.Contains(prompt, pattern) {
				return fmt.Errorf("the prompt does not name %q as gaming", pattern)
			}
		}
		return nil
	})

	sc.Step(`^the PO passes a run$`, func() error {
		p := w.newPO(owner.VerdictSatisfied, "every AC is exercised and the intent holds")
		p.validate()
		return p.err
	})

	sc.Step(`^the run advances to review submission and the verdict with its reasons is durably recorded$`,
		func() error {
			p := w.po
			if !p.got.Passed() {
				return fmt.Errorf("the verdict is %q", p.got.Verdict)
			}
			saved, found, err := p.store.Find(context.Background(), "run-1", 1)
			if err != nil || !found {
				return fmt.Errorf("no verdict recorded: %v found=%v", err, found)
			}
			if saved.Notes == "" || saved.Commit != "C2" {
				return fmt.Errorf("the record is incomplete: %+v", saved)
			}
			// The table, not this module, decides where a pass goes: on to
			// submission, and from there to review-submitted.
			if err := transitionFrom(orchestrator.StatePOValidation,
				orchestrator.StateSubmitting, orchestrator.StateDevLoop); err != nil {
				return err
			}
			return transitionFrom(orchestrator.StateSubmitting,
				orchestrator.StateReviewSubmitted, orchestrator.StateAwaitingOperator)
		})

	sc.Step(`^the PO fails a run$`, func() error {
		p := w.newPO(owner.VerdictNotSatisfied, "AC-two has no test that exercises it")
		p.validate()
		return p.err
	})

	sc.Step(`^the findings return to the dev agent, the pair loop resumes, and the verdict survives in the run's history$`,
		func() error {
			p := w.po
			if p.got.Passed() {
				return errors.New("a failed validation reported as a pass")
			}
			saved, found, err := p.store.Find(context.Background(), "run-1", 1)
			if err != nil || !found {
				return fmt.Errorf("no verdict recorded: %v found=%v", err, found)
			}
			if !strings.Contains(saved.Notes, "AC-two") {
				return fmt.Errorf("the findings were not recorded: %q", saved.Notes)
			}
			return transitionFrom(orchestrator.StatePOValidation,
				orchestrator.StateSubmitting, orchestrator.StateDevLoop)
		})
}

// transitionFrom checks the table routes a state as the requirement says.
func transitionFrom(from, onOK, onFail orchestrator.State) error {
	tr, found := orchestrator.Lookup(from)
	if !found {
		return fmt.Errorf("no transition owns %q", from)
	}
	if tr.OnOK != onOK {
		return fmt.Errorf("a pass from %q goes to %q, want %q", from, tr.OnOK, onOK)
	}
	if tr.OnFail != onFail {
		return fmt.Errorf("a failure from %q goes to %q, want %q", from, tr.OnFail, onFail)
	}
	return nil
}
