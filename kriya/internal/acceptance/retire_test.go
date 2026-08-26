//go:build acceptance

package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// retireWorld is one retirement scenario's state.
type retireWorld struct {
	head     *headWorld
	tickets  *worldTickets
	defers   *recordingDeferrer
	live     *liveClaims
	retire   planner.Retirement
	previous planner.Plan
	err      error
}

// recordingDeferrer stands in for sutra's conditional status transition.
//
// It honours expected_status the way sutra does — AC-status-conditional, a
// mismatch is a conflict — because retirement's whole claim is that it expects
// the status it FRESHLY observed. A fake that ignored the expectation would
// pass whatever kriya sent.
type recordingDeferrer struct {
	status    map[string]string
	deferred  []string
	expected  map[string]string
	keys      []string
	conflicts int
}

func newRecordingDeferrer() *recordingDeferrer {
	return &recordingDeferrer{status: map[string]string{}, expected: map[string]string{}}
}

func (d *recordingDeferrer) Status(_ context.Context, issue string) (string, error) {
	status, ok := d.status[issue]
	if !ok {
		return planner.StatusOpen, nil
	}
	return status, nil
}

func (d *recordingDeferrer) Defer(_ context.Context, issue, expect, _, key string) error {
	d.keys = append(d.keys, key)
	d.expected[issue] = expect
	if current := d.status[issue]; current != "" && current != expect {
		d.conflicts++
		return fmt.Errorf("conflict: expected status %q, current is %q", expect, current)
	}
	d.status[issue] = planner.StatusDeferred
	d.deferred = append(d.deferred, issue)
	return nil
}

// liveClaims says which tickets a live build holds.
type liveClaims struct{ held map[string]bool }

func (l *liveClaims) Claimed(_ context.Context, issues []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, issue := range issues {
		if l.held[issue] {
			out[issue] = true
		}
	}
	return out, nil
}

func (w *world) newRetire() (*retireWorld, error) {
	h, err := w.newHead(w.tempDir)
	if err != nil {
		return nil, err
	}
	r := &retireWorld{
		head:    h,
		tickets: newWorldTickets(),
		defers:  newRecordingDeferrer(),
		live:    &liveClaims{held: map[string]bool{}},
	}
	r.retire = planner.Retirement{Tickets: r.tickets, Defer: r.defers, Live: r.live}
	w.retire = r
	return r, nil
}

// ticketOf returns the predecessor's row at an ordinal.
func (r *retireWorld) ticketOf(ordinal int) (planner.Ticket, error) {
	set, err := r.tickets.ForPlan(context.Background(), r.previous.Key)
	if err != nil {
		return planner.Ticket{}, err
	}
	for _, t := range set {
		if t.Ordinal == ordinal {
			return t, nil
		}
	}
	return planner.Ticket{}, fmt.Errorf("the predecessor has no ticket at ordinal %d", ordinal)
}

// dispositionAt names how a row was consumed.
func (r *retireWorld) dispositionAt(ordinal int) (string, error) {
	t, err := r.ticketOf(ordinal)
	if err != nil {
		return "", err
	}
	if !t.Consumed {
		return "", fmt.Errorf("ticket %d was never consumed", ordinal)
	}
	return t.Disposition, nil
}

// seedPredecessor writes the four rows scenario 9 names.
func (r *retireWorld) seedPredecessor() error {
	ctx := context.Background()
	r.previous = planner.Plan{
		Key: "plan-p1", TargetKey: "/spec", SpecHash: "hash-h1",
		Generation: 1, State: planner.PlanSuperseded, Completed: true,
	}
	if err := r.head.plans.Upsert(ctx, r.previous); err != nil {
		return err
	}
	rows := []planner.Ticket{
		// 0: a pending ticket, open in sutra.
		{Title: "pending work", IssueID: "issue-pending", Ordinal: 0},
		// 1: an issued creation step whose ticket exists but is unclaimed.
		{Title: "issued creation", IssueID: "issue-issued", Ordinal: 1},
		// 2: a ticket a live build claimed.
		{Title: "claimed work", IssueID: "issue-claimed", Ordinal: 2},
		// 3: a row whose ticket was never created at all.
		{Title: "never created", IssueID: "", Ordinal: 3},
		// 4: a ticket the successor selected to carry.
		{Title: "carried work", IssueID: "issue-carried", Ordinal: 4},
		// 5: a ticket that already shipped.
		{Title: "shipped work", IssueID: "issue-shipped", Ordinal: 5},
	}
	for _, t := range rows {
		t.Plan = r.previous.Key
		if err := r.tickets.Put(ctx, "/spec", t); err != nil {
			return err
		}
	}
	r.defers.status["issue-pending"] = planner.StatusOpen
	r.defers.status["issue-issued"] = planner.StatusOpen
	r.defers.status["issue-claimed"] = planner.StatusOpen
	r.defers.status["issue-carried"] = planner.StatusOpen
	r.defers.status["issue-shipped"] = planner.StatusComplete
	r.live.held["issue-claimed"] = true
	return nil
}

func registerRetire(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^predecessor rows with a pending ticket, an issued creation step, a ticket claimed by a live build, and a row whose ticket was never created$`,
		func() error {
			r, err := w.newRetire()
			if err != nil {
				return err
			}
			return r.seedPredecessor()
		})

	sc.When(`^retirement runs$`, func() error {
		r := w.retire
		// The successor selected ONE predecessor ticket to carry. Passing an
		// empty set would make "carried-forward is stamped only for tickets
		// the successor selected" vacuously true.
		r.err = r.retire.Run(context.Background(), r.previous, "project-1",
			map[string]bool{"issue-carried": true}, "actor-1")
		return r.err
	})

	sc.Then(`^the pending ticket is deferred in sutra via a conditional transition expecting its freshly observed status, with a generation-scoped defer key$`,
		func() error {
			r := w.retire
			if got := r.defers.expected["issue-pending"]; got != planner.StatusOpen {
				return fmt.Errorf("the deferral expected %q, not the observed status", got)
			}
			if r.defers.status["issue-pending"] != planner.StatusDeferred {
				return fmt.Errorf("the pending ticket was not deferred")
			}
			// GENERATION-SCOPED: derived from the predecessor's decomposition
			// key, which carries the intake generation. A key from the ticket
			// alone would be replayed by a later plan's retirement of the same
			// ticket, and sutra would return the earlier result instead.
			mine := planner.DeferKey(r.previous.Key, 0, 0)
			other := planner.DeferKey("plan-p0", 0, 0)
			if mine == other {
				return fmt.Errorf("two plans retiring one ticket share a defer key")
			}
			if len(r.defers.keys) == 0 || !containsKey(r.defers.keys, mine) {
				return fmt.Errorf("the deferral used keys %v, not the plan-scoped one", r.defers.keys)
			}
			return nil
		})

	sc.Then(`^a pending ticket observed in status "([^"]*)" defers the same way on the first attempt, never conflicting forever against an "([^"]*)" expectation$`,
		func(observed, naive string) error {
			r := w.retire
			// A SECOND retirement, of a ticket sutra reports as blocked. An
			// unconditional "open" expectation conflicts on every attempt,
			// against a state the issue will never return to.
			blocked := planner.Plan{
				Key: "plan-blocked", TargetKey: "/spec", SpecHash: "hash-h1",
				Generation: 1, State: planner.PlanSuperseded,
			}
			ctx := context.Background()
			if err := r.head.plans.Upsert(ctx, blocked); err != nil {
				return err
			}
			if err := r.tickets.Put(ctx, "/spec", planner.Ticket{
				Title: "blocked work", IssueID: "issue-blocked",
				Plan: blocked.Key, Ordinal: 0,
			}); err != nil {
				return err
			}
			r.defers.status["issue-blocked"] = observed
			before := r.defers.conflicts

			if err := r.retire.Run(ctx, blocked, "project-1", nil, "actor-1"); err != nil {
				return fmt.Errorf("retiring a %s ticket failed: %w", observed, err)
			}
			if r.defers.conflicts != before {
				return fmt.Errorf("the deferral conflicted; it expected %q against a %q ticket",
					naive, observed)
			}
			if got := r.defers.expected["issue-blocked"]; got != observed {
				return fmt.Errorf("the deferral expected %q, want the observed %q", got, observed)
			}
			return nil
		})

	sc.Then(`^a fresh read showing the ticket already deferred counts as fence established with no retry$`,
		func() error {
			r := w.retire
			already := planner.Plan{
				Key: "plan-already", TargetKey: "/spec", SpecHash: "hash-h1",
				Generation: 1, State: planner.PlanSuperseded,
			}
			ctx := context.Background()
			if err := r.head.plans.Upsert(ctx, already); err != nil {
				return err
			}
			if err := r.tickets.Put(ctx, "/spec", planner.Ticket{
				Title: "already deferred", IssueID: "issue-already",
				Plan: already.Key, Ordinal: 0,
			}); err != nil {
				return err
			}
			r.defers.status["issue-already"] = planner.StatusDeferred
			before := len(r.defers.keys)

			if err := r.retire.Run(ctx, already, "project-1", nil, "actor-1"); err != nil {
				return err
			}
			if len(r.defers.keys) != before {
				return fmt.Errorf("an already-deferred ticket was transitioned again")
			}
			set, err := r.tickets.ForPlan(ctx, already.Key)
			if err != nil {
				return err
			}
			if len(set) != 1 || set[0].Disposition != planner.DispositionRetired {
				return fmt.Errorf("the already-deferred row was consumed as %v", set)
			}
			return nil
		})

	sc.Then(`^the row whose ticket was never created retires with no sutra call$`, func() error {
		r := w.retire
		// Nothing exists in sutra to defer, and a call here would name an
		// issue that was never created.
		for _, issue := range r.defers.deferred {
			if issue == "" {
				return fmt.Errorf("a deferral was issued for a ticket that was never created")
			}
		}
		if _, asked := r.defers.expected[""]; asked {
			return fmt.Errorf("the status of a ticket that was never created was read")
		}
		got, err := r.dispositionAt(3)
		if err != nil {
			return err
		}
		if got != planner.DispositionRetired {
			return fmt.Errorf("the uncreated row is %q, want %q", got, planner.DispositionRetired)
		}
		return nil
	})

	sc.Then(`^an issued mutation step is replayed to its terminal result first — an unclaimed outcome is then deferred and stamped retired, never assumed carried forward$`,
		func() error {
			r := w.retire
			got, err := r.dispositionAt(1)
			if err != nil {
				return err
			}
			if got == planner.DispositionCarriedForward {
				return fmt.Errorf("an unclaimed issued step was assumed carried forward")
			}
			if got != planner.DispositionRetired {
				return fmt.Errorf("the issued step is %q, want %q", got, planner.DispositionRetired)
			}
			if r.defers.status["issue-issued"] != planner.StatusDeferred {
				return fmt.Errorf("the issued step's ticket was not deferred")
			}
			return nil
		})

	sc.Then(`^the ticket claimed by a live build stamps disposition bound — its run finishes under its pinned snapshot$`,
		func() error {
			r := w.retire
			got, err := r.dispositionAt(2)
			if err != nil {
				return err
			}
			if got != planner.DispositionBound {
				return fmt.Errorf("the claimed ticket is %q, want %q", got, planner.DispositionBound)
			}
			// And it must NOT have been deferred: that would cancel a run.
			if r.defers.status["issue-claimed"] == planner.StatusDeferred {
				return fmt.Errorf("a ticket a live build holds was deferred, cancelling its run")
			}
			return nil
		})

	sc.Then(`^carried-forward is stamped only for tickets the successor plan selected to carry$`,
		func() error {
			r := w.retire
			carried, err := r.dispositionAt(4)
			if err != nil {
				return err
			}
			if carried != planner.DispositionCarriedForward {
				return fmt.Errorf("the selected ticket is %q, want %q",
					carried, planner.DispositionCarriedForward)
			}
			// ONLY. Every other row must have some other disposition, or
			// "selected" means nothing.
			for ordinal := range 6 {
				if ordinal == 4 {
					continue
				}
				got, err := r.dispositionAt(ordinal)
				if err != nil {
					return err
				}
				if got == planner.DispositionCarriedForward {
					return fmt.Errorf("row %d was carried forward without being selected", ordinal)
				}
			}
			// The completed one is stamped as such, not retired: there is
			// nothing to defer and nothing to carry.
			shipped, err := r.dispositionAt(5)
			if err != nil {
				return err
			}
			if shipped != planner.DispositionCompleted {
				return fmt.Errorf("the shipped ticket is %q, want %q",
					shipped, planner.DispositionCompleted)
			}
			return nil
		})
}

// containsKey reports whether a key was presented.
func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// registerSupersede wires the ordering claims of scenario 8.
func registerSupersede(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^an activated head plan "([^"]*)" and an amended snapshot$`, func(p1 string) error {
		r, err := w.newRetire()
		if err != nil {
			return err
		}
		ctx := context.Background()
		r.previous = r.head.plan("hash-h1", 1)
		if err := r.head.plans.Upsert(ctx, r.previous); err != nil {
			return err
		}
		verdict, err := r.head.heads.Replace(ctx, r.previous)
		if err != nil || !verdict.Won {
			return fmt.Errorf("install %s: %v won=%v", p1, err, verdict.Won)
		}
		// ACTIVATED: whole, and its fence lowered.
		r.previous.State, r.previous.Completed = planner.PlanActive, true
		if err := r.head.plans.Upsert(ctx, r.previous); err != nil {
			return err
		}
		if err := r.head.heads.Activate(ctx, r.previous.Key); err != nil {
			return err
		}
		// One open ticket, so retirement has real work to do.
		return r.tickets.Put(ctx, "/spec", planner.Ticket{
			Title: "dropped work", IssueID: "issue-dropped",
			Plan: r.previous.Key, Ordinal: 0,
		})
	})

	sc.When(`^decomposition runs with a new key producing candidate "([^"]*)"$`, func(p2 string) error {
		r := w.retire
		ctx := context.Background()
		candidate := r.head.plan("hash-h2", 2)
		if err := r.head.plans.Upsert(ctx, candidate); err != nil {
			return err
		}
		verdict, err := r.head.heads.Replace(ctx, candidate)
		if err != nil {
			return err
		}
		r.head.verdict = verdict
		return nil
	})

	sc.Then(`^one atomic transaction points "([^"]*)" predecessor at "([^"]*)", marks "([^"]*)" superseded, increments the pop fence, and moves PlanHead with a generation bump$`,
		func(p2, p1, alsoP1 string) error {
			r := w.retire
			ctx := context.Background()
			if !r.head.verdict.Won {
				return fmt.Errorf("the candidate did not take the head")
			}
			successor, err := r.head.landed(r.head.plan("hash-h2", 2).Key)
			if err != nil {
				return err
			}
			if successor.Predecessor != r.previous.Key {
				return fmt.Errorf("%s's predecessor is %q, not %s", p2, successor.Predecessor, p1)
			}
			previous, err := r.head.landed(r.previous.Key)
			if err != nil {
				return err
			}
			if previous.State != planner.PlanSuperseded {
				return fmt.Errorf("%s is %q, not superseded", p1, previous.State)
			}
			if previous.SupersededBy != successor.Key {
				return fmt.Errorf("%s does not name its replacement", p1)
			}
			head, found, err := r.head.heads.Head(ctx, "/spec")
			if err != nil || !found {
				return fmt.Errorf("read head: %v found=%v", err, found)
			}
			// The predecessor was ACTIVATED, so its fence contribution was
			// already zero — the increment is a real increment here.
			if head.Fence != 1 {
				return fmt.Errorf("the fence is %d after the head moved", head.Fence)
			}
			if head.Generation != 2 {
				return fmt.Errorf("the head generation is %d after one replacement", head.Generation)
			}
			if head.Current != successor.Key {
				return fmt.Errorf("the head did not move to %s", p2)
			}
			return nil
		})

	sc.Then(`^every "([^"]*)" row is stamped consumed with its state-appropriate disposition — "([^"]*)", "([^"]*)", "([^"]*)", or "([^"]*)" — before "([^"]*)" activates$`,
		func(p1, a, b, c, d, p2 string) error {
			r := w.retire
			ctx := context.Background()
			// The fence is still UP: retirement runs before activation.
			head, _, err := r.head.heads.Head(ctx, "/spec")
			if err != nil {
				return err
			}
			if head.Fence != 1 {
				return fmt.Errorf("the fence came down at %d before retirement ran", head.Fence)
			}
			if err := r.retire.Run(ctx, r.previous, "project-1", nil, "actor-1"); err != nil {
				return err
			}
			set, err := r.tickets.ForPlan(ctx, r.previous.Key)
			if err != nil {
				return err
			}
			if len(set) == 0 {
				return fmt.Errorf("the predecessor had no rows, so retirement proved nothing")
			}
			allowed := map[string]bool{a: true, b: true, c: true, d: true}
			for _, t := range set {
				if !t.Consumed {
					return fmt.Errorf("row %d was never stamped consumed", t.Ordinal)
				}
				if !allowed[t.Disposition] {
					return fmt.Errorf("row %d is %q, not one of the four dispositions",
						t.Ordinal, t.Disposition)
				}
			}
			return nil
		})

	sc.Then(`^after retirement completes, a separate atomic activation transaction decrements the pop fence — never the head-moving transaction, which would admit pops before retirement ran$`,
		func() error {
			r := w.retire
			ctx := context.Background()
			successor := r.head.plan("hash-h2", 2)
			// The head-moving transaction RAISED the fence and left it up.
			// Only activation lowers it, and only for a plan that is whole.
			successor.State, successor.Completed = planner.PlanActive, true
			if err := r.head.plans.Upsert(ctx, successor); err != nil {
				return err
			}
			if err := r.head.heads.Activate(ctx, successor.Key); err != nil {
				return err
			}
			head, _, err := r.head.heads.Head(ctx, "/spec")
			if err != nil {
				return err
			}
			if head.Fence != 0 {
				return fmt.Errorf("the fence is %d after activation", head.Fence)
			}
			return nil
		})
}
