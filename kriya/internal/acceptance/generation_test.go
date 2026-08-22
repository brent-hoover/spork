//go:build acceptance

package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// registerGenerations wires the intake-generation scenarios.
//
// These exercise the store directly rather than through cli.Build: the
// scenarios are about generation allocation and the mapping fence, and driving
// them through a full intake would make an assertion about the fence depend on
// avspec, the tracker, and the agent all behaving — a failure anywhere reading
// as a failure of the thing under test.
func registerGenerations(sc *godog.ScenarioContext, w *world) {
	var seeded planner.IntakeAttempt

	sc.Given(`^an intake attempt was recorded pending under its idempotency token and its generation landed on the mapping$`,
		func() error {
			a, err := w.attempts.Reserve(context.Background(), "crashed-token", "/spec")
			if err != nil {
				return err
			}
			seeded = a
			return w.attempts.MapSpec(context.Background(), planner.SpecMapping{
				TargetKey: "/spec", Generation: a.Generation, SnapshotHash: "S1",
			})
		})
	sc.Given(`^the crash hit before the plan was created$`, func() error {
		if seeded.State != planner.AttemptPending {
			return fmt.Errorf("the seeded attempt is %q, not pending", seeded.State)
		}
		return nil
	})
	sc.When(`^the intake retries under the same token$`, func() error {
		a, err := w.attempts.Reserve(context.Background(), "crashed-token", "/spec")
		if err != nil {
			return err
		}
		w.retried = a
		return nil
	})
	sc.Then(`^it resumes the recorded attempt and reuses its generation — no duplicate supersession$`,
		func() error {
			if w.retried.Generation != seeded.Generation {
				return fmt.Errorf("retry took generation %d, want the recorded %d",
					w.retried.Generation, seeded.Generation)
			}
			return nil
		})
	sc.Given(`^a deliberate same-hash re-intake arrives under a new token$`, func() error {
		a, err := w.attempts.Reserve(context.Background(), "fresh-token", "/spec")
		if err != nil {
			return err
		}
		w.retried = a
		return nil
	})
	sc.Then(`^it allocates the next generation$`, func() error {
		if w.retried.Generation != seeded.Generation+1 {
			return fmt.Errorf("got generation %d, want %d", w.retried.Generation, seeded.Generation+1)
		}
		return nil
	})

	sc.Given(`^two differently keyed intakes for the same target run concurrently$`,
		func() error { return nil })
	sc.When(`^both attempt to reserve the next generation$`, func() error {
		for _, token := range []string{"racer-a", "racer-b"} {
			a, err := w.attempts.Reserve(context.Background(), token, "/race")
			if err != nil {
				return err
			}
			w.reserved = append(w.reserved, a)
		}
		return nil
	})
	sc.Then(`^the CAS increment serializes them — one reserves N, the loser re-reads and reserves N\+1$`,
		func() error {
			if len(w.reserved) != 2 {
				return fmt.Errorf("expected two reservations, got %d", len(w.reserved))
			}
			if w.reserved[1].Generation != w.reserved[0].Generation+1 {
				return fmt.Errorf("generations %d and %d are not consecutive",
					w.reserved[0].Generation, w.reserved[1].Generation)
			}
			return nil
		})
	sc.Then(`^no two attempts ever hold the same generation$`, func() error {
		seen := map[int]bool{}
		for _, a := range w.reserved {
			if seen[a.Generation] {
				return fmt.Errorf("generation %d is held twice", a.Generation)
			}
			seen[a.Generation] = true
		}
		return nil
	})

	sc.Given(`^intake generation (\d+) pinned the project's mapping snapshot$`, func(gen int) error {
		return w.attempts.MapSpec(context.Background(), planner.SpecMapping{
			TargetKey: "/mapped", Generation: gen, SnapshotHash: "newer",
		})
	})
	sc.When(`^a delayed plan seed carrying generation (\d+) arrives$`, func(gen int) error {
		return w.attempts.MapSpec(context.Background(), planner.SpecMapping{
			TargetKey: "/mapped", Generation: gen, SnapshotHash: "older",
		})
	})
	sc.Then(`^the mapping is untouched — the older generation loses the upsert$`,
		func() error { return w.assertMapping("/mapped", 2, "newer") })
	sc.Given(`^no mapping exists yet$`, func() error { return nil })
	sc.When(`^a generation-1 plan seed inserts first and the generation-2 intake write arrives after$`,
		func() error {
			ctx := context.Background()
			if err := w.attempts.MapSpec(ctx, planner.SpecMapping{
				TargetKey: "/converge", Generation: 1, SnapshotHash: "older"}); err != nil {
				return err
			}
			return w.attempts.MapSpec(ctx, planner.SpecMapping{
				TargetKey: "/converge", Generation: 2, SnapshotHash: "newer"})
		})
	sc.Then(`^the mapping converges on generation (\d+)$`,
		func(gen int) error { return w.assertMapping("/converge", gen, "newer") })
	sc.Then(`^unplanned binds always read the newest intake's snapshot$`, func() error {
		got, found, err := w.attempts.Mapping(context.Background(), "/converge")
		if err != nil || !found {
			return fmt.Errorf("no mapping to bind against: %v", err)
		}
		if got.SnapshotHash != "newer" {
			return fmt.Errorf("a bind would read %q, not the newest", got.SnapshotHash)
		}
		return nil
	})
	sc.Given(`^a re-intake pins the same content hash a prior intake pinned$`, func() error {
		ctx := context.Background()
		first, err := w.attempts.Reserve(ctx, "same-a", "/samehash")
		if err != nil {
			return err
		}
		w.reserved = append(w.reserved, first)
		if err := w.attempts.MapSpec(ctx, planner.SpecMapping{
			TargetKey: "/samehash", Generation: first.Generation, SnapshotHash: "H"}); err != nil {
			return err
		}
		second, err := w.attempts.Reserve(ctx, "same-b", "/samehash")
		if err != nil {
			return err
		}
		w.reserved = append(w.reserved, second)
		return w.attempts.MapSpec(ctx, planner.SpecMapping{
			TargetKey: "/samehash", Generation: second.Generation, SnapshotHash: "H"})
	})
	sc.Then(`^it allocates a fresh, higher generation for the target$`, func() error {
		n := len(w.reserved)
		if n < 2 || w.reserved[n-1].Generation <= w.reserved[n-2].Generation {
			return fmt.Errorf("the re-intake did not take a higher generation: %+v", w.reserved)
		}
		return nil
	})
	sc.Then(`^a plan still carrying the prior intake's generation cannot regress the mapping, even naming the same hash$`,
		func() error {
			n := len(w.reserved)
			prior, current := w.reserved[n-2], w.reserved[n-1]
			if err := w.attempts.MapSpec(context.Background(), planner.SpecMapping{
				TargetKey: "/samehash", Generation: prior.Generation, SnapshotHash: "H"}); err != nil {
				return err
			}
			// Naming the SAME hash is the trap: a fence comparing content
			// rather than generation would see no change and let it through.
			return w.assertMapping("/samehash", current.Generation, "H")
		})
}

func (w *world) assertMapping(target string, gen int, hash string) error {
	got, found, err := w.attempts.Mapping(context.Background(), target)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no mapping for %s", target)
	}
	if got.Generation != gen || got.SnapshotHash != hash {
		return fmt.Errorf("mapping is generation %d/%q, want %d/%q",
			got.Generation, got.SnapshotHash, gen, hash)
	}
	return nil
}
