//go:build acceptance

package acceptance

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// headWorld is one supersession scenario's state.
//
// It runs against a REAL SQLite database rather than a fake head store. The
// CAS is the whole claim — one transaction deciding eligibility, supersession,
// the head move, the bump and the fence — and a fake would be a second
// implementation of exactly the thing under test, passing because both copies
// agree rather than because either is right.
type headWorld struct {
	db      *sql.DB
	heads   planner.SQLHeads
	plans   planner.SQLPlans
	verdict planner.Replacement
	// h1 is the key of the original snapshot's plan, kept so the revert
	// scenario can prove the re-intake derives a DIFFERENT one.
	h1 string
}

func (w *world) newHead(tempDir func() (string, error)) (*headWorld, error) {
	dir, err := tempDir()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "head.db"))
	if err != nil {
		return nil, err
	}
	for _, schema := range []string{planner.PlanMigration, planner.PlanKeyMigration, planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				return nil, fmt.Errorf("migrate %q: %w", stmt, err)
			}
		}
	}
	h := &headWorld{db: db, heads: planner.SQLHeads{DB: db}, plans: planner.SQLPlans{DB: db}}
	w.head = h
	return h, nil
}

// migrateSteps adds the step table to a head world's database.
func migrateSteps(h *headWorld) error {
	for _, stmt := range strings.Split(planner.StepMigration, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := h.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	return nil
}

// plan builds a candidate for a snapshot pinned under a generation.
func (h *headWorld) plan(specHash string, generation int) planner.Plan {
	return planner.Plan{
		Key:        planner.DecompositionKey("SHORT", "/spec", specHash, generation),
		TargetKey:  "/spec",
		SpecHash:   specHash,
		Generation: generation,
		State:      planner.PlanPending,
	}
}

// install decomposes a plan and makes it the active head.
func (h *headWorld) install(p planner.Plan) error {
	ctx := context.Background()
	if err := h.plans.Upsert(ctx, p); err != nil {
		return err
	}
	verdict, err := h.heads.Replace(ctx, p)
	if err != nil {
		return err
	}
	if !verdict.Won {
		return fmt.Errorf("plan for generation %d did not take the head", p.Generation)
	}
	// Retirement is empty for a bootstrap, so activation follows immediately.
	p.State = planner.PlanActive
	if err := h.plans.Upsert(ctx, p); err != nil {
		return err
	}
	return h.heads.Activate(ctx, p.Key)
}

// enter runs a candidate through the real CAS.
func (h *headWorld) enter(p planner.Plan) error {
	ctx := context.Background()
	if err := h.plans.Upsert(ctx, p); err != nil {
		return err
	}
	verdict, err := h.heads.Replace(ctx, p)
	if err != nil {
		return err
	}
	h.verdict = verdict
	return nil
}

// landed reads a plan's durable state back.
func (h *headWorld) landed(key string) (planner.Plan, error) {
	p, found, err := h.plans.ByKey(context.Background(), key)
	if err != nil {
		return planner.Plan{}, err
	}
	if !found {
		return planner.Plan{}, fmt.Errorf("no plan row for %s", key[:12])
	}
	return p, nil
}

func registerHead(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a generation-(\d+) intake crashed before decomposition while a generation-(\d+) intake installed its plan as head$`,
		func(stale, winner int) error {
			h, err := w.newHead(w.tempDir)
			if err != nil {
				return err
			}
			// The winner is installed. The stale intake's candidate exists
			// only as an in-flight request; it never reached the store.
			return h.install(h.plan("hash-h1", winner))
		})

	sc.When(`^the generation-(\d+) intake recovers and its candidate enters the replacement CAS$`,
		func(stale int) error {
			return w.head.enter(w.head.plan("hash-h0", stale))
		})

	sc.Then(`^the CAS rejects it — the candidate's intake generation is not strictly newer than the head's$`,
		func() error {
			h := w.head
			if h.verdict.Won {
				return fmt.Errorf("a stale intake took the head")
			}
			// The head must be UNMOVED. A CAS that rejected the candidate but
			// bumped the generation anyway would have mutated the thing it
			// refused to change.
			head, found, err := h.heads.Head(context.Background(), "/spec")
			if err != nil || !found {
				return fmt.Errorf("read head: %v found=%v", err, found)
			}
			if head.Generation != 1 {
				return fmt.Errorf("the head generation moved to %d on a rejected CAS", head.Generation)
			}
			winner, err := h.landed(h.plan("hash-h1", 2).Key)
			if err != nil {
				return err
			}
			if winner.State != planner.PlanActive {
				return fmt.Errorf("the winning head plan is now %q", winner.State)
			}
			return nil
		})

	sc.Then(`^being already ineligible, the candidate transitions directly to terminal historical instead of parking awaiting-operator$`,
		func() error {
			h := w.head
			got, err := h.landed(h.plan("hash-h0", 1).Key)
			if err != nil {
				return err
			}
			if got.State == planner.PlanAwaitingOperator {
				return fmt.Errorf(
					"the candidate parked for the operator, offering a retry the CAS can only reject again")
			}
			if got.State != planner.PlanHistorical {
				return fmt.Errorf("the candidate landed in %q, want %q", got.State, planner.PlanHistorical)
			}
			return nil
		})

	registerRevert(sc, w)
}

func registerRevert(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^the target moved from snapshot "([^"]*)" to "([^"]*)" and the operator re-intakes "([^"]*)" under a new intake generation$`,
		func(h1, h2, back string) error {
			h, err := w.newHead(w.tempDir)
			if err != nil {
				return err
			}
			// Generation 1 planned H1; generation 2 superseded it with H2.
			first := h.plan(h1, 1)
			h.h1 = first.Key
			if err := h.install(first); err != nil {
				return err
			}
			second := h.plan(h2, 2)
			if err := h.enter(second); err != nil {
				return err
			}
			if !h.verdict.Won {
				return fmt.Errorf("the H2 plan did not take the head")
			}
			second.State = planner.PlanActive
			if err := h.plans.Upsert(context.Background(), second); err != nil {
				return err
			}
			return h.heads.Activate(context.Background(), second.Key)
		})

	sc.When(`^decomposition runs for the reverted spec$`, func() error {
		// The SAME snapshot as generation 1, under generation 3.
		return w.head.enter(w.head.plan("H1", 3))
	})

	sc.Then(`^the decomposition key differs from the old "([^"]*)" plan's key — the generation is part of the identity$`,
		func(string) error {
			h := w.head
			reverted := h.plan("H1", 3).Key
			if reverted == h.h1 {
				return fmt.Errorf("the revert derived the old plan's key, so it would replay a superseded plan")
			}
			// And it must be a plan of its own, not a lookup miss.
			if _, err := h.landed(reverted); err != nil {
				return err
			}
			return nil
		})

	sc.Then(`^a fresh plan is created that supersedes the "([^"]*)" head through the ordinary replacement path$`,
		func(h2 string) error {
			h := w.head
			if !h.verdict.Won {
				return fmt.Errorf("the reverted plan did not take the head")
			}
			// The H2 plan is superseded and NAMES its replacement — that is
			// how a same-key retry on it learns where the head went.
			old, err := h.landed(h.plan(h2, 2).Key)
			if err != nil {
				return err
			}
			if old.State != planner.PlanSuperseded {
				return fmt.Errorf("the H2 plan is %q, not superseded", old.State)
			}
			if old.SupersededBy != h.plan("H1", 3).Key {
				return fmt.Errorf("the H2 plan does not name the reverted plan as its replacement")
			}
			// The ORDINARY path: the fence rose with the head move, and the
			// head generation bumped. A revert taking a shortcut past either
			// would admit pops before its predecessor retired.
			head, found, err := h.heads.Head(context.Background(), "/spec")
			if err != nil || !found {
				return fmt.Errorf("read head: %v found=%v", err, found)
			}
			if head.Fence < 1 {
				return fmt.Errorf("the fence is %d after a head move, so pops are already admitted", head.Fence)
			}
			if head.Generation != 3 {
				return fmt.Errorf("the head generation is %d after three installs", head.Generation)
			}
			return nil
		})

	sc.Then(`^a same-generation retry of that request still resolves its own key idempotently$`, func() error {
		h := w.head
		// The same request again: same spec, same generation, same key. It
		// must resolve to its OWN plan rather than entering the CAS, where it
		// would compete with the plan it is retrying.
		res, err := planner.Resolve(context.Background(), h.plans, h.plan("H1", 3).Key)
		if err != nil {
			return err
		}
		if res.Fresh {
			return fmt.Errorf("the retry resolved as a fresh key and would decompose again")
		}
		if res.Plan.State != planner.PlanActive && res.Plan.State != planner.PlanPending {
			return fmt.Errorf("the retry resolved to a %s plan", res.Plan.State)
		}
		head, _, err := h.heads.Head(context.Background(), "/spec")
		if err != nil {
			return err
		}
		if head.Generation != 3 {
			return fmt.Errorf("resolving a retry moved the head generation to %d", head.Generation)
		}
		return nil
	})
}
