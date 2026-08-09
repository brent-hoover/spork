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

## Relinking a document was implemented but never specified

The fourth surviving mutant, `CONDITIONALS_NEGATION` at
`internal/api/docs.go:398:41`, negates the departure guard in
`linkDocumentToIssue`:

```go
if formerIssue != nil && *formerIssue != req.Issue {
    events.Emit(tx, "doc.unlinked", *formerIssue, operation, req.Actor, &payload)
}
```

Negated, moving a document from SUT-1 to SUT-2 emits no `doc.unlinked` on
SUT-1 — the document disappears from the losing issue's history with no
record. Nothing failed, because no scenario ever relinks a document.

`AC-doc-link` says only "An existing document can be tied to an issue and
untied after creation." Tie and untie: two states, and the scenario walks
both. Relink is the third — a tie onto an issue that already has one — and
the interview never asked about it. The implementation nonetheless treats a
relink as a move and emits the departure under the *same* operation id as
the arrival, with a code comment asserting the intent ("a document never
silently vanishes from an issue's history"). That intent lived only in the
comment.

Same ruling as the issue-page comment form and `GET /labels`: behavior that
was wanted but missed at interview, so it earns an acceptance criterion
rather than deletion. Recorded as AC-doc-relink with a scenario that moves a
document between two issues and asserts both events land on the right
subjects. Mutant applied by hand and confirmed dead.

Worth separating from the AC-audit-mutations gap above, because the failure
mode differs. There the AC named the behavior and the scenario undersampled
it. Here no AC named it at all — the behavior existed only as code plus a
comment, and a comment is not a gate. Mutation testing is the only check in
the chain that can distinguish "deliberate branch nothing asked for" from
"deliberate branch nothing tests".

## A 311-line hardening file with no requirement behind it

Twelve mutants survived in `internal/api/dupkeys.go`, the streaming lexer
that rejects a request body repeating a property name. Grepping `avspec.yaml`
for the behaviour returns nothing: no requirement, no acceptance criterion,
no scenario mentions duplicate JSON keys. The whole file — a recursive
scanner, a 10,000-level depth cap, a 1 MiB property-name budget, a
cancellation interval — exists because of code review, and its own comments
cite the review numbers ("review 1900", "review 1902", "review 1906").

The intent was not undocumented. The OpenAPI contract states it in prose:

> REQUEST BODIES: a JSON object must not repeat a property name at any
> depth. […] thread transcripts and event payloads are stored VERBATIM and
> re-served, so the ambiguity would outlive the request.

So the contract knew, the code knew, and the spec did not. Recorded as
`AC-no-ambiguous-bodies` under REQ-import-export — that is where the
rationale points, since the guarantee is what makes an accepted transcript
re-importable — with a three-row outline importing transcripts that repeat a
property at the top level, inside a nested object, and inside an object in
an array.

### The interesting part: good tests that still cannot see a boundary

Four of the twelve survivors are the same species as the gaps above. The
other eight are new, and more uncomfortable, because the tests covering them
were *already thoughtful*. `TestScanDuplicateKeysBoundsKeyMemory` builds a
4,000-key object; `TestScanDuplicateKeysStreamsValues` measures allocation
against a ceiling to prove values stream. Neither is lazy. Both still let
their boundary mutants live:

| Test | Budget | Payload | Distance from the line |
|---|---|---|---|
| BoundsKeyMemory | 1 MiB | 4,000 × 600 B = 2.4 MiB | 2.4× over |
| HonoursCancellation | every 64 KiB | 8 MiB, cancelled *before* the scan | never reaches the check |

A test that overshoots a threshold proves a breach is caught. It says
nothing about *where* the threshold is, so `>` and `>=` are
indistinguishable to it — and so is a counter running the wrong way. The
depth cap had no test at all, in either its object or its array arm.

Replaced with tests that straddle each bound from both sides: exactly
`maxScanDepth` accepted and one deeper rejected, for objects and for arrays;
a name whose raw form exactly fills the budget accepted and one byte more
rejected; a held name pushing a fitting name over; and a white-box check
that the disconnect test falls on byte 65,536 and not 65,535. Eleven of the
twelve mutants confirmed dead by hand.

This is the clearest evidence in the build for what the mutation gate buys
over the coverage gate. Every one of these lines was already covered. Line
coverage cannot express "the test is on the wrong side of the boundary".

### The twelfth mutant: a guard that cannot be killed because it is dead

`dupkeys.go:193` checks the property-name budget again after a name is
decoded:

```go
if l.keyBytes += len(key); l.keyBytes > maxKeyMemory {
    return fmt.Errorf("property names exceed the %d-byte scan budget", maxKeyMemory)
}
```

`str()` already enforced the same budget as the name's bytes arrived, and it
counts RAW bytes — both quotes and every escape included — which are never
fewer than the decoded name's. So any name that would breach the budget here
was rejected there. The branch is unreachable.

Two independent confirmations: the coverage profile records
`dupkeys.go:193.56,195.4` with an execution count of **0**, even under the
test that deliberately overshoots the budget 2.4×; and the mutant survives
every test in the package, by construction.

The fix is to delete the check and keep the accumulation. That edit was
**blocked by the tooling's safety classifier**, which reads the removal of a
bounds check as weakening a limit. It is left in place, and the gate will
keep reporting it. Flagged for the operator rather than forced.

It is also a finding about the gate itself, and belongs with the mutation
gate respecification: **a 100%-efficacy threshold is unsatisfiable over
provably-dead defensive code.** There is no test that can kill such a
mutant. The only ways out are to delete the code or to let the gate carry an
explicit, justified exclusion. That is an argument for the gate — it found
genuinely dead code that `deadcode` cannot see, because the function is
reachable and only the branch is not — but it means "any surviving mutant
fails the gate" cannot be the literal rule.

## Decisions no scenario can observe

Thirteen more survivors, in five clusters, share a property none of the earlier
gaps had: **both branches produce identical output.** They are not
behaviours a scenario could assert, because from outside the API there is
nothing to see.

`export.go:266` and `export.go:587` guard the same guarantee: an export
abandoned by its reader stops early. `write()` records the first failed
write into a single shared flag; `eachSubmission` returns the moment its
callback reports trouble. Ten `failed()` checkpoints read that flag. Negate
either mutant and the export still emits exactly the bytes it managed to
emit — it just keeps scanning rows for a client that has gone, holding a
read transaction and pinning the WAL. The intent is stated only in a code
comment citing review 1889. `avspec.yaml` and the contract say nothing about
client disconnect.

`idempotency.go:91` routes a request body: buffered in RAM below
largeBodyThreshold, spooled to an unlinked temp file above it *or when the
length is unknown*. All four mutants on that line — both boundaries, both
negations — leave the response byte-identical. They only change whether a
4 MiB body lands in memory or on disk, and whether a chunked upload
declaring no length is treated as small.

`idempotency.go:224` decides the same question one level up: which requests
queue for the single large-body slot. Four more mutants, same invisibility.
Widen the test and a 4 MiB body serializes behind every earlier large upload
for no reason; narrow it and two gigabyte uploads buffer at once — the exact
thing the slot exists to prevent. The response is identical in every case.

`idempotency.go:133` is the smallest of them: `close()` releases the spool
file only if one was opened. Negate it and every spooled body leaks its
descriptor *and* its disk bytes, because the temp file is unlinked at
creation and the descriptor is the only thing holding it. Nothing fails
loudly — `(*os.File).Close()` on a nil receiver returns `ErrInvalid` rather
than panicking — so the leak is silent, and the response is, again,
byte-identical.

`idempotency.go:282` is the one member of this cluster whose consequence is
observable — eventually. It draws the line between a rejection the client
caused, which spends the (operation, key) pair, and a failure the server
had, which leaves it fresh. Both write the same status to the same response;
the difference only surfaces on the retry, which no scenario makes. Move the
boundary by one and a transient 500 becomes permanent: every retry replays
the error instead of running the mutation.

Covered with white-box unit tests that assert the mechanism directly: a
ResponseWriter whose client hangs up mid-stream, a submission walk counting
its callbacks, a routing table straddling the threshold with the
unknown-length case pinned to the spooling side, a handler that reads slot
occupancy from inside the request it is serving, and a seek on the spool
file after close. All thirteen mutants confirmed dead by hand-applied
mutation, plus two neighbours.

### What this says about where acceptance criteria stop

The earlier gaps in this document all resolved the same way: behaviour was
wanted, the interview missed it, so it earned an acceptance criterion. That
ruling does not reach these. An AC states something a reader of the spec
could check; "the export stops scanning when nobody is listening" has no
observable form at the API boundary, and writing a Gherkin scenario for it
would mean asserting on a timing artefact.

So the line drawn here is: **observable guarantees earn acceptance criteria;
resource and robustness decisions earn unit tests.** The duplicate-key
scan's ambiguity rejection is observable — a 400 naming the property — and
got `AC-no-ambiguous-bodies`. Its 1 MiB budget, its depth cap, the spool
threshold, the admission slot, the descriptor release, and the disconnect
abort are not, and got tests instead.

That line is a proposal, not a settled rule, and it bears directly on the
mutation gate's threshold. A 100%-efficacy gate demands a test for every
branch, including these; it does not demand an AC for every branch. The two
gates measure different things, and the spec is the weaker instrument here —
which is worth saying plainly in an experiment whose premise is that a ready
avspec is buildable.

## A fourth species: mutants no test could ever kill

Three survivors in `import.go` turned out to be **equivalent mutants** — the
mutated program computes exactly what the original does, so no test
distinguishes them and none ever will. They are not gaps in the suite; they
are artefacts of how the code happened to be written.

`import.go:239` guarded a decode with a length check the decode already
subsumed: `json.Unmarshal` fails on empty input, landing in the very branch
the length check would have chosen. `import.go:411` picked a running maximum
with `>`, where `>=` picks the same maximum. `import.go:415` gated the
sequence seed on `maxNumber > 0`, where `>= 0` differs only for an
issue-less import — and seeding 0 leaves the next create at 1, exactly as
seeding nothing does.

All three were removed rather than excluded: the guard is gone, the maximum
now goes through the `max` builtin, and the seed is gated on `!= 0`. The
mutation gate does not need an allowlist for code that no longer contains
the site. This is worth separating from `dupkeys.go:193`, which is also
unkillable but for a different reason — that guard is *dead*, reachable by
no input at all, rather than equivalent to its own mutant.

## The export the round-trip scenario never exported

Four survivors clustered in `import.go` shared one cause, and it is the
sharpest instance yet of the first species in this document — **an AC that
names a set, discharged by a scenario that samples one member.**
`AC-import-round-trip` says the imported project "matches the original".
The original, as seeded, contained no live relation (only a deleted one)
and no complete issue. So import's relation validation and its
completeness invariant were exercised in exactly one direction: the
malformed payloads that must be rejected. Nothing proved a *legitimate*
relation or a legitimately closed issue survives the trip.

That let three guards flip undetected. `import.go:508` rejects a
self-relation — negated, it rejects every relation between distinct issues
instead, and no export carried one. `import.go:543` skips non-complete
issues before the descendant walk — negated, it checks the non-complete
ones, which the only complete-issue scenario in the suite also rejects, for
a different reason, so the scenario stayed green. `import.go:586` builds the
close-used index — negated, it indexes the reviews that were *not* spent,
and with no closed issue in the export nothing consulted it.

Fixed by seeding what the AC already claimed: a live `parent_of` relation
and a review-gated closed issue. All three mutants then die inside the
round-trip scenario itself.

## Two guarantees the interview missed, both observable

`import.go:313` and the number-sequence seed are the second species again —
implemented behaviour with no acceptance criterion — and both are visible at
the API boundary, so both earned one.

`assigned_at` is FIFO queue state the contract never carries, so import
reconstructs each assignee's position from the issue's `updated` stamp.
Negate the guard and assigned issues import with a NULL position while
unassigned ones gain a spurious stamp; the queue silently falls back to
issue-number order. Every record round-trips intact and the import reports
success. Now `AC-import-queue-order`, with a scenario that assigns issues
against their number order so the two orderings disagree.

Display numbers are minted from a per-project sequence the export does not
carry either. Without seeding it past the highest imported number, the next
create mints 1 again and collides with an imported issue on
`(project, number)` — a 500 on the first issue created after any import.
Now `AC-import-number-sequence`.

## The two anchors the round-trip only ever exercised one of

Threads and comments both carry an exclusive choice of anchor, and the
seeded export picked the same side of each: the thread was project-anchored,
the comment issue-anchored. So half of each `oneOf` reached import's
validation only through the malformed payloads that get rejected — which
proves nothing about the legitimate case.

`import.go:672` rejects a thread with neither anchor. Negate its second
operand and an ISSUE-anchored thread reads as anchorless; every import
carrying one fails, and none did. `import.go:675` guards the
wrong-project check — negated, it dereferences a nil project anchor, which
only an issue-anchored thread has. One seeded issue-anchored thread kills
both.

The comment side is the same shape with an extra wrinkle. `import.go:646`
was written `c.Review != nil && c.ReviewRevision != nil`, but the pass ten
lines above already rejects the two apart, so the second operand can never
be false when the first is true. That is the fourth species again — a
condition no input can falsify on its own — and the fix is again to remove
the site, not to exclude it: the guard is now `c.ReviewRevision != nil`
alone. What remained needed a review-anchored comment to reach at all, and
the revision range check behind it needed one both valid and invalid. Now
`AC-import-comment-revision`: the API pins a review comment to the revision
it was written against, and an import that does not check the same thing can
seat a comment on work that never existed.

## A validator that walks in order, and a spec that never says so

`validateImportShapes` checks every identifier and timestamp in the payload,
record type by record type, each guard written the same way:

```go
if apiErr := uuidOf("thread", t.ID); apiErr != nil {
    return apiErr
}
```

Negate any one of those 23 conditions and it returns `nil` — "shapes fine" —
the instant that check *passes*, silently skipping every record type after
it. One structural fact, 23 mutants: a guard that stops early is
indistinguishable from one that passes, and the only way to tell them apart
is to plant a fault at the far end of the walk. `AC-import-thread-transcript`
and its siblings each corrupt one record and assert rejection, which the
first guard alone satisfies. Nothing in the spec said the walk finishes.

Now `AC-import-malformed-anywhere`, an outline that corrupts the payload's
extremes on purpose: its first record, its last, a field it was free to omit,
and an identifier buried inside an event's snapshot. Twenty-one of the 23
died to it.

The two that did not were a different failure: the export carried no such
record at all. `p.Labels` was empty, so the label guard's mutant never
executed — and `AC-export-full`'s completeness check only asserts the literal
`"labels"` key is present, which an empty array satisfies. The spec named
labels and the scenario never checked one existed. The same hole covered
`archived_at` (no export was ever taken of an archived project), the `blocks`
half of the relation-kind check, and the `parent_of` half of the kind carried
inside a relation-removed snapshot. Four holes, one shape: **an
export-completeness check that counts keys cannot see an empty collection.**

The archived case earns its own guarantee rather than a tamper row alone:
archiving is what makes a project read-only, and it is a nullable field a
round trip can silently drop while every record still comes back and the
import reports success. Now `AC-import-archived-round-trip`.

## The cycle the API can never build, and import can

`findCycle` guards three graphs at import — parent_of, blocks, and comment
reply threads — under a comment reading "cycles in either graph are
unreachable through the API". That is exactly why they matter: the API
attaches one edge at a time and refuses the one that closes the loop, so
import, which writes the whole graph at once, is the only door a cycle can
come through. A cycle in the parent_of tree makes every ancestor walk below
it non-terminating — the reopen cascade, the subtree revision, the
completion check. No acceptance criterion said so, and the DFS was reachable
only through payloads that had no cycle in them: negate its unvisited-node
guard and it detects nothing, and every scenario still passed. Now
`AC-import-relation-cycle`.

The other survivor in that region is the cross-project asymmetry already
logged above, seen from the test side. Only `blocks` may cross a project
boundary; the live edge cannot round-trip, but its removal event does,
because export scopes events by subject and the subject is the in-project
source. So import carries a deliberate exception — a relation-removed
snapshot whose destination the payload does not carry is legal for a blocks
removal and only for one — and the seeded export contained no such removal,
so the exception was never taken. Now `AC-import-foreign-block-removal`,
discharged by the round trip with a crossing block seeded and removed.

Seeding a second project also broke the collision scenario's "no records
were partially written", which asserted the source still listed exactly one
project. It was reading a constant, not a consequence: it now compares
against the count taken before the rejected import.

The fourth species surfaced once more in the review pass: the human-verdict
check was written `humanSet != nil && !humanSet[actual.Actor]`, but the set
is built unconditionally by the function's one caller. No input can make the
first operand false, so the mutant that negates it is equivalent by
construction — removed, not excluded.

## A set of two, sampled once — and two sentinels for a case with no input

Six mutants clustered in the review half of import's validation, and they
divided into the two species that keep recurring.

`latestVerdictEventFor` skips every event that is not a verdict:
`e.Kind != "review.approved" && e.Kind != "review.changes-requested"`. A
review has two verdicts. Every export the suite had ever built carried only
approvals, so the walk never had to recognise the other kind — it could drop
`changes-requested` entirely and no round trip would notice. That is species
one again, an AC that names a set discharged by a scenario that samples one
member, and the fix is the same: seed a standing changes-requested verdict
in the rich project so both operands are load-bearing.

`AC-export-full` was the AC that let it through. It said the export captures
"reviews" and the scenario's check asks whether the record type is present —
which one approval satisfies as well as ten. The same blindness had already
produced four holes earlier in this document, so the statement now says the
thing the check cannot: where a record type has kinds or states, the export
carries more than one of each.

`verdictPrecedesResubmission` was the other kind. It scanned for two
indices, seeded `-1`, and returned `verdictIndex >= 0 && resubmitIndex >
verdictIndex`. But the sole caller has already found the verdict event in
the very list it passes, so `verdictIndex >= 0` is unfalsifiable — four
mutants (two boundary, two invert-negatives, on the sentinels and the
comparison) sitting on a not-found case no input can produce. The question
the function actually answers is "does a resubmission appear after the
verdict", which one pass with a boolean answers without inventing an absent
value to compare against. Rewritten that way, the sentinels are gone and
every mutant on the remaining conditions dies.

Species four says an equivalent mutant is a defect in the code, not in the
gate. This is the second time the defect was the same one: a comparison
written against a state the caller has already excluded.

## Two doors for a collision, and only one of them ever opened

Import rejects a colliding payload at two separate points: a UUID pass that
probes thirteen tables for ids the payload already claims, and a uniqueness
preflight that probes the project key, every identity handle, and every label
name. `AC-import-collision` is discharged by re-importing an export into the
server it came from — same records, same ids — which trips the first door and
returns before the second one runs. Every guard in the uniqueness preflight,
and the 409 it raises, was unreachable by any scenario.

The case the spec never named is two servers that independently minted a
project with the same key. Nothing about their UUIDs collides, so only the
second door can catch them; without it the import fails mid-insert as a
constraint error instead of naming the records that already hold each key.
Now `AC-import-unique-violation`, discharged by seeding a target with the
export's project key, one of its handles, and its label name — all minted on
the target, so all three probes must run and all three holders must be named.
This is species six again: three early-return guards in a linear walk, killed
by making the collision reachable at all rather than by planting it at the far
end.

`chunkIDs` gave the fourth species one more turn. It batched the id probe with
`for len(ids) > size` followed by `if len(ids) > 0`, and those two comparisons
disagree only when `len(ids)` is an exact multiple of `size` — where they
produce identical output. No input can tell `>` from `>=` there. Rewritten to
walk a cursor, it has one comparison instead of two, and that one is
answerable: at an exact multiple an off-by-one appends an empty trailing chunk,
and an empty chunk builds `IN (?` + `strings.Repeat(",?", -1)` + `)`, which
panics. Batching is a resource decision, so by the ruling above it earns a unit
test rather than an acceptance criterion — `TestChunkIDs`, whose exact-multiple
case is the one real payloads (never a multiple of 500 ids) cannot reach.

## The check that only owns the case nothing else can see

`walkDocumentExport` requires both halves of a documents element and reports
`documents element requires document and versions` when one is missing. The
obvious scenario — omit the versions array — does not exercise it: import's
later pass already rejects that with `document %s carries no versions`, naming
the document. Omit the document instead and the versions fall through to
`doc versions name unknown document %s`. Both single-key faults are caught
downstream by better messages, and a first attempt at killing the guard
SURVIVED for exactly that reason.

The one input the guard uniquely owns is an element carrying neither half.
Nothing downstream can object to it: no document is emitted, no version is,
and every later check reads the payload as one document fewer rather than as a
fault. That case has to be constructed by ADDING an empty element beside the
real one — emptying the only element strands the review anchored to its
version, and the dangling reference does the rejecting instead.

`AC-import-document-element` now names both flaws as one outline. The lesson
generalises past this guard: when a check is shadowed by a downstream one, the
scenario that discharges it has to be built from the input the downstream check
cannot see, or the AC is discharged by a different mechanism than the one it
describes — and the gate is the only thing that notices.

## A contract rule enforced at one door, and refusals that all look alike

`isUUID` is the shape check every API door runs on an identifier arriving in a
request body — actor, assignee, review, review verdict event. Its comment said
it accepts "the canonical 8-4-4-4-12 hex form", and it accepted uppercase hex
too. The contract does not: `contracts/sutra.openapi.yaml` states that every
`format: uuid` value is the CANONICAL LOWERCASE form (RFC 9562 §4), because
identifiers are compared as text at every layer and SQLite's comparison is
case-sensitive, so two casings of one uuid would name one entity while behaving
as two.

Import enforced the rule; nothing else did. `canonicalUUIDv7` called `isUUID`,
then case-folded the variant nibble and re-checked `strings.ToLower(id) == id` —
accept-then-reject, the same rule in two places. Every other door let uppercase
through the shape check and into the store, where the lookup missed, and the
server answered that an id which plainly exists names no identity.

That answer is why the laxity survived: the two refusals are both `400
bad-request`, so no scenario asserting merely "rejected" can tell them apart.
The mutation gate found it from the other side — `c >= 'A'` and `c <= 'F'` both
LIVED, at boundary and negation, because no test ever fed a character near
those bounds. Per the equivalent-mutant ruling the fix is to REMOVE the site:
`isUUID` is now lowercase-only, and `canonicalUUIDv7` drops both the fold and
the ToLower re-check, which have no input left that could tell them from their
absence.

Two things follow. The observable half — an existing id in uppercase is refused
for its FORM, not reported as unknown — is a guarantee, so it earned
`AC-identity-canonical-casing` and a scenario at the assign door. The character
rule itself is not reachable from any scenario: the acceptance suite only ever
sends ids the server minted, all canonical, so no input can reach the far side
of any of those four comparisons. That half earned unit tests (`TestIsUUID`,
`TestCanonicalUUIDv7`) pinning each boundary and its neighbour.

The generalisation: a rule stated in contract prose but enforced at one door is
invisible whenever the un-enforced doors fail for some other reason anyway. The
spec named the rule and named the door that keeps it; it never said the rule
holds at EVERY door, and the checks at the others were free to be laxer than
the contract without a single scenario noticing.

## A path parameter nobody said had to mean anything

`DELETE /issues/{issueId}/relations/{relationId}` refuses when the relation
involves neither issue: `rel.From != pathIssue && rel.To != pathIssue`. The
mutation gate reported the SECOND operand surviving at negation, and the first
one killed — which is the whole story. Every scenario that removes a relation
removes it from the `from` end, so a check that examines one endpoint and a
check that examines both agree on every input the suite produces. The mutant
that survives is the one that breaks the untested end, and it also lets any
issue id in the path remove any relation in the system.

The contract describes the removal's effects at length — clears both sides,
snapshots the relation into the event, bumps ancestor subtree revisions — and
never says what `{issueId}` is for. It is the only reason the route is nested,
and no acceptance criterion mentioned it. `AC-block-remove-endpoint` now names
the set (either end works, anything else is a 404 naming both the relation and
the issue) and its scenario samples the untested member.

The same handler produced a second survivor two lines on:
`if toIssue.Project != fromIssue.Project` guards the far project only when the
relation spans two. For a same-project relation, guarding one end and guarding
both are indistinguishable — so, again, the only input that separates them is
the one no scenario built. A cross-project blocks relation is a designed
feature (`parent_of` is refused across projects precisely because its cascades
walk out of one; blocks never cascade, so it may span "with both sides
guarded"), and that design lived entirely in a code comment. Archive the far
project and the live side becomes a lever for writing into a frozen one.
`AC-block-cross-project` now carries the rule and covers both writes.

Both are the same shape: a guard whose two readings coincide on every input the
suite happens to produce, so the AC is discharged without ever exercising what
it claims. Note that this is the second of the sixteen `guardWritable` call
sites the gate has reached; `AC-project-archive` still says "read-only" and
still samples one door, which stays open as a question for Brent.

## A shape rule the fixtures could never break

`isCommitSHA` accepts a commit id at 40 hex digits or 64 — git's two object
formats — and refuses everything else with a 400 naming the form. The mutation
gate reported the second width's check as a survivor, and the reason is in the
harness rather than the feature files: every commit in every scenario is a real
sha minted by `git commit` in a real repository, and `git init` builds sha-1
repositories, so the shape check has never seen anything but a well-formed
40-hex id. The 40-hex operand was already killed; its twin could not be, because
nothing in the suite is 64 hex digits or shorter than 40.

The spec is thinner than the code. `AC-review-create` says "pinned at an
immutable commit" and the contract annotates the field `# canonical full object
id`; neither states a width, and nothing anywhere says an abbreviation is
refused. That silence is the gap, not the width itself — *why* a full id is
required is the whole point: an abbreviation names whatever it currently
disambiguates to, and a repository that grows can make the same prefix name a
second object, or none, at which point the review's pin has quietly stopped
being a pin. `AC-review-commit-pin` now says that, and its outline samples the
members the fixtures cannot mint: an unknown 64-hex id, a real id cut to twelve
digits, and a full-width non-hex string.

The species this belongs to is worth naming on its own: **a rule the test
fixtures are structurally incapable of violating.** Every earlier gap was a
scenario that could have exercised the branch and didn't. Here the harness
generates its inputs from a real tool, and that tool only emits valid ones —
so no amount of scenario-writing in the existing style would have reached it.
The inputs had to be synthesised by hand, against the grain of a harness built
to be realistic. Realistic fixtures are exactly the ones that never produce the
malformed input a validator exists to refuse.

## A bound chosen to be invisible, duplicated three times, hiding a real refusal

`scanExplicitNulls` caps how much of a JSON key it retains, so the scan stays
O(1) over hostile input. The mutation gate reported all three copies of that
cap check as survivors, and the reason they survive is the reason the cap was
chosen: 256 bytes is far past any field name a handler looks up, so moving the
bound by one byte changes only a map entry nobody reads. The bound is deliberately
set where it cannot matter — which is exactly what makes it unfalsifiable.

Chasing that, though, turned up something that does matter. The captured prefix
was handed to `encoding/json` whenever it contained a backslash, and a prefix
can end *inside* an escape sequence. A body whose long key merely happens to
carry a backslash across the cap came back rejected — 400, "bad key escape" —
for an escape it does not contain. The function's own contract says it reports
gross shape errors only and leaves malformed JSON to pass B; this was neither
shape nor malformed. An overlong key's decoded spelling is discarded three
lines later regardless, so the decode bought nothing and cost a refusal on
well-formed input. It is now skipped when the key is overlong.

The three duplicated cap checks are now one closure that every byte passes
through — plain, backslash, escaped alike — so the bound cannot drift between
the three routes a byte takes into the buffer. Collapsing them also collapses
three unfalsifiable sites into one, and the one that remains is falsifiable
after all: a key *at* the cap is a name and its null-ness is honored, a key one
byte past it is a prefix and is dropped. Sample one side and a bound at 256
reads exactly like a bound at 257. Four unit cases pin both sides by both
routes; all four mutants of the surviving check die.

Two lessons, and they pull in opposite directions. The first is that a constant
chosen so generously that no input can reach its boundary is a constant whose
boundary no test can pin — the generosity is what makes it safe *and* what
makes it untestable. The second is that "unkillable" was a poor diagnosis: the
site was unkillable as written, and rewriting it — once, honestly, without
weakening the guarantee — made it killable. Three copies of a rule are three
places for it to be wrong; one copy is one place to test.

## Timeouts that are neither survivors nor defects

Twice now the gate has reported `TIMED OUT` rather than `LIVED` — first at the
commit-shape check, then at the doc-deliverable branch. Both mutants negate a
nil check whose body dereferences the very pointer being checked, so the
mutated server panics on the first request that reaches it. Neither is a
reachable state in unmutated code: the guard is what makes the dereference
safe, and negating it produces a program no input could have produced.

Two things follow. Gremlins scores efficacy as killed/(killed+lived), so a
timeout costs nothing and needs no exclusion — the right response is to read
the site, confirm the panic is guard-manufactured, and move on. But the four
minutes each one burns is not gremlins' doing: a handler panic should fail the
acceptance suite immediately, and instead the run hangs until the *binary*
timeout fires. `go test -timeout` is not honored here — something in godog's
flag handling inside `TestMain` overrides it — so every panicking mutant costs
the wall clock of a full binary timeout rather than a failed scenario. Both
mechanisms that could plausibly wedge a real server were checked and are
panic-safe (every `s.db.Begin()` is followed by a deferred rollback; the
large-body slot is released by a deferred `release()`), so this is a harness
property, not a production one.

Left alone deliberately — restructuring the harness is not in scope for the
build — but it is the thing to fix first if the mutation gate is ever to run in
CI, and it belongs in whatever respecifies that gate.

## A guard duplicated at every call site, where the callee already knew

Both operands of `if req.ExpectedBaseCommit != nil || req.ExpectedDefaultHead
!= nil` survived, at the create door and again at the resubmit door, where the
identical guard is copied. It wraps `RevalidateFences`, the second fence check
that runs at the tail of prepare. Nothing observable distinguishes the guard
from its absence: the fences were already validated inside `resolveDeliverable`,
so the recheck exists to narrow the window between that validation and the
commit, and the only input that separates "rechecked" from "not rechecked" is a
push landing inside that window. The acceptance suite cannot stage that race,
and neither can any test that drives the server over HTTP.

So the guard is not a rule at all — it is a cost decision, avoiding a git
subprocess when there is nothing to check. And it was stated twice in the API
layer while the function it guards, which alone knows what a fence means, said
nothing. Moved: `RevalidateFences` now returns before touching git when no
fence was supplied, and both callers hand their fences over unconditionally.

The consolidated decision is falsifiable in a way the two copies were not,
because it can be asked directly rather than through a race. Point it at a
repository that cannot be read: with nothing pinned it must still succeed —
a submission that made no claim about the repository must not be refused over
one — and with either fence supplied it must fail, or the short-circuit would
be indistinguishable from skipping revalidation altogether. Three unit cases,
and all four mutants of the surviving guard die, including its outright
removal.

This is the same lesson as the key-capture cap one section up, arriving by a
different road. There the rule was copied three times inside one function; here
twice across two handlers, with the callee that owned the concept left out of
it. In both cases the duplication was what made the site unfalsifiable, and
stating the rule once — in the place that owns it — made it testable without
weakening anything.

## The kind of deliverable nobody ever read

Both operands of `if (!found || content == nil) && sub.DocVersion != nil`
survived, in `getReviewDeliverable`. The branch resolves a doc review's text
from the immutable document version, because a doc review stores no copy of
what it reviews — a code deliverable renders at submission and the render is
stored, a doc deliverable is a reference and nothing else. Kill either operand
and the branch stops firing, and a doc deliverable comes back 409 "submission
predates stored content" instead of its text.

Nothing caught that, because no scenario had ever read a doc deliverable's
content. `AC-review-web` says the deliverable's content is shown for reading,
and the suite discharged it with a code deliverable — species 1, an AC naming
a set and sampled at one member. The rule that made doc different, that the
version IS the deliverable and there is no stored copy, appeared only in a
comment inside the branch it justified — species 2, implemented behavior with
no AC. The two species compound: the AC that would have covered the doc route
looks discharged, so the missing AC never shows up as missing.

Added `AC-review-doc-deliverable-resolves`, saying that the two kinds reach
the reader by different routes and only the doc route has nothing stored to
fall back on, and a scenario that reads the deliverable of a doc review and
checks it carries the version's own text. The fixture already existed; no test
had ever asked it for content.

### A returned bool that was never a second fact

Chasing the survivors turned up a redundant operand. `ContentAt` returned
`(*string, bool, error)`, and its bool was false exactly when its pointer was
nil — sql.ErrNoRows was the only path that set it. So `!found || content ==
nil` was a two-term way of writing `content == nil`, and `!found` could not be
mutated into anything observable. Dropped the bool: one caller, and the caller
cared about one thing. Both remaining operands of the branch die under
mutation, as does the 409 guard below it.

### And one operator that no input can reach

The `&&` itself still survives, and this one is not a testing gap. Swap it for
`||` and a *code* submission carrying no stored content would enter the doc
branch and dereference a nil doc version. No such row can exist: the live
create path renders and stores content for every code deliverable, and import
refuses a bundle whose code submission omits its content (and, symmetrically,
one whose doc submission carries any). With both doors enforcing it, `content
== nil` and `sub.DocVersion != nil` are the same fact about every row the
database can hold, so `&&` and `||` compute the same answer on all of them.

That also makes the 409 below unreachable — species 5, a guard reachable by no
input, arrived at from the opposite direction: not a condition nobody thought
about, but an invariant enforced so completely at every door that the guard
downstream can never see it broken. Left in place. Removing it would trade a
dead branch for a live nil dereference the first time an invariant two files
away stops holding, which is a bad trade for a mutation score.

## Every filter named, so the unfiltered query was never asked

Eighteen survivors clustered in one file. `search.go` composes three query
parameters — `q`, `project`, `session` — and derives a fourth state from them:
`bare`, meaning none was given. That state gates four separate blocks. Not one
scenario had ever issued a search with a filter missing. Every scenario in
REQ-search names a project, a term, or a session, because every AC did.

The handler's own comment claimed the contract required it: "all content in the
remaining scope — a project when set, otherwise EVERYTHING." The contract says
that about threads only. The rest was extrapolation, and correct extrapolation
as it turns out — but a rule nobody wrote down is a rule nobody can check, and
a handler answering an unfiltered query with nothing at all would have looked
exactly like one answering it correctly. Species 2, at the widest scale seen so
far: not one branch unspecified, but a whole axis of the query surface.

`AC-search-scope` states the axis: the filters intersect, so each one omitted
*widens* the scope rather than narrowing it, and omitting all of them widens it
to the database. One scenario outline with three rows — nothing at all, only
the term, only the project — killed fourteen of the fifteen boolean mutants in
the file.

The asymmetry worth naming in the AC: a review carries no text of its own, so
it enumerates under a scope but never under a term. That is why the term row
expects issues, docs, and threads but no reviews, and it is exactly the kind of
sentence that only gets written when someone tries to state the rule.

### The fifteenth: a filter pair nobody combined

`if project != nil && ref.Project != *project` decides whether a session-reached
issue belongs in the result. It survived the outline because the outline never
sets two filters at once, and the session scenario sets `session` alone. Species
1 again, in its combinatorial form: the rule "filters intersect" was now stated,
but the one pairing that can violate it — a session whose work reaches an issue
outside the named project — had no fixture. `AC-search-session` gained the
second half of its sentence (a session names work, not a place) and the scenario
gained a step that scopes the same session to a project the work never touched.

### An order the reader depends on and nobody promised

Three more survivors sat in the result sort. The handler gathers issues into a
map — deliberately, to dedupe across passes — and a Go map has no order at all,
so the sort is the only thing between the reader and a sequence that changes
every request. Nothing said so. Every assertion in the suite was
`strings.Contains(body, id)`, which cannot see order by construction.

`AC-search-scope` gained the guarantee: each project's issues arrive together,
ascending by number, however many internal passes contributed them. Grouping is
the promise — not which project leads, since the sort key is a UUID and no
reader should depend on that.

Killing it took two rounds, and the second round is the lesson. With two issues
in the project, a mutated comparator that leaves the map order untouched still
produced a correct-looking answer about a third of the time, because a random
order of two items is sorted half the time. The assertion was right and the
fixture was too small to make it bite — species 9, sharpened: not a fixture that
*cannot* violate the rule, but one that violates it too rarely to notice. Four
issues in one project, and three runs in a row killed it.

### The meta-lesson: mutants that flip a coin

A mutant whose surviving behavior is *random* cannot be killed by a stronger
assertion — only by a fixture large enough that accidental correctness stops
happening. Any gate reporting such a mutant as survived is reporting one sample.
This is the second sampling trap in this run; the first was the stale tree.

### The first sampling trap: the run tests the tree it started on

Partway through this cluster the survivor list stopped making sense — reported
mutants in `export.go` were already covered by tests sitting in the working
tree. The mutation run snapshots the module when it launches, and this one had
launched 24 commits earlier, before roughly 2,300 lines of unit tests across
nine new files. Six mutants re-probed by hand against HEAD came back 6/6 killed.

So the output is a *candidate* list, not a work queue. Line numbers belong to
the snapshot, not to HEAD: reading them needs `git show '<tree>:./path.go'`, and
acting on them needs a re-probe. Before merge the gate has to run again against
the tree it is judging.

## An anchor has two halves, and a retarget has two ends

Six survivors in `internal/api/threads.go`. Confirmed current first — the file
is byte-identical between the gremlins tree `b51d4d8` and HEAD, so these line
numbers meant what they said. Three of the six were species 2 again, and they
all trace to one AC that under-described what it was guarding.

`AC-thread-anchor` said a thread is anchored to a project or an issue, at least
one, retargetable later, and that both sides list it. Every word of that is
true, and none of it covers either thing the code actually checks:

- **An anchor naming both a project and an issue has two halves that can
  disagree.** `resolveAnchor:39` refuses the pair when the issue lives in some
  other project. Nothing in the spec said the halves must agree, so nothing
  tested it, so the guard was free.
- **A retarget touches two projects, not one.** `guardCurrentAnchor` resolves
  the project the thread is *leaving* — through its issue at `:228`, or
  directly — and guards that too. Only the destination was ever exercised, so
  the whole current-anchor path (`:228`, and the `project == ""` shortcut at
  `:231` for a thread anchored to nothing) never had to be right.

The second one is the interesting failure, because the guarantee it protects
belongs to a *different* requirement: archiving a project freezes its contents.
A thread anchored inside an archived project is that project's content, and
without this guard it could walk out while everything around it stayed frozen.
`AC-thread-anchor-guarded` states both halves; one scenario with two `When`s
kills all three mutants.

### Two mutants that were never about behavior

The other three were the familiar species-4 shape — sites where no output can
change — and both got removed rather than excluded.

`:123` was `if apiErr := writeThreadJSON(...); apiErr != nil { return }` as the
last statement of a handler. The status is already committed; the branch
returns, and falling through also returns. Byte-identical either way. Deleting
the conditional deletes the mutant: `_ = writeThreadJSON(w, thread)`.

`:269` was arithmetic on a capacity hint — `len(raw)+len(t.Transcript)+16`,
where the `16` is slack chosen so nobody has to count. A capacity that only
approximates cannot be wrong in any observable way, which is exactly why two
mutants lived there. Sizing it *exactly* turns the hint into a claim:

    const key = `,"transcript":`
    buf := make([]byte, 0, len(raw)-1+len(key)+len(t.Transcript)+1)

and now `cap(out) == len(out)` iff the buffer never grew. That is a resource
guarantee, not an observable one, so under the established ruling it earns a
unit test rather than an AC. Both arithmetic mutants die on it.

### The lesson: slack is what makes a resource claim untestable

Species 10 was a constant so generous no input reached its boundary. This is the
same shape one level down: `+16` is slack, and slack is unfalsifiable by
construction — every arithmetic perturbation of it still produces a buffer big
enough. Counting the bytes exactly costs one `const` and makes the thing the
comment already claimed ("no second copy of an unbounded transcript") into
something a test can check.

## Nobody ever wrote a flag anywhere but last

Four survivors in `internal/cli/cli.go`, in two pairs, and the cluster's own
first lesson is about reading the report. The reported line 188 does not exist
in the tree the earlier clusters were checked against — cli.go had shifted by
exactly twelve lines when `CoveredOperations` and its `slices` import were
deleted — but it is line 188 at HEAD, exactly. The mutation run's snapshot is
per-file and its age is not uniform. `git rev-list HEAD -- <file>` with the
target grepped out of each revision says which tree a report belongs to; the
answer here was HEAD, not the tree the previous cluster used.

### Two mutants at the end of the argument list

`splitFlags:188` is `if i+1 < len(args)`, and both the arithmetic mutant
(`i-1`) and the boundary mutant (`<=`) make the guard true when `i` is the last
index — so `args[i+1]` reads past the end and the CLI panics. Neither could be
observed, because **every scenario in the suite writes its flag last**, and the
only flags written last were `--json` and `--yaml`, which are caught by the
boolean case above and never reach line 188 at all.

The spec is why. `AC-cli-json`, `AC-cli-yaml` and `AC-cli-stable-shape` are all
about what comes *out*. Nothing described what goes *in* — the argument grammar
was never specified, so the scenarios wrote the one command line that asks
nothing of the parser. `AC-cli-flag-grammar` states the grammar: format flags
never consume the argument after them, every other flag takes the next argument
and takes one only when there is one to take. Two `When`s kill both mutants —
`--json` written ahead of its positional, and a value-taking flag written last.

This is species 1 with an unusual shape. The AC named a *set* implicitly — the
positions a flag can occupy — and the scenarios sampled the one position where
the code has nothing to decide.

### Two mutants the API cannot see

`doRaw:231` and `:234` decide two request headers: `Content-Type` when there is
a body, `Idempotency-Key` when the method is not GET. Negating either changes
nothing an acceptance scenario can observe, because sutra's own daemon reads
neither back and accepts a body whether or not it is labelled. Species 8 in its
purest form: a contract rule — the OpenAPI document declares request bodies as
`application/json` — enforced at *no* door.

Both still matter off loopback, which is what `AC-parity-remote` is about: an
unlabelled body is at the mercy of whatever the receiving server assumes, and an
idempotency key on a GET asks a proxy to remember a read. That is a protocol
decision, not an observable guarantee, so under the established ruling it earns
a unit test — one that runs the client against an `httptest` server and reads
the headers off the wire. It kills `:231`, and it kills `:234` before that
mutant was even reported.

### The lesson: the wire is a surface, and the suite was only watching one

Every acceptance scenario in this build observes the *response*. Four of the six
CLI decisions here are about the *request*, and two of them are invisible to a
server that happens to be lenient. Testing a client only through the server it
ships with cannot see anything the server chooses to ignore.

### The init that could have said nothing

One more in the same file, and the smallest gap yet. `initProject:275` writes
the marker and then, on the next line, prints what it created. Negating the
write's error check makes the successful path return *before* the print — and
no scenario noticed, because the init scenario checked the project, checked the
marker, and never looked at stdout.

`AC-project-init` listed three things init does — create, record, mark — and the
fourth, telling you, was not among them. It is not decoration: the project id in
that line is what a script carries forward, since the key is only what the
caller already typed.

The ORDER is the other half. A report printed first and a report printed last
are indistinguishable on a successful init, so the AC's promise — nothing is
announced that did not happen — is only falsifiable when the marker write fails.
That is a robustness direction, so it earns a unit test: a Workdir that is a
regular file makes the write fail without permission games, and the assertion is
that stdout stayed empty. Both halves stated, both halves tested.

### The reference nobody typed wrong

Three more in `cli.go`, all on one line — `if cut <= 0 || cut == len(args[1])-1`
— and one line is the whole shape check for how an issue is named on the command
line. The boundary mutant lets a leading hyphen through; the two arithmetic
mutants let a trailing one through. Every one of them survived because **no
scenario in the suite ever typed a reference wrong**. `SUT-1` was the only
reference ever written, and it is well formed.

`SUT-1` also hides the rule the line exists for. A reference splits at the LAST
hyphen because a project key may contain hyphens of its own — `MY-PROJ-42` is
issue 42 of `MY-PROJ`, not issue 42 of `MY`. In `SUT-1` the first hyphen and the
last are the same hyphen, so the split rule decides nothing and a wrong rule
would pass. That is species 9 once more: a fixture structurally incapable of
violating the rule it was supposed to check.

`AC-cli-issue-reference` states the split; `AC-cli-reference-shape` states the
refusal and, more importantly, *when* — on shape alone, before the key becomes a
lookup. Without that ordering a mistyped reference comes back as `no project
with key ""`, a complaint about a project the caller never typed. A five-row
outline covers the ways the alias can fail and kills all three mutants, plus the
`n < 1` guard on the next line before it was reported.

### The lesson: a validator is a set, and the happy path samples none of it

Three of this build's clusters now have the same root: a linear validator whose
every guard is untested because the only inputs the suite ever built were valid
ones. Guards are the cheapest place for a mutant to hide, and an outline is the
cheapest thing that flushes them — one row per way the input can be wrong.

## The door every operation walks through, and nobody walked through it

Seven mutants in `generic()`, the function behind `sutra api <operationId>` — the
generic invoker that reaches any operation with no bespoke command. They arrived
one at a time and read like seven unrelated leaks; they are one hole.

`AC-parity-coverage` promises that "for every operation in the API contract, a
CLI command exists that invokes it." The suite discharges that promise with a
**static mapping check** over the contract, plus exactly two invocations:

```
"sutra api listIdentities --json",
"sutra api listProjects --json",
```

Both are parameterless reads that succeed. Between them they exercise the
operation lookup and nothing else. Every decision `generic()` makes after that
line was untouched, and each one grew a mutant:

| site | mutant | what it did |
|---|---|---|
| `len(query) > 0` | `>= 0` | appended a bare `?` to every URL |
| `len(query) > 0` | `<= 0` | dropped every query parameter |
| `body != nil` (payload) | `== nil` | dropped every request body |
| `body != nil` (header) | `== nil` | mislabelled the Content-Type |
| `method != GET` | `== GET` | keyed reads, left writes unkeyed |
| `StatusCode >= 400` | `> 400` | reported a 400 as a result |
| `io.Copy(...); err != nil` | `== nil` | dropped the terminating newline |

This is species 1 at its largest scale. The AC names a set — every operation in
the contract — and the coverage check samples it *statically*, which proves a
mapping exists and nothing about what happens when you use it. A command that
exists but drops half the request covers the operation on paper only.

### Three of them were the same code, written twice

`generic()`'s request construction was a verbatim copy of `doRaw`'s: the same
nil-body check, the same Content-Type line, the same idempotency-key rule. The
previous cluster pinned `doRaw`'s copy with `request_test.go`. The other copy sat
there unfalsifiable, because nothing that tested one touched the other.

The fix was not a second test. It was `newRequest` — one function both doors call
— which deletes three mutant sites outright and leaves the surviving copy already
covered. This is the same lesson as the duplicated status rule earlier in this
document, and it keeps arriving: **duplication is what makes a site
unfalsifiable, because neither copy can be falsified without the other.**

### One promise the API cannot show you

`>= 0` only ever appends `"?"` to a path with no query. sutra's daemon reads
`/projects` and `/projects?` as the same request, so nothing an acceptance
scenario can observe distinguishes them — species 4's neighbourhood, an
*apparently* equivalent mutant.

It is not equivalent off loopback: an empty query component is a distinct URI,
and a cache keyed on the request line holds two entries for one resource. So it
gets the same treatment Content-Type got — a unit test asserting the exact
`RequestURI` — under the standing rule that **observable guarantees earn
acceptance criteria; wire and resource decisions earn unit tests.**

### The lesson: coverage of a set is not coverage through it

`AC-parity-coverage` was satisfiable by a check that never sent a request. The
new `AC-cli-generic-invoker` states what the invoker carries — path parameters
substituted, query appended, `--body` verbatim, a refusal reported as a failure
and not a result, output terminated as a line — and the scenario puts a query, a
body, and a rejection through it.

The general shape, and the eleventh species: **an AC that asserts a mapping
exists is discharged by checking the map, not the territory.** Where a spec says
"there is a command for every X," some scenario has to actually run one for an X
that is not the easiest X.
