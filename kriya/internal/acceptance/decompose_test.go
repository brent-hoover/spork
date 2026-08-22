//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// registerDecompose wires REQ-decompose.
func registerDecompose(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^no epic exists yet for the build target$`, func() error {
		if err := w.copyLinkshort(); err != nil {
			return err
		}
		w.citeCriteria("AC-valid-url")
		if _, found, err := w.targets.Find(context.Background(), w.dir); err != nil || found {
			return fmt.Errorf("the target already has a row")
		}
		return nil
	})
	sc.Given(`^a pinned SpecSnapshot for project "([^"]*)"$`, func(string) error {
		if err := w.copyLinkshort(); err != nil {
			return err
		}
		w.citeCriteria("AC-valid-url", "REQ-create-link")
		return nil
	})

	sc.When(`^the first decomposition runs$`, func() error { return w.runOnce() })
	sc.When(`^the PM agent decomposes it$`, func() error { return w.runOnce() })
	sc.When(`^a later decomposition supersedes the plan$`, func() error {
		// A second run under a NEW token: a deliberate re-intake, which is
		// what supersession is. The epic must not be recreated.
		w.token = "token-2"
		w.tracker.reset()
		return w.runOnce()
	})

	sc.Then(`^a BuildTarget row is written ahead of the epic's creation and records the epic id when it returns$`,
		func() error { return w.assertTargetRecordsEpic() })
	sc.Then(`^an umbrella epic is created exactly once — a recovered replay reuses the recorded row — and every ticket is parented under it$`,
		func() error { return w.assertOneEpicEveryTicketParented() })
	sc.Then(`^carried-forward and new tickets sit under the same epic with no re-parenting$`,
		func() error { return w.assertEpicUnchangedAcrossRuns() })
	sc.Then(`^every ticket's acceptance criteria cite REQ and AC ids present in the snapshot$`,
		func() error { return w.assertCitationsAreInTheSnapshot() })
}

// runOnce points kriya at the project and requires it to succeed.
func (w *world) runOnce() error {
	if err := w.run(); err != nil {
		return err
	}
	if w.err != nil {
		return fmt.Errorf("build failed: %w", w.err)
	}
	return nil
}

func (w *world) target() (planner.BuildTarget, error) {
	t, found, err := w.targets.Find(context.Background(), w.dir)
	if err != nil {
		return planner.BuildTarget{}, err
	}
	if !found {
		return planner.BuildTarget{}, fmt.Errorf("no BuildTarget row for %s", w.dir)
	}
	return t, nil
}

func (w *world) assertTargetRecordsEpic() error {
	t, err := w.target()
	if err != nil {
		return err
	}
	if t.EpicID == "" {
		return fmt.Errorf("the row records no epic id")
	}
	if t.EpicState != planner.EpicCreated {
		return fmt.Errorf("epic_state is %q, want %q", t.EpicState, planner.EpicCreated)
	}
	// Written AHEAD: the row must carry everything a replay needs to present
	// the same idempotency key, or recovery creates a duplicate instead of
	// replaying.
	for name, value := range map[string]string{
		"project key": t.ProjectKey, "name": t.Name, "actor": t.Actor, "spec hash": t.SpecHash,
	} {
		if value == "" {
			return fmt.Errorf("the row records no %s, so a replay could not rebuild its call", name)
		}
	}
	return nil
}

func (w *world) assertOneEpicEveryTicketParented() error {
	if w.tracker.epics != 1 {
		return fmt.Errorf("created %d epics, want exactly one", w.tracker.epics)
	}
	tickets := w.tracker.issues - w.tracker.epics
	if tickets < 1 {
		return fmt.Errorf("no tracer tickets were created")
	}
	// One parent_of per ticket, and no more: a ticket parented twice would
	// bump the epic's subtree revision for nothing.
	if w.tracker.relations != tickets {
		return fmt.Errorf("%d tickets but %d parent relations", tickets, w.tracker.relations)
	}
	return nil
}

func (w *world) assertEpicUnchangedAcrossRuns() error {
	t, err := w.target()
	if err != nil {
		return err
	}
	if t.EpicID != w.firstEpicID {
		return fmt.Errorf("the epic changed across runs: %q then %q", w.firstEpicID, t.EpicID)
	}
	// No re-parenting of the epic itself, and no second epic.
	if w.tracker.epics != 0 {
		return fmt.Errorf("the later decomposition created %d epics", w.tracker.epics)
	}
	return nil
}

func (w *world) assertCitationsAreInTheSnapshot() error {
	snap, err := w.store.only()
	if err != nil {
		return err
	}
	manifest, ok := snap.Content["avspec.yaml"]
	if !ok {
		return fmt.Errorf("the snapshot holds no manifest to cite against")
	}
	if len(w.tickets) == 0 {
		return fmt.Errorf("no tickets were produced")
	}
	for _, t := range w.tickets {
		if len(t.Criteria) == 0 {
			return fmt.Errorf("ticket %q cites nothing", t.Title)
		}
		for _, id := range t.Criteria {
			// Against the SNAPSHOT, not the working tree: an edit after
			// intake must not retroactively make a bad citation look valid.
			if !strings.Contains(manifest, id) {
				return fmt.Errorf("ticket %q cites %q, absent from the pinned manifest", t.Title, id)
			}
		}
	}
	return nil
}
