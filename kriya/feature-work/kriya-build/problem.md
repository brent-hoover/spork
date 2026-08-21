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

- [x] **How is a coding agent invoked?** **The Claude Code CLI as a
      subprocess** (`claude -p --output-format stream-json`). Forced and
      confirmed: the Agent SDK is a library for **Python and TypeScript
      only**, and the docs prescribe the CLI subprocess to drive the same
      agent loop from any other language. Kriya is Go. A flip to Python to
      reach the SDK was considered and rejected 2026-08-21 — the repo's
      Python side carries only ruff and pytest, so kriya would have had to
      rebuild the branch-coverage, fault-injection, mutation, arch, and
      dead-code apparatus the Go side just finished validating.
      This is not a downgrade: the CLI *is* the Agent SDK exposed as a
      process, same agent loop and tools. What the SDK adds is in-process
      ergonomics kriya does not need — permissions are per-ticket policy
      that `--allowedTools` expresses, interrupt is `SIGINT`, and the
      subprocess boundary is a positive for parallel dev agents
      (independent kill and timeout, crash containment), which is what
      `CON-deterministic-orchestrator` wants structurally.
      Three ACs get concrete mechanisms from it: `system/init` reports the
      resolved model (`AC-tier-observed`), `session_id` stamps every
      payload (`REQ-thread-capture`), and `stream-json` with
      `parent_tool_use_id` feeds the live TUI (`AC-tui-overview`).
      Billing is a non-differentiator — Agent SDK and `claude -p` both draw
      on the Claude subscription's usage limits; the June 15 2026
      Agent-SDK credit scheme is announced but paused. Kriya stays agnostic
      about auth: it is ambient environment state, not kriya's business.

- [x] **What does the test suite run sutra against?** Both, split by test
      kind: **unit tests run against a fake**, **integration tests run
      against a real sutra instance**. The fake keeps unit tests fast and
      hermetic; the real instance is what proves kriya against sutra's
      actual fences — verdict-ABA, idempotency replay, atomic pop — which
      exist specifically for kriya and cannot be proven against a stub.
- [x] **Delivery shape.** **Merged milestones** — not sutra's
      one-branch-one-merge. Kriya's 12 modules plus the TUI is a larger
      surface than sutra's, and milestone merges keep `develop` moving
      rather than holding a months-long branch.
- [x] **The coverage bar.** Every app gates on a **floor**, branch
      coverage included, at **75** repo-wide — the value sutra measured
      its way to (78.8% after the fault-injection campaign). Amendment
      landed 2026-08-21: `REQ-gate-branch-coverage` retitled,
      `AC-coverage-every-arm` renamed to `AC-coverage-floor` and restated
      so the bar belongs to the target rather than to kriya, the scenario
      rewritten, kriya's own stack command given the floor, and R3 retired
      in full. Kriya counts no arms and infers no threshold; a target that
      wants the every-arm rule omits the floor and the same script
      enforces it.
- [x] **Which target proves the engine end-to-end?** **linkshort**, made
      buildable 2026-08-21. It was refused by `AC-intake-commands` — one
      project stack block, no module overrides, so all four modules
      (`MOD-api`, `MOD-domain`, `MOD-data`, `MOD-web`) inherited a stack
      declaring only `install`, `test`, and `lint`: 16 refusal reasons.
      Its stack is now Go 1.25 with all six required commands plus a
      hand-written `arch-go.yml`. Simulated against the intake rule: 4
      modules PASS, 0 refusals; still verifies
      `status=ready errors=0 todos=0 ok=True`.

### Deferred — these block finishing, not starting

- [ ] **`--bare` forces API-key billing — reproducibility vs the
      subscription.** `--bare` is the recommended mode for scripted calls
      and is becoming the default for `-p`, but it never reads OAuth
      credentials or the keychain, so it requires `ANTHROPIC_API_KEY` and
      abandons subscription billing. Without it, every dev agent inherits
      whatever sits in the host's `~/.claude` — hooks, plugins, MCP
      servers, auto memory, `CLAUDE.md` — which is neither reproducible
      nor safe for a build engine running unattended. A set
      `ANTHROPIC_API_KEY` also silently shadows subscription credentials
      in the non-bare path. Decide deliberately; it is not a default to
      drift into.

- [ ] **How does the gate name uncovered arms in the GateResult detail?**
      `AC-coverage-recorded` requires uncovered arms "named in detail for
      the dev agent", but `MOD-gates` only runs the target's command and
      observes its exit. Whether detail comes from captured stdout, a
      convention on output format, or an artifact the command writes is a
      DESIGN decision, not a product one — to be proposed in design. Same
      question for every gate whose failures must be actionable.
- [ ] **Mutation gate scope for kriya's own code.** Kriya's mutation
      command takes no directory argument — a whole-module run. Sutra
      measured whole-module at 88.74% efficacy over ~1678 mutants in
      **15h48m** and scoped its gate to eight domain-core packages to make
      it usable. Kriya likely needs the same narrowing. (Distinct from R2,
      which asks the cadence question for *targets*.)
- [ ] **What model access does the end-to-end proof assume?**
      `CON-no-secrets` covers credentials but not availability. A real
      driven build spends real tokens against a rate-limited API and is
      not offline-reproducible — a constraint on a success criterion that
      requires real agent runs.

## Change log

- 2026-08-21: Initial draft (Brent Hoover)
- 2026-08-21: Applied problem-reviewer findings — corrected constitution
  count (10, not 11), restated Scale as wall-clock per gate chain,
  reframed the concurrency checklist as provenance rather than open spec
  work, added the research/spike document path and R2, scoped the
  spec-change non-goal to behavioral changes, made the end-to-end
  criterion observable, replaced linkshort as the proof target (refused by
  `AC-intake-commands` — four modules, 16 missing commands), and added
  open questions for GateResult detail,
  model access, and roborev patching scope (Brent Hoover)
- 2026-08-21: Recorded the coverage-floor decision — every app gates on a
  floor including branch coverage; the policy lives in each target's stack
  command and kriya executes it verbatim (Brent Hoover)
- 2026-08-21: linkshort made buildable — stack flipped to Go 1.25 with all
  six required commands and an arch-go.yml; resolves the end-to-end proof
  target question (Brent Hoover)
- 2026-08-21: Resolved the test-double split (unit against a fake,
  integration against a real sutra), delivery shape (merged milestones),
  and landed the coverage-floor amendment in kriya's spec and scenarios
  (Brent Hoover)
- 2026-08-21: Agent invocation resolved — Claude Code CLI as a subprocess.
  A Python flip to reach the Agent SDK was considered and rejected; the
  stack stays Go. DESIGN is unblocked. The `--bare` billing/reproducibility
  tension is recorded as deferred (Brent Hoover)
