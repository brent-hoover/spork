---
title: Kriya Build — Scope
type: scope
status: draft
owner: Brent Hoover
created: 2026-08-21
design: ./design.md
---

# Kriya Build — Scope

**Objective (one sentence):** Implement kriya — the build engine specified
in `kriya/avspec.yaml` — in Go, until all 155 scenarios in
`kriya/verification/` pass under godog and the pinned gate chain is green.

## In scope

* All twelve avspec modules as Go packages under `kriya/internal/`, plus
  the composition root `kriya/cmd/kriya`, the `store` package, and the
  `acceptance` godog harness.
* The orchestrator state table covering all sixteen `BuildRun` states.
* The four seams: `claude -p` subprocess agents, the roborev CLI bridge,
  the sutra HTTP client, and the target-command gate runner.
* SQLite schema and migrations for all 25 entities, with write-ahead
  idempotency keys and fence columns.
* The bubbletea TUI: live state and the operator inbox.
* Both test rings — unit against a fake tracker, integration against a
  real sutra instance.
* Completing `kriya/arch-go.yml` with rules for `cmd/kriya`, `store`, and
  `acceptance`, using the explicit-complement idiom where `may_import` is
  empty.
* Stack-command changes to `kriya/avspec.yaml` where a gate cannot run as
  written — specifically the mutation scope — each with its own review.
* One end-to-end driven build of `avspec/examples/linkshort`.

## Out of scope / non-goals

* Behavioral changes to `kriya/avspec.yaml` — requirements, acceptance
  criteria, entities, and module boundaries are fixed. A defect found
  during implementation is a spec fix with its own review, not silent
  drift.
* Changes to `kriya/verification/*.feature`. They are the acceptance
  tests; making a scenario pass by editing the scenario is out.
* Any tracker other than sutra.
* Modifying roborev, including the correlation key and SHA dedupe R1 found
  missing.
* Kriya building kriya (bootstrapping).
* Multi-machine, hosted, or multi-operator operation.
* avspec slice 2 (constitution `check:` attachments, cross-manifest links,
  `avspec render`).
* Sutra's coverage residue — `web`, `cli`, `cmd/sutra` arms and the three
  shadowed import rules.

## Files/areas you MAY modify (allowlist)

* `kriya/cmd/**`
* `kriya/internal/**`
* `kriya/go.mod`, `kriya/go.sum`
* `kriya/arch-go.yml`
* `kriya/branch-coverage-skip` (if app-specific coverage exclusions prove
  necessary)
* `kriya/avspec.yaml` — **stack commands only**, and only where a gate
  cannot run as written
* `kriya/feature-work/kriya-build/**`
* `avspec/examples/linkshort/**` — the end-to-end proof target

## Files/areas you must NOT touch

* `kriya/verification/**` — the acceptance tests
* `kriya/avspec.yaml` outside the `stack.commands` block — no requirement,
  acceptance-criterion, entity, or boundary edits
* `sutra/**` — sutra is built and merged; kriya consumes it
* `avspec/src/**`, `avspec/tests/**` — the verifier is not part of this work
* `scripts/**` — `branch-coverage.sh` and its regression suite are shared
  by every Go app in the repo and are not kriya's to change
* `.worktrees/**` other than this one
