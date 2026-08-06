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
- 2026-08-06 — SQLite DDL (indexes, composite keys, on-delete) not
  derivable mechanically from entities — resolved by convention (derive
  1:1, decide the rest in code) — proposed home: deferred item 7,
  entities-as-schema.
