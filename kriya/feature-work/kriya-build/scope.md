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
`kriya/verification/` pass under godog — 155 scenario headers, 162 runs
after outline expansion — and the pinned gate chain is green.

## In scope

* All twelve avspec modules as Go packages under `kriya/internal/`, plus
  the composition root `kriya/cmd/kriya` and the non-module packages
  `agent`, `specverify`, `clock`, `recovery`, and `acceptance`. Each module owns its
  own tables — there is no shared store package.
* All four write-ahead state machines: `BuildRun` (sixteen states) plus
  the `review_submission_state` axis, the `BuildTarget` /
  `CompletionAdvance` completion machine, `MergeAttempt`, and
  `AttributionAmbiguity` — plus the `Plan` lifecycle.
* `internal/recovery`: a `Recover` method per owning module and the
  nine-stage sequencer that runs them. It is a composition-root concern:
  a legal route exists through `devloop`, but it would put cross-module
  startup sequencing inside a domain module.
* The five seams: `claude -p` subprocess agents, the `avspec verify`
  subprocess, the roborev CLI bridge, the sutra HTTP client, and the
  target-command gate runner.
* Per-module SQLite schema and migrations for all 25 entities. There are
  37 `_key` columns, of which 15 are declared unique; each must be
  classified as an idempotency key or an identity/scoping reference before
  it is implemented — `project_key` is explicitly NOT unique.
* The bubbletea TUI: live state and the operator inbox.
* Both test rings — unit against a fake tracker, integration against a
  real sutra instance.
* Completing `kriya/arch-go.yml`: rules for `cmd/kriya`, `agent`,
  `specverify`, `clock`, `recovery`, and `acceptance`, and adding those non-module
  packages to the six `shouldOnlyDependsOn` allowlists that need them.
  **This deliberately diverges from avspec `may_import` for non-module
  edges** — avspec cannot express a package that is not a module. State it
  in the file header and log it in
  `kriya/feature-work/kriya-build/spec-gaps.md`, as sutra did in its own
  (`sutra/arch-go.yml:22-25` is the precedent, not a file to edit).
* Ownership divergence: `internal/agent` owns the `agent_invocation`
  table even though `ENT-agent-invocation` is declared under
  `MOD-orchestrator`, because no module that invokes an agent can legally
  import orchestrator. Second spec gap; log it with the first.
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
* `kriya/testdata/**` — fixtures outside `internal/`
* `kriya/*` — root-level app files: `go.mod`, `go.sum`, `kriya.toml`,
  `.golangci.yml`, `branch-coverage-skip`
* `kriya/arch-go.yml` — including the six `shouldOnlyDependsOn` allowlists,
  for non-module packages only
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
* `sutra/**` — sutra is built and merged; kriya consumes it. The
  integration ring *runs* a sutra binary, which is fine; it does not
  modify sutra's source
* `avspec/src/**`, `avspec/tests/**` — the verifier is not part of this work
* `scripts/**` — `branch-coverage.sh` and its regression suite are shared
  by every Go app in the repo and are not kriya's to change
* `.github/**` — CI is out of scope. Gates run locally, as sutra's did
* `.worktrees/**` other than this one
