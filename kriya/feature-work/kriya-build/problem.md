---
title: Kriya Build — Problem Statement
type: problem
status: draft
owner: Brent Hoover
created: 2026-08-21
updated: 2026-08-21
---

# Kriya Build — Problem Statement

## Context

Spork has three components. **avspec** is the spec format and its verifier
(Python, built). **sutra** is the issue tracker for agent-driven work —
specified, then built, and merged to `develop` on 2026-08-21. **kriya** is
the build engine: it takes a ready avspec, decomposes it into
tracer-bullet tickets in sutra, and drives coding agents through a
pair-programming loop — small commits, continuous roborev review, fix,
recommit — then the machine gate chain, product-owner validation, and a
sutra Review for human approval, looping until the app is complete.

Kriya exists today only as a *ready* avspec: `kriya/avspec.yaml` (1,738
lines — 19 requirements, 12 modules, 10 constitution principles),
`kriya/verification/*.feature` (19 files, 1,287 lines of Gherkin mapped
from every acceptance criterion), and `kriya/arch-go.yml` (declared import
boundaries). No line of Go exists.

The specified surface carries **two delivery paths**, not one.
`CON-gate-chain` governs code deliverables through the full chain; research
and spike tickets carry a **document** deliverable and traverse a separate
path — documented finding → document Review → human approval → close —
with no code gates, because none apply. `CON-risk-first` and
`REQ-risk-first` make that second path mandatory: a risk item is retired by
research or a spike before anything depending on it is built.

Sutra was built the same way and is the precedent this work follows: same
repo, same stack family, same gate chain, same one-branch-sliced-commits
delivery. Kriya is the component sutra was built *for* — sutra's contract
guarantees (keyed replay, fencing revisions, cascade events, watermarked
feeds) exist because kriya's crash-recovery protocols depend on them.

## Problem

There is no running kriya. Nothing decomposes a spec into tickets, no
orchestrator advances a build through the gate chain, no dev agent is
driven through a pair loop, and none of the 1,287 lines of verification
scenarios execute against real software. Until kriya runs:

- **Every build in spork is hand-driven.** Sutra's own build — the
  precedent — was executed manually across weeks of sessions: decomposing
  the spec, sequencing modules, running gates, shepherding roborev rounds,
  and deciding when a thing was done. Kriya exists to make that
  repeatable, and its absence means the next app repeats the manual cost.
- **Sutra's contract guarantees remain unexercised by their intended
  consumer.** The fences, idempotency keys, and cascade semantics were
  specified for kriya's recovery protocols and have been proven only
  against sutra's own tests, never against the consumer whose design
  motivated them.
- **The spec's claim of internal consistency has never been tested against
  executable behavior.** Nineteen requirements' worth of protocol — plan
  supersession, pop fences, binding races, retirement and reopen rules —
  are asserted in Gherkin that has never run.

## Complexity drivers

- **Scale**: not user or data volume — single-operator, single-machine,
  and the issue graph grows linearly with activity. The scale fact is
  **wall-clock per gate chain**, and it is severe: kriya runs the whole
  chain *per ticket, per touched module*. Sutra's coverage gate alone
  takes ~20 minutes (gobco instruments one package at a time), and its
  whole-module mutation run measured **15h48m over ~1678 mutants**. A
  build of any size multiplies these.
- **Concurrency**: inherent, and it is most of the specified surface.
  Multiple dev agents run in parallel workspaces against one sutra
  instance while the operator mutates the same issue graph. The spec pins
  races as behavior, not edge cases: a global pop fence while any plan
  head is unactivated, carried-forward pop races where a ticket claimed
  mid-supersession binds to the predecessor snapshot, verdict-ABA fencing
  on review consumption, and write-ahead import keys replayed on either
  side of sutra's acceptance. These interleavings were found by review
  rather than by design — the avspec carries a provenance note listing
  them — and they now have acceptance criteria and scenarios (e.g. the
  global pop fence at `kriya/avspec.yaml:193`, scenarios at
  `kriya/verification/REQ-parallel-build.feature:25-38`).
- **Failure modes**: severe and silent. Kriya drives autonomous agents
  that commit code and merge it on approval. A gate chain that mis-orders
  or skips a stage ships unreviewed code under a passing report; a
  recovery bug duplicates a dev session's work or loses a transcript; a
  binding bug attributes a ticket's acceptance criteria to the wrong
  decomposition and the agent builds to a superseded spec. The
  constitution calls the chain inviolable because the failure is invisible
  from inside a green run.
- **Cross-cutting policies**: no secrets in source — model credentials and
  the sutra endpoint are injected at runtime (`CON-no-secrets`), and
  `AC-tier-config` forbids hardcoded model identifiers outright.
  Auditability is load-bearing: every agent execution writes an
  `AgentInvocation` recording role, configured tier, and the resolved
  model that actually ran; every dev transcript auto-imports into sutra's
  thread catalog, write-ahead, for every outcome including crashes. The
  TUI is specified to hold no second source of truth — it reads the same
  durable records recovery reads.

## Constraints

- **The avspec is pinned and ready.** `avspec verify ../kriya` (run from
  `avspec/`) must still print `status=ready errors=0 todos=0 ok=True` when
  the build lands.
- **All 19 `kriya/verification/*.feature` files must pass under godog** —
  they are the acceptance tests, not documentation.
- **Stack is fixed by the spec**: Go 1.25, `go` toolchain, godog,
  SQLite (durable observability plane and recovery rows), bubbletea
  (+ bubbles/lipgloss) for the TUI.
- **The gate chain is pinned** and is the same one sutra's build proved
  out: golangci-lint v2.1.6 + deadcode v0.48.0 (both passes, and kriya's
  spec explicitly grants **no allowlist**), `go vet`, arch-go v1.7.0
  against `kriya/arch-go.yml`, `../scripts/branch-coverage.sh`, and
  go-gremlins v0.5.0 with zero survivors and zero timeouts.
- **Kriya must pass the chain it enforces.** It is the build engine; a
  gate kriya cannot itself clear is a gate it has no standing to impose.
- **Coverage is enforced as a floor, for every app** (decision
  2026-08-21). The policy lives in each target's own stack command string
  — sutra ships `BRANCH_COVERAGE_FLOOR=75 ../scripts/branch-coverage.sh`
  — and kriya executes that command verbatim from the pinned SpecSnapshot.
  This makes `AC-coverage-every-arm`'s "no threshold to slip under" and
  the feature file's "no overall percentage can compensate" false as
  written; amending them is in scope (see Non-goals and Open questions).
- **roborev is the review engine** (`CON-buy-over-build`) — kriya drives
  it, never reimplements it. Risk R1 is retired: roborev was proven
  drivable programmatically (enqueue, poll, respond, close), with the
  known facts that enqueue accepts no caller correlation key and does not
  dedupe by SHA.
- **R2 remains open in the spec** (`kriya/avspec.yaml:87-91`): whether the
  mutation gate kriya runs *for the targets it builds* is fast enough
  per-ticket, or viable only as a slower periodic gate. The gate command
  stands either way; the cadence does not.
- **The orchestrator is a deterministic state machine** — this is imposed
  by `CON-deterministic-orchestrator`, not chosen here. The spec sequences
  an explicit state table and delegates all judgment to agent modules, and
  records why: in earlier attempts the orchestrator became a god module,
  and the state table plus structural lint limits are the spec's named
  correction.
- **Process**: work in the `feat/kriya-build` worktree; incremental
  commits; every commit to roborev; a clean whole-branch pass gates merge.

## Non-goals

- **Behavioral spec changes.** The avspec is ready; its protocol is not up
  for redesign here. *In* scope, with their own review: stack-command
  changes to kriya's own spec (the coverage floor, mutation scope) and the
  coverage-requirement wording the floor decision invalidates.
- **Reimplementing or patching roborev.** `CON-buy-over-build` forbids
  reimplementation; this build also does not modify roborev to add the
  correlation key or SHA dedupe R1 found missing. Kriya works within the
  protocol R1 settled.
- **Other issue trackers.** `MOD-tracker-client` is the seam where they
  would plug in; only sutra is implemented.
- **avspec slice 2** (constitution `check:` attachments, cross-manifest
  links, `avspec render`) — deferred in
  `avspec/feature-work/avspec-0.3-restart/deferred.md`.
- **Sutra's coverage residue** (web/cli/cmd arms, the three shadowed
  import rules) — separate work on a separate branch.
- **Kriya building kriya.** Bootstrapping is a later question; this build
  is hand-driven, exactly as sutra's was.
- **Multi-machine, hosted, or multi-operator operation.**

## Success criteria

- `go test ./...` green, with every scenario in `kriya/verification/`
  executing and passing under godog.
- The full pinned gate chain passes: lint (both deadcode passes, no
  allowlist), `go vet`, arch-go against declared boundaries, the branch
  coverage gate, and gremlins with zero survivors and zero timeouts.
- `avspec verify ../kriya` (run from `avspec/`) prints
  `status=ready errors=0 todos=0 ok=True`.
- **One real end-to-end driven build completes**, measured observably: an
  intake-eligible target spec is accepted, its epic closes in sutra under
  an approved Review, every tracer ticket it produced traversed the full
  chain to merge, and the count of operator escalations is recorded (not
  bounded — recorded, because the first run's escalation count is data
  about kriya, not a pass/fail line).
- Whole-branch roborev pass, then merge to `develop`.

## Open questions

- [ ] **What floor does kriya's own build carry, and when is it set?**
      The floor policy is decided; the number is not. Sutra's 75 was set
      *after* measuring 78.8%, following a full fault-injection campaign.
      Kriya has zero Go today, so any number now is a guess. Recommended:
      leave kriya's coverage command floor-less until the first module
      lands, measure, then set it with a few points of headroom — the same
      shape as sutra's "measure the mutation gate after module 1".
- [ ] **How far does the coverage spec amendment go?**
      `AC-coverage-every-arm` becomes a misnomer under a floor; renaming it
      (e.g. `AC-coverage-floor`) cascades to its `test:` mapping and the
      scenario name, and `REQ-gate-branch-coverage.feature:2-3,10` assert
      "no percentage threshold" as a universal. Scenarios 2 and 3
      (test-first ordering, commit-pinned upsert) are policy-neutral and
      survive untouched.
- [ ] **How does the gate name uncovered arms in the GateResult detail?**
      `AC-coverage-recorded` requires uncovered arms "named in detail for
      the dev agent", but `MOD-gates` only runs the target's
      snapshot-resolved command and observes its exit — it does not parse
      coverage itself. Whether detail comes from captured stdout, a
      convention on the command's output format, or an artifact the
      command writes is undesigned, and it blocks `MOD-gates`. Same
      question applies to every gate whose failures must be actionable.
- [ ] **Which intake-eligible target proves the engine end-to-end, and is
      authoring one in scope?** No existing target qualifies.
      `avspec/examples/linkshort` is refused by `AC-intake-commands`: its
      three modules (`MOD-api`, `MOD-domain`, `MOD-data`) carry no stack
      overrides, and the project stack declares only `install`, `test`,
      and `lint` — missing `typecheck`, `arch`, `coverage`, and
      `mutation` — and it is TypeScript/pnpm/cucumber-js, so it would also
      exercise a non-Go toolchain. Extending linkshort, authoring a small
      Go target, or building sutra's own residue are the candidates.
- [ ] **How is a coding agent actually invoked?** The spec is deliberately
      role-and-tier-neutral: `AC-tier-config` requires roles map to tiers
      in configuration with no hardcoded model ids, and
      `ENT-agent-invocation` records the resolved model — but nothing
      pins the mechanism (Claude Code CLI subprocess, the Agent SDK, or
      direct API). This decision shapes `MOD-dev-loop`, transcript
      capture, and how `AC-tier-observed` is proven.
- [ ] **What does the test suite run sutra against?** `MOD-tracker-client`
      speaks sutra's HTTP API. The scenarios need a tracker — an
      in-process sutra, a spawned binary, or a fake honoring the pinned
      OpenAPI contract — and the choice determines whether the suite
      proves kriya against sutra's real fences or against a stub of them.
- [ ] **Mutation gate scope for kriya's own code.** Kriya's mutation
      command takes no directory argument — a whole-module run. Sutra
      measured whole-module at 88.74% efficacy over ~1678 mutants in
      **15h48m** and scoped its gate to the eight domain-core packages to
      make it usable. Kriya likely needs the same narrowing, which is a
      stack-command change in its own spec. (Distinct from R2, which asks
      the cadence question for *targets*.)
- [ ] **What model access does the end-to-end proof assume?**
      `CON-no-secrets` covers credentials but not availability. A real
      driven build spends real tokens against a rate-limited API, and the
      run is not offline-reproducible — a constraint on a success
      criterion that requires real agent runs.
- [ ] **Delivery shape.** Sutra's answer was one branch, modules as
      ordered commits in dependency order, roborev per commit, one merge.
      Same here, or sliced into separately-merged milestones given kriya's
      12 modules and the TUI?

## Change log

- 2026-08-21: Initial draft (Brent Hoover)
- 2026-08-21: Applied problem-reviewer findings — corrected constitution
  count (10, not 11), restated Scale as wall-clock per gate chain,
  reframed the concurrency checklist as provenance rather than open spec
  work, added the research/spike document path and R2, scoped the
  spec-change non-goal to behavioral changes, made the end-to-end
  criterion observable, replaced linkshort as the proof target (refused by
  `AC-intake-commands`), and added open questions for GateResult detail,
  model access, and roborev patching scope (Brent Hoover)
- 2026-08-21: Recorded the coverage-floor decision — every app gates on a
  floor including branch coverage; the policy lives in each target's stack
  command and kriya executes it verbatim (Brent Hoover)
