---
title: Kriya Build — Implementation Plan
type: plan
status: draft
owner: Brent Hoover
created: 2026-08-21
updated: 2026-08-21
design: ./design.md
---

# Kriya Build — Implementation Plan

## Overview

Six merged milestones, each landing on `develop` green. Order is forced by
the arch-go import graph and by which scenarios can pass at all: M1 makes
the gate chain run against an empty module, M2 gets a spec into sutra as
tickets, M3 drives one ticket through the gate chain, M4 adds review and
completion, M5 closes the outer loop with parallelism and full recovery,
M6 adds the operator surface and proves the engine on linkshort.

**M1 and M2 are planned to atomic steps below. M3-M6 are planned to
tasks**, expanded when their milestone starts. That is deliberate: the
later milestones depend on facts this build has not produced yet — real
module sizes for the mutation scope, real gate runtimes, and whatever the
first driven agent run teaches. Writing 162 scenarios' worth of steps now
would be fiction.

## Preconditions

- [x] `design.md` — five review passes, all findings applied
- [x] All 19 `.feature` files exist and are frozen (written during the spec phase)
- [x] `scope.md` filled with objective, allowlist, and non-goals
- [x] `spec-gaps.md` records the three ownership/boundary divergences
- [x] Stack, agent mechanism, coverage floor, test doubles, delivery shape decided
- [ ] **`--bare` decision** — needed before M3's first real dev-agent run, not before M1
- [ ] Working sutra binary available to the integration ring (built from `sutra/`, not modified)

---

## M1 — Skeleton

Goal: `go test ./...` and the full gate chain run green against a module
with no behavior. Nothing here proves a requirement; it makes every later
step verifiable.

### 1. Go module and package skeleton

**What:** `kriya/go.mod` (Go 1.25). Create all eighteen package
directories from the design's layout, each with a doc.go stating its
responsibility. `cmd/kriya` with a `main()` that parses no flags and
exits 0.

**Why:** arch-go needs packages to exist before its rules mean anything.

**Scenarios:** none.

**Verify:** `cd kriya && go build ./...` → exit 0.

### 2. Complete `arch-go.yml` and prove it fails closed

**What:** Add rules for `cmd/kriya`, `agent`, `specverify`, `clock`,
`recovery`, `acceptance`. Add those non-module packages to the six
`shouldOnlyDependsOn` allowlists that need them (planner, orchestrator,
devloop, context, cli, tui). Update the file header to state the
divergence from avspec `may_import` for non-module edges.

**Why:** arch-go requires 100% package coverage; without this the gate
fails on the first commit. This is also the first spec gap made concrete.

**Scenarios:** none.

**Verify:** `go run github.com/arch-go/arch-go@v1.7.0` → exit 0.
Then **prove the gate is not vacuous**: add a temporary illegal import
(`workspace` importing `planner`), confirm arch-go exits non-zero, remove it.

### 3. `clock` package and the no-`time.Now()` rule

**What:** `internal/clock` with a `Clock` interface, a real
implementation, and a fake with settable time. A `golangci-lint` custom
rule (or `forbidigo`) banning `time.Now` outside `internal/clock`.

**Why:** the one Optimal element pulled forward. It is a retrofit if
deferred, and every recovery test needs it.

**Scenarios:** none.

**Verify:** `golangci-lint run` → exit 0; add `time.Now()` to any other
package and confirm it fails.

### 4. CI with both toolchains

**What:** Add a Go job to `.github/workflows/verify.yml` running install,
test, lint, typecheck, arch against `kriya/`. Keep the existing uv job;
the integration ring needs both because `specverify` shells out to
`avspec verify`.

**Why:** the repo has no Go CI at all today.

**Scenarios:** none.

**Verify:** push the branch; both jobs green.

### 5. `acceptance` harness skeleton

**What:** `internal/acceptance` wiring godog to `kriya/verification/`,
with a `TestMain` that registers all 19 feature files and every step as
pending. Assert the suite **discovers 162 runs**.

**Why:** the discovery count is the definition of done, and step
definitions are the real harness cost — the design's risk notes several
scenarios carry a dozen-plus steps under one header.

**Scenarios:** all 19 files, discovered and reported pending.

**Verify:** `go test ./internal/acceptance/...` reports 162 undefined
scenarios, exit non-zero (pending is not passing).

---

## M2 — Intake and decomposition

Goal: point kriya at a ready spec and see tickets in sutra.

### 6. `specverify` seam

**What:** `internal/specverify` wrapping `avspec verify <dir> --json`,
returning `Report{Status, OK, Findings}`. A fake for the unit ring.
**A non-zero exit with a parseable payload is a `Report`, not an `error`**
— `ok=False` exits 1 and is the ordinary refusal path.

**Why:** every intake scenario needs it; three consume its findings.

**Scenarios:** `REQ-spec-intake.feature` — "draft spec is refused",
"erroring spec is refused", "a ready claim with todo findings is refused".

**Verify:** `go test ./internal/specverify/...`, plus an integration test
running the real binary against `avspec/examples/linkshort`.

### 7. `trackerclient` seam

**What:** `internal/trackerclient` speaking sutra's pinned OpenAPI:
projects, issues, relations, work stacks, idempotency-key headers. A fake
implementing the same interface.

**Why:** M2's output is tickets in sutra; nothing else can produce them.

**Scenarios:** none directly — enables 8-11.

**Verify:** unit tests against the fake; one integration test creating and
reading an issue against a real sutra binary.

### 8. `agent` seam and the invocation ledger

**What:** `internal/agent`: the `Agent` interface, the `claude -p`
implementation (stream-json parsing for `system/init` model, `session_id`,
`parent_tool_use_id`, `api_retry`), a fake, tier config loading from
`kriya.toml`, and **ownership of the `agent_invocation` table**.
A role with no configured tier fails at startup.

**Why:** `REQ-decompose.feature:11` is "When the PM agent decomposes it" —
M2 cannot go green without it. Second spec gap made concrete.

**Scenarios:** `REQ-tier-routing.feature` — "roles map to tiers in
configuration", "a missing tier fails loudly"; and the plan-scoped PM
clause of "config changes take effect and runs record models".

**Verify:** `go test ./internal/agent/...`; assert a PM invocation records
role, tier, and resolved model with a `plan` ref and no `build` ref.

### 9. `planner`: intake and the `Plan` lifecycle

**What:** `internal/planner`: `SpecSnapshot` pinning with resolved
per-module commands, `IntakeAttempt` generation fencing, `SpecMapping`,
`Plan` states, and the `AC-intake-commands` check (six commands per
module's effective stack). Declares the `PopBinder` interface it does not
yet implement against.

**Why:** the core of M2.

**Scenarios:** `REQ-spec-intake.feature` — "ready spec is pinned",
"missing module command refuses intake", "complete module override
resolves to the module's own commands", "partial module override falls
back per field", "working tree edits do not change a pinned build", "an
intake crash never allocates a second generation", "overlapping intakes
never share a generation", "a delayed older plan can never regress the
mapping", "amended spec pins a new snapshot".

**Verify:** `go test ./internal/acceptance/... -run 'spec-intake'` — 11 of
12 pass; ":55" (`every parked state recovers forward` prerequisites)
remains pending until M5.

### 10. `planner`: decomposition and `BuildTarget.epic_state`

**What:** tracer-bullet decomposition via the PM agent, `PlannedTicket`
rows, the umbrella epic written **write-ahead** as `BuildTarget` with
`epic_state`, relations for risk ordering and phase barriers,
`decomposition_key` state-aware retries, and supersession/retirement.

**Why:** turns a pinned snapshot into tickets. `epic_state` is M2, not M4 —
the row is written ahead of the epic's creation.

**Scenarios:** `REQ-decompose.feature` — "tracer bullets carry traceable
acceptance criteria", "one epic umbrellas the build target", "relations
wire risk ordering and parallelism", "phases run behind plan-wide
barriers", "same-key retries are state-aware", "a stale intake recovering
late cannot supersede a newer head", "reverting to a previously seen spec
is a fresh plan, not a replay", "supersession retires the predecessor
before activating", "retirement distinguishes pending from issued work".

**Verify:** `go test ./internal/acceptance/... -run 'decompose'` — 9 of 10
pass; ":31" (pop admission) is M5.

### 11. `planner`: risk-first ticketing

**What:** risk items become spike tickets carrying the `risk`/`spike`
label, with block-relations so dependents cannot proceed.

**Why:** `CON-risk-first` requires risks retired before dependents build.

**Scenarios:** `REQ-risk-first.feature` — "each risk becomes a blocking
spike".

**Verify:** `go test ./internal/acceptance/... -run 'risk-first'` — 1 of 6
passes; the other 5 need the research path (M5).

### 12. `cli`: `kriya build` and `kriya status`

**What:** `internal/cli` with `build <project>`, `status [--json]`,
`learn add`. Wired in `cmd/kriya`.

**Why:** how M2's scenarios are driven at all.

**Scenarios:** the operator-level steps across the M2 files.

**Verify:** `kriya build avspec/examples/linkshort` creates a project and
tickets in a real sutra; `kriya status --json` reports the plan.

### 13. `recovery` stages 1-2

**What:** `internal/recovery` with the sequencer, `Recover(ctx)` on
`planner`, and stage 1's pop-binding sweep behind the `PopBinder`
interface — implemented as a no-op stub until `orchestrator` exists in M3.
Wired into `cmd/kriya` startup.

**Why:** M2's intake-crash scenarios call recovery. Landing the sequencer
now is why M3 and M4 can grow it rather than invent it.

**Scenarios:** `REQ-spec-intake.feature` — "an intake crash never
allocates a second generation"; `REQ-decompose.feature` — "every parked
state recovers forward" (plan states only).

**Verify:** crash-injection tests: fake seam accepts, harness suppresses
the outcome write, store reopens, `recovery.Run` reconciles.

**M2 done when:** `spec-intake` 11/12, `decompose` 9/10, `risk-first` 1/6,
`tier-routing` 2/3 — and the full gate chain green.

---

## M3-M6 — tasks

Expanded to steps when each milestone starts.

### M3 — One ticket through the chain
`workspace` (git shell-out seam, worktree lifecycle) · `context` (bundle
assembly, gate wrapper scripts, learning store) · `agent` real subprocess
implementation · `devloop` (pair loop, commit cadence, `DevSession`) ·
`reviewbridge` (roborev enqueue/poll/respond/close, `EnqueueAttempt`
write-ahead) · `gates` (six gate runners, `GateResult` with the
exit/stdout/stderr envelope) · `architect` (impasse, escalation) ·
`orchestrator` BuildRun **code traversal only** · `recovery` stages 5-7, 9.
**Proves:** workspaces, pair-loop, the four gate REQs, context-assembly,
thread-capture, tier-routing, sa-agent (partial).

### M4 — Review, merge, completion
`owner` (AC validation, anti-gaming) · `orchestrator` `MergeAttempt` and
the BuildRun **research path** · `planner` `completion_state` +
`CompletionAdvance` · `recovery` stages 3, 8.
**Proves:** po-validation, submit-review.

### M5 — The outer loop
`orchestrator` full transition table, parallelism, pop admission,
`AttributionAmbiguity` · `recovery` stage 4 · learning loop feeding
context.
**Proves:** run-to-complete, parallel-build, learning-loop, and the
remainders of risk-first, spec-intake, decompose, sa-agent.

### M6 — Operator surface and proof
`tui` (bubbletea; live state, the inbox, operator actions via orchestrator
pass-throughs and the read facade) · then the **linkshort end-to-end
driven build**.
**Proves:** tui, and the design's observable success criterion.

---

## Scenario coverage

Every scenario is claimed by exactly one milestone. Files split across
milestones are marked, matching design.md.

| Feature file | Scenarios | Milestone(s) | Steps |
|---|---|---|---|
| `REQ-spec-intake.feature` | 12 | M2 (11), M5 (1) | 6, 9, 13 |
| `REQ-decompose.feature` | 10 | M2 (9), M5 (1) | 10, 13 |
| `REQ-risk-first.feature` | 6 | M2 (1), M5 (5) | 11 |
| `REQ-tier-routing.feature` | 3 | M2 (2), M3 (1) | 8 |
| `REQ-workspaces.feature` | 5 | M3 | M3 |
| `REQ-pair-loop.feature` | 6 | M3 | M3 |
| `REQ-context-assembly.feature` | 6 | M3 | M3 |
| `REQ-gate-structure.feature` | 4 | M3 | M3 |
| `REQ-gate-typing.feature` | 2 | M3 | M3 |
| `REQ-gate-branch-coverage.feature` | 3 | M3 | M3 |
| `REQ-gate-mutation.feature` | 2 | M3 | M3 |
| `REQ-thread-capture.feature` | 5 | M3 | M3 |
| `REQ-sa-agent.feature` | 4 | M3 (3), M5 (1) | M3, M5 |
| `REQ-po-validation.feature` | 4 | M4 | M4 |
| `REQ-submit-review.feature` | 24 | M4 | M4 |
| `REQ-run-to-complete.feature` | 44 | M5 | M5 |
| `REQ-parallel-build.feature` | 6 | M5 | M5 |
| `REQ-learning-loop.feature` | 5 | M5 | M5 |
| `REQ-tui.feature` | 4 | M6 | M6 |
| **Total** | **155 headers / 162 runs** | | |

No scenario is unclaimed.

## Rollback

Each milestone is a branch merged to `develop` only when its scenarios and
the full gate chain are green, so the fallback is always the previous
merged milestone. Within a milestone, commits are incremental and
roborev-reviewed, so `git revert` of a single commit is the unit of
undo. Nothing kriya does is destructive outside its own worktrees and its
own SQLite file; the sutra instance used by the integration ring is
disposable per suite.

## Out of scope for this plan

- Expanding M3-M6 into atomic steps before their milestone starts.
- Behavioral spec changes; only stack-command changes, with their own review.
- Editing `kriya/verification/**`.
- The `--bare` decision (needed by M3, not schedulable here).
- Kriya building kriya.

## Change log

- 2026-08-21: Initial draft (Brent Hoover)
