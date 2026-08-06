# Spec gaps — sutra-build

Running log of every point during implementation where the ready avspec
did not hold the information needed to proceed. This build doubles as the
experiment testing avspec's core claim (`ready` = buildable without
further prompting); each entry here is evidence for avspec 0.4 or a
requirement on kriya's context assembly.

Format: date — what was needed — where the answer came from instead —
proposed home (avspec field / kriya context / convention).

## Entries

- 2026-08-06 — Contract conformance mechanism (how to prove served
  responses match the pinned OpenAPI doc) — resolved in conversation:
  test-time validation via kin-openapi — proposed home: stack block or a
  constitution `check:` (deferred item 1).
- 2026-08-06 — Execution strategy for browser-bound scenarios (kanban
  snap-back, doc-review refresh) with no browser driver in the stack —
  resolved in conversation: handler/HTML-level assertions — proposed
  home: a per-scenario execution-level convention avspec could declare.
- 2026-08-06 — Composition root: arch-go requires every package covered
  by a rule, but avspec modules only cover the declared surface —
  cmd/sutra (main, wiring api/cli/web) has no home in the spec's module
  list — resolved by an explicitly-commented extra rule in arch-go.yml —
  proposed home: avspec could declare an app composition module, or the
  arch-config generator (deferred item 8) could emit the rule.
- 2026-08-06 — SQLite driver unpinned: `store: sqlite` names the engine
  but not the Go driver — resolved with Brent: modernc.org/sqlite (pure
  Go, FTS5, single-binary) — proposed home: stack block could pin driver
  alongside engine.
- 2026-08-06 — Contract invalid per OpenAPI 3.0: `Error.conflicts`
  declared `type: array` with no `items` — ~100 LLM review rounds missed
  it; the FIRST mechanical validation (kin-openapi doc load in the
  harness) caught it — fixed minimally (`items: {}`) — proposed home:
  avspec verify should validate referenced OpenAPI documents (motivates
  deferred item 1's check attachments).
- 2026-08-06 — Stale toolchain pin: the spec's Go 1.23 was already out
  of security support at build time, and current dependency versions
  floor at go 1.25 — resolved by bumping both specs to 1.25 — proposed
  home: avspec could flag EOL language pins at verify time, or the pin
  belongs at major-version altitude.
- 2026-08-06 — Cross-project hierarchy unspecified: the spec never says
  whether parent_of may cross projects, but the reopen cascade and
  subtree_revision walk ancestor chains — a cross-project chain lets a
  live descendant mutate an archived ancestor project past its write
  guard (found by roborev 1728) — resolved: parent_of confined to one
  project (AC-project-scoping intent); blocks may cross with both-sides
  guards — proposed home: an explicit AC on relation project scope.
- 2026-08-06 — Unproducible constant in a scenario: the approved-close
  scenario pinned a literal 40-hex commit sha, but no real repository
  can mint a chosen sha, so the step could never execute against real
  git — amended to reference the pinned commit abstractly (intent
  preserved: approval covers the pin) — proposed home: scenario-writing
  guidance to keep externally-minted identifiers symbolic.
- 2026-08-06 — Unachievable atomicity in the contract: the submission
  fences claimed validation "atomically against the CURRENT ref while
  the review is created", but the repository is an external store no
  tracker transaction can lock — external pushes race any
  implementation, and three review rounds (1730/1735/1741/1744)
  ping-ponged between fence-tightness and lock-health before the root
  cause surfaced — amended to observed-at-submission semantics (the
  fence rejects stale caller knowledge; the pinned base stays
  authoritative) — proposed home: spec guidance that cross-store
  atomicity claims name which store's clock they linearize on.
- 2026-08-06 — SQLite DDL (indexes, composite keys, on-delete) not
  derivable mechanically from entities — resolved by convention (derive
  1:1, decide the rest in code) — proposed home: deferred item 7,
  entities-as-schema.
