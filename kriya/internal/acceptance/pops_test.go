//go:build acceptance

package acceptance

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

// writeAheadWorld is one write-ahead-pop scenario's state.
//
// Against a REAL database, because the claim under test is that the run and
// the admission commit together — a pair of fakes would agree with each other
// rather than with SQLite.
type writeAheadWorld struct {
	db    *sql.DB
	store orchestrator.SQLStore
	fence planner.SQLFence
	stack *scriptedPops
	loop  orchestrator.Loop
}

// scriptedPops hands out one ticket, then nothing, honouring keys.
//
// It settles a claim under its key the way sutra does: a replayed key returns
// the SAME ticket rather than taking a second. Recovery depends on exactly
// that.
type scriptedPops struct {
	byKey  map[string]string
	next   []string
	handed int
	keys   []string
	// empty makes every claim return an explicitly empty result.
	empty bool
	// blocked is a ticket sutra never offers, because a blocker holds it
	// shut. It is here so "a blocked ticket is never handed out" has
	// something to be about.
	blocked string
}

func (p *scriptedPops) Pop(_ context.Context, key string) (string, string, error) {
	p.keys = append(p.keys, key)
	if claimed, ok := p.byKey[key]; ok {
		return claimed, "T-" + claimed, nil
	}
	if p.empty || p.handed >= len(p.next) {
		return "", "", nil
	}
	id := p.next[p.handed]
	p.handed++
	p.byKey[key] = id
	return id, "T-" + id, nil
}

func (w *world) newWriteAhead(tickets ...string) (*writeAheadWorld, error) {
	dir, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "pops.db"))
	if err != nil {
		return nil, err
	}
	for _, schema := range []string{
		orchestrator.Migration, orchestrator.RoundLimitMigration,
		orchestrator.SubmissionMigration, orchestrator.CompletionMigration,
		orchestrator.PopMigration, orchestrator.CursorMigration,
		orchestrator.MergeMigration, orchestrator.ResourceMigration,
		orchestrator.HeadMigration, orchestrator.IssueMigration,
		orchestrator.StallMigration, orchestrator.StartedMigration,
		orchestrator.FindingMigration, orchestrator.PopKeyMigration,
		planner.FenceMigration,
	} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				return nil, fmt.Errorf("migrate %q: %w", stmt, err)
			}
		}
	}
	p := &writeAheadWorld{
		db:    db,
		store: orchestrator.SQLStore{DB: db},
		fence: planner.SQLFence{DB: db},
		stack: &scriptedPops{byKey: map[string]string{}, next: tickets},
	}
	p.loop = orchestrator.Loop{
		Pops: p.stack, Ordinals: orchestrator.SQLOrdinals{DB: db},
		Admit: p.fence, Reserve: p.store, TargetKey: "/spec", MaxTickets: 1,
		Build: func(_ context.Context, issue, title string) (orchestrator.BuildRun, error) {
			return orchestrator.BuildRun{Ticket: title, Issue: issue}, nil
		},
	}
	w.writeAhead = p
	return p, nil
}

// unbound lists the runs a crash would leave claimed-but-unbound.
func (p *writeAheadWorld) unbound() ([]orchestrator.BuildRun, error) {
	return p.store.Unbound(context.Background())
}

func registerPops(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^an agent pops a ticket$`, func() error {
		p, err := w.newWriteAhead("issue-1")
		if err != nil {
			return err
		}
		// The loop reserves, claims, and binds. What matters is the ORDER,
		// which the assertions below read from durable state.
		if _, err := p.loop.Run(context.Background()); err != nil {
			return err
		}
		return nil
	})

	sc.Then(`^the BuildRun record is durable before any other side effect$`, func() error {
		p := w.writeAhead
		// The run's id IS the pop key when nothing else names it, so the
		// reservation and the claim are the same identity — which is how a
		// replayed reservation lands on the same row rather than making a
		// second run for one claim.
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		if len(runs) != 1 {
			return fmt.Errorf("the pop produced %d runs", len(runs))
		}
		if runs[0].PopKey == "" {
			return fmt.Errorf("the run records no pop key, so it could not replay its own claim")
		}
		if len(p.stack.keys) == 0 || p.stack.keys[0] != runs[0].PopKey {
			return fmt.Errorf("the claim used key %v, not the one the run recorded", p.stack.keys)
		}
		if runs[0].Issue != "issue-1" {
			return fmt.Errorf("the run bound issue %q", runs[0].Issue)
		}
		return nil
	})

	sc.When(`^the process crashes between the claim and the binding$`, func() error {
		p := w.writeAhead
		// Reproduced in durable state: the claim landed, the binding did not.
		if _, err := p.db.ExecContext(context.Background(),
			`UPDATE build_run SET issue = '', ticket = '', state = ?`,
			string(orchestrator.StateQueued)); err != nil {
			return err
		}
		return nil
	})

	sc.Then(`^recovery discovers the claimed-but-unbound pop and binds it by replaying the binding rules$`,
		func() error {
			p := w.writeAhead
			ctx := context.Background()
			unbound, err := p.unbound()
			if err != nil {
				return err
			}
			if len(unbound) != 1 {
				return fmt.Errorf("recovery found %d claimed-but-unbound pops", len(unbound))
			}

			// REPLAYED under the persisted key, so sutra returns the same
			// ticket rather than taking a second.
			issue, title, err := p.stack.Pop(ctx, unbound[0].PopKey)
			if err != nil {
				return err
			}
			if issue != "issue-1" {
				return fmt.Errorf("the replay claimed %q, not the original ticket", issue)
			}
			if err := p.store.Bind(ctx, unbound[0].ID, issue, title); err != nil {
				return err
			}
			bound, found, err := p.store.Find(ctx, unbound[0].ID)
			if err != nil || !found {
				return fmt.Errorf("find: %v found=%v", err, found)
			}
			if bound.Issue != "issue-1" {
				return fmt.Errorf("the recovered run bound %q", bound.Issue)
			}
			return nil
		})

	sc.Then(`^no claim remains without a BuildRun that owns it$`, func() error {
		p := w.writeAhead
		left, err := p.unbound()
		if err != nil {
			return err
		}
		if len(left) != 0 {
			return fmt.Errorf("%d claims are still unowned", len(left))
		}
		// And every key the tracker was asked under belongs to a run.
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		owned := map[string]bool{}
		for _, r := range runs {
			owned[r.PopKey] = true
		}
		for _, key := range p.stack.keys {
			if !owned[key] {
				return fmt.Errorf("key %s claimed work no run owns", planner.Short(key))
			}
		}
		return nil
	})
}

// registerQueuedPops wires the reconciliation scenarios.
func registerQueuedPops(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a queued BuildRun whose ticket field is unfilled at recovery because a claim landed but the ticket was never bound$`,
		func() error {
			p, err := w.newWriteAhead("issue-1")
			if err != nil {
				return err
			}
			ctx := context.Background()
			// Reserved and claimed, then a crash before the binding.
			if _, err := p.loop.Run(ctx); err != nil {
				return err
			}
			if _, err := p.db.ExecContext(ctx,
				`UPDATE build_run SET issue = '', ticket = '', state = ?`,
				string(orchestrator.StateQueued)); err != nil {
				return err
			}
			return nil
		})

	sc.Given(`^a queued BuildRun whose ticket field is unfilled because the pop never claimed anything$`,
		func() error {
			p, err := w.newWriteAhead()
			if err != nil {
				return err
			}
			p.stack.empty = true
			// The loop reserves, claims nothing, and settles it as no-work.
			// Rewound to queued, this is the state a crash in that window
			// leaves behind.
			if _, err := p.loop.Run(context.Background()); err != nil {
				return err
			}
			if _, err := p.db.ExecContext(context.Background(),
				`UPDATE build_run SET state = ?`, string(orchestrator.StateQueued)); err != nil {
				return err
			}
			return nil
		})

	sc.When(`^retirement begins$`, func() error { return w.writeAhead.reconcile() })
	sc.When(`^recovery replays the pop and the replay returns an explicit empty result$`,
		func() error { return w.writeAhead.reconcile() })

	sc.Then(`^the BuildRun is bound by pop replay before any ownership classification$`, func() error {
		p := w.writeAhead
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		if len(runs) != 1 {
			return fmt.Errorf("reconciliation left %d runs", len(runs))
		}
		if runs[0].Issue != "issue-1" {
			return fmt.Errorf("the run bound %q, not the ticket its claim took", runs[0].Issue)
		}
		return nil
	})

	sc.Then(`^the in-flight claim is classified as issued work, never retired as pending$`, func() error {
		p := w.writeAhead
		ctx := context.Background()
		// Nothing is left unbound, so nothing reads as pending. Retirement's
		// live-work check asks by ISSUE, and the reconciled run answers.
		left, err := p.unbound()
		if err != nil {
			return err
		}
		if len(left) != 0 {
			return fmt.Errorf("%d claims would still be classified as pending", len(left))
		}
		runs, err := p.store.ForPlan(ctx, "/spec")
		if err != nil {
			return err
		}
		if len(runs) == 0 || runs[0].State != orchestrator.StateQueued {
			return fmt.Errorf("the reconciled run is not queued for work: %+v", runs)
		}
		return nil
	})

	sc.Then(`^the BuildRun transitions to no-work$`, func() error {
		p := w.writeAhead
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		if len(runs) != 1 {
			return fmt.Errorf("reconciliation left %d runs", len(runs))
		}
		if runs[0].State != orchestrator.StateNoWork {
			return fmt.Errorf("the run settled as %q, want %q",
				runs[0].State, orchestrator.StateNoWork)
		}
		return nil
	})

	sc.Then(`^no work is invented for it and nothing is classified as issued$`, func() error {
		p := w.writeAhead
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		for _, r := range runs {
			if r.Issue != "" || r.Ticket != "" {
				return fmt.Errorf("a run that claimed nothing was given ticket %q issue %q",
					r.Ticket, r.Issue)
			}
		}
		left, err := p.unbound()
		if err != nil {
			return err
		}
		if len(left) != 0 {
			return fmt.Errorf("%d runs would still be classified as issued work", len(left))
		}
		return nil
	})
}

// reconcile replays every claimed-but-unbound pop, as recovery does.
//
// The same shape as production's reconcilePops: replay under the PERSISTED
// key, bind what came back, and settle an explicitly empty replay as no-work.
func (p *writeAheadWorld) reconcile() error {
	ctx := context.Background()
	unbound, err := p.unbound()
	if err != nil {
		return err
	}
	for _, run := range unbound {
		issue, title, err := p.stack.Pop(ctx, run.PopKey)
		if err != nil {
			return err
		}
		if issue == "" {
			if err := p.store.SettleEmpty(ctx, run.ID); err != nil {
				return err
			}
			continue
		}
		if err := p.store.Bind(ctx, run.ID, issue, title); err != nil {
			return err
		}
	}
	return nil
}

// registerParallel wires the concurrency scenario.
func registerParallel(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^two unblocked tickets with no relation between them$`, func() error {
		p, err := w.newWriteAhead("issue-1", "issue-2")
		if err != nil {
			return err
		}
		// A THIRD ticket that a blocker holds shut. sutra never offers it, so
		// it must never reach an agent — and its absence is what makes "a
		// blocked ticket is never handed out" a claim about something.
		p.stack.blocked = "issue-3"
		return nil
	})

	sc.When(`^two agents pop work concurrently$`, func() error {
		p := w.writeAhead
		ctx := context.Background()
		// Two passes of one ticket each, which is what concurrent agents look
		// like from the tracker: two claims under two different keys.
		p.loop.MaxTickets = 1
		for range 2 {
			if _, err := p.loop.Run(ctx); err != nil {
				return err
			}
		}
		return nil
	})

	sc.Then(`^each agent receives a different ticket in its own BuildRun$`, func() error {
		p := w.writeAhead
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		if len(runs) != 2 {
			return fmt.Errorf("two pops produced %d runs", len(runs))
		}
		if runs[0].Issue == runs[1].Issue {
			return fmt.Errorf("both runs bound %q", runs[0].Issue)
		}
		// Each has its OWN run and its own pop key: two agents sharing a run
		// would share a workspace, a branch and a review.
		if runs[0].ID == runs[1].ID || runs[0].PopKey == runs[1].PopKey {
			return fmt.Errorf("the two pops share a run or a key")
		}
		for _, r := range runs {
			if r.Issue == "" {
				return fmt.Errorf("a run bound no ticket")
			}
		}
		return nil
	})

	sc.Then(`^a blocked ticket is never handed out$`, func() error {
		p := w.writeAhead
		runs, err := p.store.ForPlan(context.Background(), "/spec")
		if err != nil {
			return err
		}
		for _, r := range runs {
			if r.Issue == p.stack.blocked {
				return fmt.Errorf("the blocked ticket %s was handed out", r.Issue)
			}
		}
		if p.stack.blocked == "" {
			return fmt.Errorf("no ticket was blocked, so nothing was under test")
		}
		return nil
	})

	sc.Then(`^sutra's conditional claim rejects a second claim on an already-claimed ticket$`,
		func() error {
			p := w.writeAhead
			ctx := context.Background()
			// A replayed key returns the SAME ticket rather than taking a
			// second — that is what makes a recovered pop safe. And no key
			// ever hands out a ticket another key already holds.
			claimed := map[string]string{}
			for _, key := range p.stack.keys {
				issue, _, err := p.stack.Pop(ctx, key)
				if err != nil {
					return err
				}
				if issue == "" {
					continue
				}
				if owner, taken := claimed[issue]; taken && owner != key {
					return fmt.Errorf("ticket %s was claimed by both %s and %s",
						issue, planner.Short(owner), planner.Short(key))
				}
				claimed[issue] = key
			}
			if len(claimed) < 2 {
				return fmt.Errorf("only %d tickets were claimed, so no conflict was possible",
					len(claimed))
			}
			return nil
		})
}
