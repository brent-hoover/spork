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

RESOLVED 2026-08-08 by replacing the tool. A fourth gobco defect
surfaced on re-examination: passing several packages in one invocation
panics on gobco's own assertion, `checking multiple packages doesn't
work yet`. So the pinned `./...` silently instruments nothing, the
obvious repair crashes, and the only working form — one package per
run — is exactly the form whose attribution does not fit this
architecture. There is no invocation of gobco that gates this module.

The gate is now `go test -coverpkg=./... ./...` with a 65% floor.
Go's own coverage attributes ACROSS packages, so the acceptance suite
counts toward the code it drives: review 0% -> 71.5%, threads 0% ->
81.5%, comments 0% -> 50.8%, api and docs (which gobco could not
instrument at all) 74.3% and 70.5%. Module total 69.9%. This trades
condition coverage for statement coverage — a real loss in strictness,
accepted because a condition gate that cannot execute measures
nothing, and because the mutation gate is the one that proves the
tests test. 65% is a REGRESSION FLOOR, not a quality claim.

The number should not be read as reassurance. Sampling the 1,830
uncovered statements found ordinary logic, not defensive plumbing —
an import loop over document versions, an export row loop, the
verdict-staleness check, duplicate-label detection. Path-based
exclusion cannot rescue the bar either: excluding all of cmd/sutra,
the most defensible exclusion available, moves the total 69.9% ->
70.7%. And neither gobco nor Go's coverage supports a source-level
ignore directive, so there is no way to annotate a branch as
deliberately unreachable. Raising this bar means writing tests.

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

## A scenario naming an interface does not bind execution to it

REQ-code-review opens with "humans read, discuss, and pass verdict in
the web UI", and its stale-feedback scenario reads `Given a review at
revision 1 open in a reviewer's browser`. The step definition behind
it posted straight to `/reviews/{id}/verdict` on the API. Same shape in
REQ-doc-review-server: `When a new version is saved` posted to
`/documents/{id}/versions`. Both scenarios passed for the entire build
while `reviewVerdict` and `saveDocVersion` — including the branch that
re-renders the review page with the API's conflict where the reviewer
will actually read it — never executed once.

The browser framing is prose. avspec's `test:` field points at a
scenario by name, and nothing in the scenario constrains which surface
the step drives; a step is free to satisfy browser language through the
API sitting behind it. That is the same failure mode as the coverage
gate above — a check that reads as coverage and is not — and it
survived 137 passing scenarios and 102 LLM review rounds, because
reviewers read diffs and the suite read green. Repairing the coverage
gate is what exposed it: the handlers sat at 0%.

Fixed 2026-08-08 by rewriting both step definitions to drive the web
handlers, and by making the browser precondition real (the page is
opened, and the revision the form carries is asserted on the rendered
page before the verdict is submitted). Proposed home: let an
acceptance criterion or scenario declare the surface it exercises, so
a step entering through a different door fails verification instead of
passing quietly.

## Issue commenting in the browser was never specified

The issue page carries a comment form (`POST /p/{key}/i/{num}/comment`)
with no requirement behind it — REQ-comments never mentions the web,
the browser, or a page. It surfaced at 0% coverage alongside the two
above, and it is a different problem: not an untested requirement but
an untethered implementation.

Brent's call on finding it: a spec miss, not unrequested scope —
commenting on an issue from its page is wanted behavior that the
interview simply never asked about. Recorded as AC-comment-in-browser
with a scenario that drives the form. Proposed home: interview
coverage. When a requirement (comments) and a surface (the web UI)
both exist independently, the interview should ask whether they meet.
sutra ended up with thirteen web routes and only four requirements
that mention the browser at all, so the gap was structural rather
than an oversight about this one form.

## Dead code survived because the unused-identifier exemption is transitive

Ten functions were unreachable when the build finished: cli.CoveredOperations,
comments.Get, comments.ListByAnchor, docs.Search, docs.list, docs.ListVersions,
events.SortByFeedOrder, events.BySubject, issues.List, review.List.

All ten are residue from the streaming campaign. Reviews 1805-1833, then 1918
and 1926, each replaced a materializing call with a cursor or an ids-only
projection: the new function was added, the call site was pointed at it, and
the old one stayed. That is not carelessness — the finding was always "this
materializes unbounded data", and switching the call site answers it
completely. Deleting the superseded function is a separate act that nothing
in the loop prompts.

Four mechanisms could have caught it. None did, each for a structural reason:

1. Go rejects unused imports and unused locals but never unused package-level
   functions. Deleting the ten immediately produced two unused-import errors,
   which demonstrates the asymmetry rather than contradicting it.
2. staticcheck's `unused` — on by default in golangci-lint — EXEMPTS exported
   identifiers, and nine of the ten are exported. Verified on a two-function
   probe module where nothing calls either function: the unexported one is
   reported, the exported one is not, and `unused.exported-is-used: false` is
   accepted but inert. Worse, the exemption is TRANSITIVE: docs.list is
   unexported and still went unreported, because its only caller was the
   exempt-and-dead docs.Search. One exported dead function shields every
   unexported function beneath it. The exemption exists so libraries do not
   flag their own public API, but these live under internal/ where nothing
   outside the module can call them — the tool cannot tell the difference.
3. Review reads diffs. A function that should have been deleted does not
   appear in a diff; its absence is invisible. 102 rounds, none flagged it.
4. The gate that would have shown all ten at 0% was the gobco gate, which was
   instrumenting nothing.

Fixed 2026-08-08: all ten deleted (no cascade — deadcode reports zero after),
and `golang.org/x/tools/cmd/deadcode@v0.48.0 -test ./...` pinned as a gate.
It does reachability from real entry points rather than staticcheck's
per-identifier rule, `-test` counts test binaries as entry points so
test-only helpers are not false positives, and it found all ten with ten
findings total — quiet enough to gate at zero tolerance. It exits 0 while
printing findings, so the gate tests for empty output.

## avspec's stack cannot pin a gate it did not anticipate

`Commands` is a strict model (`extra="forbid"`) over a fixed set: install,
test, lint, typecheck, arch, coverage, mutation. Adding `deadcode:` fails
`avspec verify`, so the new gate had to ride inside `lint` as a second
command. That works, but it hides a distinct check behind another one's
name — the thing this log keeps finding fault with elsewhere.

The deeper point is that the gate set is a closed vocabulary decided by
avspec, while which checks a project needs is a property of the project and
its language. Proposed home: an open map of named checks (`checks: {deadcode:
"..."}`) alongside the well-known ones, so a stack can declare a gate avspec
has never heard of and still have it named, run, and reported honestly.

## The label catalog listing had no acceptance criterion

`GET /labels` (listLabels) is declared in the contract and registered on
the mux, and the CLI carries a command for it because REQ-cli-parity
requires one per operation — but no acceptance criterion asked for it,
so nothing ever called it. It surfaced at 0% coverage with the three web
handlers, and it is the same shape as the issue-page comment form: an
operation that exists because building the surface implied it, not
because a requirement named it.

Treated the same way, per the ruling on that form: wanted behavior the
interview never asked about, not unrequested scope. A catalog of unique
label names is only useful if a client can read it — otherwise callers
mint near-duplicates of labels that already exist, which is exactly what
the uniqueness constraint is there to prevent. Recorded as
AC-label-catalog with a scenario that creates labels out of alphabetical
order so the assertion tests the catalog's sort rather than insertion
order.

Note what carried the operation this far with no requirement behind it:
REQ-cli-parity forced a CLI command for it, and the contract declared a
response schema for it, so two mechanical checks were satisfied by an
operation nothing wanted. Coverage was the only gate that could see the
difference between "declared and wired" and "actually exercised".

## An AC that enumerates a set, verified by a scenario that checks one member

The mutation gate's first surviving mutants — three of them, all
`CONDITIONALS_NEGATION` on the `guardWritable` check in
`resolveCommentAnchor` (`internal/api/comments.go` lines 53, 73, 93, one
per comment anchor: issue, doc version, review) — traced back to a single
under-specified scenario.

`AC-audit-mutations` reads:

> When an issue is created, changes status, is assigned, labeled,
> commented, or linked, an Event is recorded with actor, change kind, and
> timestamp.

Six mutation kinds. Its scenario verified one:

```gherkin
Scenario: every mutation is recorded
  Given issue SUT-1 exists
  When "claude" changes SUT-1 status to "in-progress"
  Then an event exists for SUT-1 with actor "claude", kind "issue.status-changed", and a timestamp
```

The scenario name promises the enumeration; the body samples it. Nothing
ever asserted that commenting emits an event at all, let alone on the
right subject — so negating those three guards, which makes
`resolveCommentAnchor` return `("", nil)` on the happy path, changed
observable behavior in a way no test could see. The comment is still
created and still appears in the issue's comment list, because that list
is queried by the comment's anchor column. Only the `comment.created`
event goes out with an empty subject, silently dropping the comment from
the issue's audit history — the exact thing AC-audit-mutations exists to
guarantee.

Fixed by turning the scenario into a `Scenario Outline` with a row per
enumerated kind, each asserting the event lands on **SUT-1's own
subject**. Comments get three rows, one per anchor, because each anchor
resolves its subject through separate code. Verified directly rather than
by waiting for the gate: each of the three mutants was applied by hand
and the suite failed on all three.

This is the same defect species as `And writing to "OTH" is rejected`
(below) and as the browser-framed scenarios satisfied through the API: a
step whose prose covers a set while its implementation covers one case.
Gherkin makes this easy to write and impossible to detect by reading —
the scenario reads as complete. Coverage could not see it (the lines all
ran); only mutation testing could.

**Still open, same species, not yet fixed:** `AC-project-archive` says
archived projects are "read-only", and its scenario discharges that with
one step, `And writing to "OTH" is rejected`, implemented as a single
POST that creates an issue. There are sixteen `guardWritable` call sites
across ten handlers; that step exercises one. The other nine are
unverified. It has not produced surviving mutants because in most
handlers the negated guard also breaks the happy path loudly — the
mutants die for a reason unrelated to the archive behavior, which is its
own warning about reading a kill as evidence.
