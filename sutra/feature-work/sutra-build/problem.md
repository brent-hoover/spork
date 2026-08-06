---
title: Sutra Build — Problem Statement
type: problem
status: draft
owner: Brent Hoover
created: 2026-08-06
updated: 2026-08-06
---

# Sutra Build — Problem Statement

## Context

Sutra is an issue tracker designed for AI-agent workflows: durable issue
hierarchies, review-gated closes, an event feed with watermarks, and
strictly idempotent mutations. It exists today only as a *ready* avspec —
`sutra/avspec.yaml` (requirements and acceptance criteria),
`sutra/contracts/sutra.openapi.yaml` (the pinned API contract), and
`sutra/verification/*.feature` (Gherkin scenarios mapped from every
acceptance criterion). The spec reached a clean whole-branch roborev pass
and `avspec verify` reports it ready. No line of Go exists.

The specified surface is large: 23 requirements, ~96 acceptance criteria,
23 feature files, 53 API operations across a ~2,600-line contract, and 10
modules with declared import boundaries.

Kriya — the build engine specified alongside sutra — consumes sutra's API
for all ticket tracking, completion detection, and review consumption. Its
spec's recovery protocols assume sutra's contract guarantees (keyed
replay, fencing revisions, cascade events) hold exactly as written.

## Problem

There is no running sutra. Nothing serves the pinned OpenAPI contract, no
store enforces the close gate or the reopen cascade, no event feed exists
to drain, and none of the verification scenarios execute against real
software. Until sutra runs:

- Kriya implementation cannot start against a real tracker, and the
  contract guarantees its crash-recovery design depends on remain
  unexercised by any implementation.
- The operator has no tracker for agent-driven work at all — no
  hierarchy, no review-gated closes, no watermarked feeds.
- The verification scenarios — the executable definition of the spec —
  have never run, so the spec's claim of internal consistency has never
  been tested against executable behavior.

## Complexity drivers

- **Scale**: N/A — single-operator, single-machine tool; data volume is
  bounded by local projects and grows linearly with activity.
- **Concurrency**: inherent. Multiple agents and the operator mutate the
  same issue graph concurrently. The contract mandates Idempotency-Key
  replay returning original responses, `subtree_revision` /
  `latest_verdict_event` / `expected_base_commit` fences on close,
  consume, and resubmit paths, and single-transaction reopen cascades —
  races are specified behavior, not edge cases.
- **Failure modes**: a close that hides active descendant work, a
  double-consumed review approval, or a replayed mutation that applies
  twice silently corrupts tracker state — and kriya's completion
  detection then reports builds done that are not. Every crash window has
  a specified recovery; an implementation that misses one breaks a
  downstream consumer that trusts it.
- **Cross-cutting policies**: no secrets in source or configuration
  (CON-no-secrets); the data plane itself is freeform operator content.
  There is no authentication boundary: `actor` is a client-supplied identity id
  validated only for existence (AC-identity-referenced), so every caller
  is trusted. Auditability is load-bearing: every mutation the audit
  requirements cover emits events carrying a server-generated operation
  id — the contract's closed kind list (issue.*, comment.created, doc.*,
  thread.*, review.*, project.*) is the authority; identity, label, and
  template CRUD have no event kinds — and the watermarked feed is the
  observability surface consumers reconcile against.

## Constraints

- The implementation must serve `sutra/contracts/sutra.openapi.yaml` as
  pinned — the contract is the wire truth. A few schemas (`Issue`,
  `IssueRead`, and the complete/status-transition request bodies) are
  explicitly closed with `additionalProperties: false` and must reject
  unknown fields; the rest are ordinary open schemas.
- All `sutra/verification/*.feature` scenarios must pass, wired through
  godog — they are the acceptance tests, not documentation.
- Stack is fixed by the spec: Go 1.25 (bumped from the spec's original
  1.23 pin — out of security support, and current dependency floors
  require 1.25; recorded in spec-gaps.md), `go` toolchain, godog, SQLite
  (FTS5 for search), net/http + htmx.
- The gate chain is pinned: golangci-lint v2.1.6 via `go run`, `go vet`,
  arch-go v1.7.0 against `sutra/arch-go.yml` module boundaries, gobco
  v1.3.4 requiring 100% condition coverage, go-gremlins v0.5.0 at 100%
  efficacy/mutant-coverage thresholds. The arch and coverage commands are
  spike-validated; the mutation command is documented behavior only (see
  Open questions).
- Constitution: test-first (no behavior before its failing AC-mapped
  test), one-way dependency flow, no secrets in source.
- Process: work in the `feat/sutra-build` worktree; every commit gets a
  roborev review; a clean whole-branch pass gates merge.

## Non-goals

- Kriya implementation (separate feature; only sutra's contract must be
  ready for it).
- Prepared reviews (Crucible-style review preparation) — deferred in
  `avspec/feature-work/avspec-0.3-restart/deferred.md`.
- Spec changes — the avspec is ready; if implementation exposes a spec
  defect, that is a spec fix with its own review, not silent drift. This
  includes the known contract warts already catalogued in `deferred.md`
  (asymmetric 400 enumeration, mixed nullability on `Thread.session` and
  `Document.current_version`, the misplaced `searchThreads` description
  sentence) — implement what is pinned; do not quietly "fix" them.
- Multi-user auth, hosted deployment, or scale beyond single-operator.

## Success criteria

- `go test ./...` green, with every scenario in `sutra/verification/`
  executing and passing under godog.
- The full pinned gate chain passes: lint, vet, arch-go, gobco 100%
  condition coverage, gremlins 100% thresholds.
- `avspec verify ../sutra` (run from `avspec/`) still prints
  `status=ready errors=0 todos=0 ok=True`.
- Whole-branch roborev pass, then merge to develop.

## Open questions

All resolved 2026-08-06 with Brent:

- [x] Contract conformance → **test-time validation**: the godog harness
      validates every HTTP response against the OpenAPI document via a
      validator library (kin-openapi — approved as a test-only
      dependency). Lives inside the existing `go test` gate; no new gate
      command. Response validation alone cannot prove the server rejects
      unknown fields on the closed schemas (`Issue`, `IssueRead`, the
      status-transition bodies) — that is proven behaviorally: dedicated
      steps submit unknown fields and assert rejection without mutation,
      alongside the closed-schema scenarios already in verification/.
- [x] Browser-bound scenarios → **handler/HTML-level assertions**: godog
      asserts the server side of each behavior (the rejected move returns
      the error htmx uses to snap the card back; the doc-review server
      emits the refresh event). The browser half is htmx's documented
      contract; no browser driver.
- [x] Mutation gate runtime → **measure after module 1**: run the pinned
      gremlins command as soon as the first small module is implemented;
      if runtime or attainability is unreasonable, raise the spec-change
      conversation then, with data.
- [x] Delivery shape → **one branch, sliced commits**: stay on
      `feat/sutra-build`, implement modules as ordered commits in
      dependency order, roborev per commit, one merge when the whole
      surface is green.

## Change log

- 2026-08-06: Initial draft (Brent Hoover)
- 2026-08-06: Applied problem-reviewer findings — corrected
  additionalProperties claim, sized the surface, restated auth as fact,
  narrowed spike-validation claim, added conformance/browser/mutation/
  slicing open questions (Brent Hoover)
- 2026-08-06: Resolved all four open questions with Brent — test-time
  conformance validation (kin-openapi approved, test-only),
  handler-level web scenarios, gremlins measured after module 1,
  one-branch sliced delivery (Brent Hoover)
