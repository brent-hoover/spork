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
- [x] **The `--bare` decision — resolved as `--safe-mode`** (2026-08-21).
      Verified with a control: `--safe-mode` keeps subscription auth (exit
      0, real models, no `ANTHROPIC_API_KEY`) while disabling CLAUDE.md,
      skills, plugins, hooks, MCP servers, and custom agents. With no flag
      a target repo's `SessionStart` hook **executed**; with `--safe-mode`
      it did not. `--bare` was rejected — it forces pay-per-token billing.
      Redirecting `HOME` does not work: "Not logged in".
- [ ] **Kriya's mutation scope** — `avspec.yaml:26` runs whole-module with
      no directory argument, and M2 already lands five packages plus an
      integration ring booting a real sutra. Sutra measured 15h48m
      whole-module. Must be settled as an M2-exit item, not deferred to M3.
- [ ] Working sutra binary available to the integration ring (built from `sutra/`, not modified)

---

**One rule for every step below:** a scenario is claimed by exactly one
step, and a step's **Scenarios** field names scenario *headers* — never a
clause, a step line, or a line number. Three defects in the first draft
were the same mistake: quoting an `And …` line as if it were a scenario.

## M1 — Skeleton

Goal: the **four fast gates** — test, lint, typecheck, arch — green
against a module with no behavior, each proven to fail closed, run locally.
**CI is out of scope**: the repo has no Go workflow, sutra shipped without
one, and adding it was never asked for. Coverage and
mutation are deliberately *not* claimed here: on a module of `doc.go`-only
packages `branch-coverage.sh` takes its `expected=0` path, leaves
`sum_total=0`, skips the floor block and exits 0 having measured nothing.
They become meaningful in **M2**, not step 3: `internal/clock` turned out to
have no conditional at all (`Now()` is a single return), so it adds no arms
either. The first real branching code with tests to drive it is `planner`.
Nothing in M1 proves a requirement; it makes every later step verifiable.

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

**What:** `internal/clock` with the `Clock` interface **only** — the real
`System` implementation is deferred to the composition root (step 5c),
because until `main` can reach it, it is unreachable production code and the
no-allowlist deadcode gate rejects it. The fake lives in `internal/fakes`
(step 3b). `forbidigo` banning
`time.Now` outside `internal/clock`. **Also pin `.golangci.yml`'s
structural limits now** — `funlen`, `cyclop`, `gocognit` — because
design.md names them as the only mechanical guard against orchestrator
god-module drift, and configuring them after `orchestrator` exists is the
same retrofit the clock was pulled forward to avoid.

**Why:** the one Optimal element pulled forward. It is a retrofit if
deferred, and every recovery test needs it.

**Scenarios:** none.

**Verify:** `golangci-lint run` → exit 0. Two negative controls: add
`time.Now()` to another package and confirm it fails; add a deliberately
over-long function and confirm `funlen` fires. Coverage is **not** run here: `clock` turned out to have no conditional at
all, so it contributes no arms and the gate would still measure nothing.
The floor is first exercisable in M2 with `planner`.

### 3b. Settle where fakes live — BEFORE step 3, not after

**What:** `kriya/avspec.yaml`'s lint gate runs `deadcode` **twice**, the
second pass without `-test`, requires empty output, and says "There is NO
ALLOWLIST here and the second pass must come back empty". A fake clock,
fake `specverify`, fake `trackerclient`, and fake `Agent` are production
symbols reached only from tests — and they cannot hide in `_test.go`
because `internal/acceptance` is a different package. **Lint breaks at
step 3, on the first fake.**

Create `internal/fakes` as a test-only package and strip it from the
**production** deadcode pass only, mirroring sutra's `internal/faultsql`
(`sutra/avspec.yaml:19`). Add its arch-go rule. This is a reviewed
stack-command change to `kriya/avspec.yaml`, which `scope.md` permits.

**Why:** sutra hit this exact wall and solved it this exact way. Kriya's
lint command inherited the shape without the escape hatch.

**Ordering:** this is numbered 3b but **runs before step 3's fake**. Step 3
ships the `Clock` interface; the fake and this gate change land together,
because a fake with no home breaks lint the moment it exists. In practice
they were one commit.

**Scenarios:** none.

**Verify:** `go run golang.org/x/tools/cmd/deadcode@v0.48.0 ./...` reports
the fakes; the gate's `testonly` strip removes them; the **`-test` pass
must still be empty**, so a fake nothing uses still fails the gate.

### 5. `acceptance` harness skeleton — discovery only

**What:** `internal/acceptance` wiring godog to `kriya/verification/`,
registering all 19 feature files. **Behind a `//go:build acceptance` tag**,
so plain `go test ./...` — which *is* the `test` gate — stays green while
scenarios are still pending. A separate `go test -tags acceptance` runs
them.

Two assertions, deliberately separated: **discovery** (162 runs found) is
the definition of done and must pass from M1; **pass/fail** is per
milestone and is expected red until M6.

**Why:** without the build tag the `test` gate goes red at step 5 and
stays red until M6, making every milestone's "green" exit criterion
unattainable.

**Scenarios:** none turned green. All 19 files discovered.

**Verify:** `go test ./...` → exit 0 (tag excluded).
`go test -tags acceptance ./internal/acceptance/ -run TestDiscovery` →
asserts exactly 162, exit 0.

**Scenario filtering is by godog, not `-run`.** godog is driven from
`TestMain`, so `go test -run 'spec-intake'` filters Go test functions and
does nothing to scenarios — every pending scenario would still run and keep
the command red. The harness must expose godog's own selection (a
`-godog.tags` flag, or `GODOG_PATHS`/format options wired in `TestMain`),
and every per-step Verify below uses that.

### 5b. Step definitions land with their milestone

**What:** Not a single step. Step definitions are written in the milestone
that needs them, against module APIs that exist by then.

**Why:** the design's own risk notes "162 runs" understates the harness
surface by about an order of magnitude — `REQ-run-to-complete.feature:234`
carries fourteen `Then` steps under one header, `REQ-tui.feature:22` has
fifteen `Given`s. Writing them before the APIs exist means rewriting them.

**Scenarios:** none directly.

**Verify:** each milestone's own scenario runs.

### 5c. Composition root opens the store

**What:** `cmd/kriya` opens one SQLite connection, applies each module's
DDL in dependency order, and owns migration sequencing. Ships with the
**key classification artifact** `scope.md` requires: each of the 37 `_key`
columns labelled idempotency-key or identity/scoping reference, with the
15 declared-unique ones marked.

**Why:** steps 9, 10 and 13 all assume a live store and nothing opens one.
The classification is a scope.md deliverable with no other home, and
treating a reference as an idempotency key would fence work that should
proceed.

**Scenarios:** none.

**Verify:** `kriya build` against an empty temp dir creates the schema;
`sqlite3 .schema` shows every owned table. The classification lands as a
table in `spec-gaps.md` or a `docs/` note, reviewed.

---

## M2 — Intake and decomposition

Goal: point kriya at a ready spec and see tickets in sutra.

**M2's steps are VERTICAL SLICES, not layers.** The steps below were
originally written as one seam per step — specverify, then trackerclient,
then agent, then planner — and that ordering cannot go green. Kriya's lint
gate runs `deadcode` with a production pass and **no allowlist**, so a seam
whose first caller does not exist yet is unreachable production code and
fails the gate. A seam and the thing that calls it must land together, with
enough `cli` and `main` wiring to reach them from an entry point. Discovered
at step 6; the same constraint had already forced `clock` to ship as an
interface with its implementation deferred. Steps 6-12 are therefore
grouped into slices below rather than executed in their written order.

### 6. `specverify` seam

**What:** `internal/specverify` wrapping `avspec verify <dir> --json`,
returning `Report{Status, OK, Findings}`. A fake for the unit ring.
**A non-zero exit with a parseable payload is a `Report`, not an `error`**
— `ok=False` exits 1 and is the ordinary refusal path.

**Why:** every intake scenario needs it; three consume its findings.

**Scenarios:** none. Those refusals are operator-level ("When the operator
points kriya at the project") and cannot pass until `planner` (step 9) and
`cli` (step 12) exist; step 12 claims them.

**Verify:** `go test ./internal/specverify/...`, plus an integration test
running the real binary against `avspec/examples/linkshort` and asserting
that `ok=False` (exit 1) yields a `Report`, not an `error`.

### 7. `trackerclient` seam

**What:** `internal/trackerclient` speaking sutra's pinned OpenAPI:
projects, issues, relations, work stacks, idempotency-key headers. A fake
implementing the same interface.

**Why:** M2's output is tickets in sutra; nothing else can produce them.

**Scenarios:** none directly — enables 8-11.

**Verify:** unit tests against the fake; one integration test creating and
reading an issue against a real sutra binary.

### 8. `agent` seam and the invocation ledger

**What:** `internal/agent`: the `Agent` interface, a fake in
`internal/fakes`, tier config loading from `kriya.toml`, and **ownership of
the `agent_invocation` table**. A role with no configured tier fails at
startup. The **real `claude -p` subprocess implementation is M3** — M2
needs only the seam so `planner` can drive a fake PM agent. When it lands
the invocation is `claude -p --safe-mode --output-format stream-json …`
(see Preconditions).

**Why:** `REQ-decompose.feature:11` is "When the PM agent decomposes it" —
M2 cannot go green without it. Second spec gap made concrete.

**Scenarios:** none — the tier-routing scenarios are operator-level
("When kriya starts") and are claimed by step 12.

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
back per field", "an intake crash never allocates a second
generation", "overlapping intakes never share a generation", "a delayed
older plan can never regress the mapping", "amended spec pins a new
snapshot".

**Deferred to M5:** "working tree edits do not change a pinned build" — its
`And the build runs its gate chain and recovery later replays a step`
clause needs the gate chain (M3) and a supersession-era replay (M5).

**Verify:** `go test -tags acceptance ./internal/acceptance/ -godog.paths ../../verification/REQ-spec-intake.feature`
— 8 from this step; 11 of 12 for the file once step 12 lands.

### 10. `planner`: decomposition and `BuildTarget.epic_state`

**What:** tracer-bullet decomposition via the PM agent, `PlannedTicket`
rows, the umbrella epic written **write-ahead** as `BuildTarget` with
`epic_state`, relations for risk ordering and phase barriers,
`decomposition_key` state-aware retries, and supersession/retirement.

**Why:** turns a pinned snapshot into tickets. `epic_state` is M2, not M4 —
the row is written ahead of the epic's creation.

**Scenarios:** `REQ-decompose.feature` — "tracer bullets carry traceable
acceptance criteria", "one epic umbrellas the build target", "phases run behind plan-wide
barriers", "same-key retries are state-aware", "a stale intake recovering
late cannot supersede a newer head", "reverting to a previously seen spec
is a fresh plan, not a replay", "supersession retires the predecessor
before activating".

**Deferred to M3:** "retirement distinguishes pending from issued work" —
it requires a ticket claimed by a **live build** to stamp disposition
`bound`, and no `BuildRun` exists until `orchestrator` lands.

**Deferred to M5:** "relations wire risk ordering and parallelism" — its
`And an agent popping work receives an unblocked ticket` clause is pop
admission, which is M5.

**Verify:** `-godog.paths ../../verification/REQ-decompose.feature` — 7 from this step, **8 of 10**
for the file once step 13 adds "every parked state recovers forward".

### 11. `planner`: risk-first ticketing

**What:** risk items become spike tickets carrying the `risk`/`spike`
label, with block-relations so dependents cannot proceed.

**Why:** `CON-risk-first` requires risks retired before dependents build.

**Scenarios:** `REQ-risk-first.feature` — "each risk becomes a blocking
spike".

**Verify:** `go test -tags acceptance ./internal/acceptance/ -godog.paths ../../verification/REQ-risk-first.feature`
— 1 of 6. The other five need pop admission and architect escalation, both
M5; the research path itself is M4, so "needs the research path" was the
wrong reason.

### 12. `cli`: `kriya build` and `kriya status`

**What:** `internal/cli` with `build <project>` and `status [--json]`,
wired in `cmd/kriya`. **Not `learn add`** — the learning store is
`context`, which is M3, and nothing in M2 needs it.

**Why:** the operator-level entry point every M2 scenario starts from
("When the operator points kriya at the project", "When kriya starts").

**Scenarios:** `REQ-spec-intake.feature` — "draft spec is refused",
"erroring spec is refused", "a ready claim with todo findings is refused".
`REQ-tier-routing.feature` — "roles map to tiers in configuration",
"a missing tier fails loudly".

**Verify:**
`go test -tags acceptance ./internal/acceptance/ -godog.paths ../../verification/REQ-spec-intake.feature`
and the tier-routing file — the three intake refusals and two tier-routing
scenarios pass. Plus
`kriya build avspec/examples/linkshort` against a real sutra creating a
project and tickets.

### 13. `recovery` stages 1-2

**What:** `internal/recovery` with the sequencer and
`Recover(ctx, stage)` on `planner` — the stage parameter is required
because the nine-stage table invokes `orchestrator` at 1, 4, 7 and 8 and
`planner` at 2 and 3, which one unqualified method cannot express. M2 lands
**stage 2 only**; stage 1's pop-binding sweep needs orchestrator-owned
`BuildRun` rows and arrives in M3 with the real `PopBinder`.

The M2 `PopBinder` genuinely does nothing, and that is correct rather than
a stub excused: in M2 there are no `BuildRun` rows to bind. It is wired in
`cmd/kriya`, not in `recovery`, so `recovery` carries no knowledge of what
is not built yet. The scenario that needs a real binding —
`REQ-decompose.feature:79`, "the ticket claimed by a live build stamps
disposition **bound**" — is claimed by M3, not faked here.

**Why:** M2's intake-crash scenarios call recovery. Landing the sequencer
now is why M3 and M4 can grow it rather than invent it.

**Scenarios:** `REQ-decompose.feature` — "every parked state recovers
forward", "retirement distinguishes pending from issued work".
(The intake-crash scenario is claimed by step 9.)

Note "every parked state recovers forward" has operator actions —
`ACT-plan-restore`, `ACT-plan-retry` — whose TUI vehicle is M6. The harness
calls `planner`'s module API directly, which design.md's acceptance drive
surface permits.

**Verify:** crash-injection tests: fake seam accepts, harness suppresses
the outcome write, store reopens, `recovery.Run` reconciles.

**M2 done when:** `spec-intake` 11/12, `decompose` **8/10**, `risk-first` 1/6,
`tier-routing` 2/3 under `-tags acceptance`; `go test ./...` green; the
four fast gates green; coverage above the 75 floor; and **kriya's mutation
scope decided and its gate PASSING** — zero survivors, zero timeouts.
"Run once" would have been a milestone declaring itself green on a gate
whose result it never checked.

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
`orchestrator` BuildRun **code traversal only**, plus the real `PopBinder`
· `recovery` **stage 1** and stages 5-7, 9.
**Proves:** workspaces, pair-loop, the four gate REQs, context-assembly,
thread-capture, tier-routing, sa-agent (partial), **spec-intake
(remainder — "working tree edits do not change a pinned build", which needs
the gate chain and BuildRun recovery, both here)**, and **decompose's
"retirement distinguishes pending from issued work"**, which needs a live
BuildRun to stamp disposition `bound`.

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
remainders of risk-first, decompose (its pop-admission scenario), and
sa-agent. **Not spec-intake** — that completes in M3.

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
| `REQ-spec-intake.feature` | 12 | M2 (11), M3 (1) | 9, 12 |
| `REQ-decompose.feature` | 10 | M2 (8), M3 (1), M5 (1) | 10, 13 |
| `REQ-risk-first.feature` | 6 | M2 (1), M5 (5) | 11 |
| `REQ-tier-routing.feature` | 3 | M2 (2), M3 (1) | 12 |
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
- ~~The `--bare` decision~~ — resolved as `--safe-mode`; see Preconditions.
- Kriya building kriya.

## Change log

- 2026-08-21: Initial draft (Brent Hoover)
- 2026-08-21: Applied plan-reviewer findings — put the acceptance suite
  behind a build tag so the `test` gate is not red from step 5 to M6;
  added step 3b settling where fakes live before the first one trips the
  no-allowlist deadcode gate; added step 5c opening the store and
  producing the 37-key classification; corrected three steps that quoted a
  step *clause* as if it were a scenario header; made step 13's
  `PopBinder` a programmable fake rather than a no-op that could not
  produce the `bound` stamp; moved operator-level scenarios from steps 6
  and 8 to step 12 where a CLI exists to drive them; scheduled the
  mutation-scope decision as an M2 precondition; pinned `.golangci.yml`
  structural limits in M1; restated M1's goal as the four fast gates,
  since coverage measures nothing on a `doc.go`-only module; and recorded
  the `--bare` decision as `--safe-mode` (Brent Hoover)
