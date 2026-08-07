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
- 2026-08-06 — Missing listing operation: the doc-database scenario
  reads documents back "under SUT" (the project side), but the contract
  declared only POST on /projects/{projectId}/documents — no GET
  existed for the listing the scenario requires — fixed by adding
  listProjectDocuments — proposed home: a verifier check that every
  scenario read-back has a contract operation to serve it.
- 2026-08-06 — SQLite DDL (indexes, composite keys, on-delete) not
  derivable mechanically from entities — resolved by convention (derive
  1:1, decide the rest in code) — proposed home: deferred item 7,
  entities-as-schema.

## Cross-project blocks cannot round-trip through export

`blocks` relations may cross projects (REQ-issue-blocking allows it; only
`parent_of` is project-confined), but ProjectExport is single-project:
IssueRelation carries bare from/to uuids, and import validates both ends
resolve within the payload. A cross-project block therefore has no
representable foreign endpoint — export omits it (now normative on the
exportProject description) and a round-trip loses it. The asymmetry runs
deeper than the relation table: a relation-REMOVED event for a
cross-project block IS exported, because export scopes events by subject
and the subject is the in-project source issue. Import therefore accepts
a foreign `to` in that payload (rejecting it would make a valid export
unimportable) while requiring both endpoints for parent_of, which cannot
cross projects. So history crosses the boundary the live relation cannot
— another face of the same missing decision. avspec's data model
would need either an external-endpoint representation in ProjectExport or
a constraint confining `blocks` to one project. Surfaced by roborev
review 1805.

## assigned_at (pop FIFO position) is not exportable

The work-stack pops FIFO by `assigned_at`, an internal column stamped at
create/assign. The contract's Issue schema is strict
(additionalProperties: false) and carries no such field, so export
cannot represent it and import reconstructs it from `updated` — a
round-trip can therefore reorder an agent's pop sequence when several
assigned issues survive the trip. Fixing this needs a contract/spec
change (an explicit queue-position field on Issue). Surfaced by roborev
review 1807.

## The pinned coverage gate does not run against the real codebase

The stack pins `go run github.com/rillig/gobco@v1.3.4 ./...` with a 100%
condition-coverage bar (spike-validated on a toy module). Against the real
multi-package module: (a) `./...` instruments NOTHING — gobco does not
expand recursive package patterns, and the awk wrapper's missing-summary
guard is the only reason the gate would fail rather than silently pass;
(b) per-package runs panic on packages using the standard `export_test.go`
test-hook idiom (gobco's type resolution includes external _test packages
but omits in-package test files). Beyond tooling, a 100% condition bar
counts thousands of defensive `if err != nil` branches on in-memory SQLite
calls that cannot be made to fail without fault injection. The gate as
pinned needs a spec decision: a working tool invocation (per-package loop,
export_test.go accommodation) and an attainable bar, or a different
coverage measure. First measured 2026-08-06 after the verification
directory went green (136/136).

First per-package measurements (packages without export_test.go):
identity 8/34, projects 26/44, events 38/66, issues 58/212,
comments 0/68, threads 0/30. The zeros expose the structural mismatch:
gobco runs each package's OWN tests, but sutra's behavior coverage lives
in the acceptance package driving the system over HTTP — per-package
condition coverage cannot see it. The gate needs respecification against
this architecture, not just tool fixes.

Gremlins (the mutation gate, pinned 100/100) DOES run — first
measurement 2026-08-06: killed 454, lived 73, not covered 1030;
test efficacy 86.15%, mutator coverage 33.85%. The mutator-coverage
number has the same structural cause: coverage attribution is
per-package, so the acceptance suite exercising internal packages over
HTTP does not count toward their mutants. Both quality gates need a
spec decision on measurement architecture and attainable bars.

## No metadata-only document version history in the contract

listDocVersions returns full DocVersion records (content required by the
schema), but the web UI's version selector needs only ids and numbers.
The client cannot skip the content by decoding into a lighter struct —
json.Decoder.Decode buffers the whole top-level value first — so the UI
token-scans the response and stops at the content key, and the wire
order puts content last to make that possible. Client memory is bounded
by that scan, but the server still loads and ships every version's
content for data the page discards. A metadata-only listing variant would need a
contract addition (a new operation or a response-shape parameter).
Surfaced by roborev review 1873.

## No contract bound on JSON property names in verbatim values

Thread transcripts and event payloads are arbitrary JSON stored and
re-served verbatim, and the contract bounds neither their nesting nor
their property names. Duplicate properties make such a value ambiguous
(encoding/json keeps the last), so every mutating body is scanned and a
repeat at any depth rejects — applied at EVERY door, so anything the API
accepts survives export and re-import.

That scan needs one implementation-level bound the contract does not
provide: 1 MiB of property-name bytes live across all currently-open
objects (nesting depth reuses encoding/json's own 10000 limit, so the
scan never rejects what the decoder would accept). Real records carry a
handful of short names; only hand-built adversarial JSON reaches it.
It is nonetheless a cap below an unconstrained contract — the same class
as the SQLite value-length bound already logged. Surfaced by roborev
review 1902 and re-raised in 1904, 1908, 1910, 1916, 1920, and 1924.

RESOLVED 2026-08-07 by declaring it: the API description now states the
no-duplicate-properties rule, why it exists (verbatim storage would
outlive the ambiguity), and the 1 MiB bound on simultaneously-open
property names, with 400 bad-request for both. This follows the same
route as the canonical-lowercase identifier rule — where implementation
and contract disagreed, the contract now says what the implementation
does, rather than the implementation silently imposing an undeclared
limit. The NUMBER remains a product decision Brent can change; what is
no longer open is whether it is declared.

## AC-docweb-live needs a bounded projection the contract did not have

The document view's live-refresh signal polls for the current version
number every five seconds, but the only operation carrying it was
getDocument, whose DocumentView embeds the version's unbounded content.
Bounding the CLIENT's decode was not enough — the API still read and
marshaled the whole content per poll — so an implementable AC-docweb-live
requires a representation that carries no content.

Added getDocumentMeta (GET /documents/{documentId}/meta → DocumentMeta:
the bounded Document plus DocVersionMeta — id, document, number, author,
created). The first attempt returned only the version metadata, which
was not enough: the web view's SCOPE GUARD also resolves a document's
project through getDocument, so every guarded request — including each
poll — still forced the unbounded read. One projection carrying both the
document and the version metadata serves the guard and the poll in a
single request. This is also the shape the earlier "no metadata-only
document version history" entry asks for, now provided for the current
version only; the full history listing still ships content it does not
need. avspec's contract should
either declare bounded projections wherever a UI polls, or declare which
operations a live view may use. Surfaced by roborev review 1912.

Note the verification suite caught the omission mechanically: adding an
operation failed REQ-cli-parity ("every api operation has a command")
until the CLI's operations table carried it too. The parity scenario is
doing exactly what it was written to do.

## No declared scale bound: export planning holds one id per record

Export streams every record, but its PLAN holds an id list per group —
issues, documents, versions, reviews, threads, identities, labels — plus
the sets needed to filter relations and deduplicate threads. That is
O(records) of fixed-size ids, not O(content), and it is the shape review
1902 and review 1920 explicitly asked for ("retain only bounded IDs in
the plan and stream the records").

Review 1922 then asked for the next step down: iterate directly from
ordered SQL or stage the dependency metadata in temporary disk-backed
tables, so a project with enough records cannot exhaust memory before
the response starts. Both remedies are real, and both are architecture
decisions rather than local fixes — concurrent cursors held open across
one read transaction, or a temp-table staging layer — and the
cross-referencing sets (which relations are in scope, which threads have
already been emitted) do not disappear under either.

The underlying gap is that the contract declares no scale bound and no
pagination for any listing or export: with no stated ceiling on records
per project, no implementation can be shown to be bounded, only smaller.
Deciding this needs a spec answer — pagination in the contract, a
declared maximum project size, or an accepted memory profile stated in
terms of record count. Surfaced by roborev review 1922 (declined pending
that decision).
