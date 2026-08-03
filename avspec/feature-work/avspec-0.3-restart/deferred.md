# Deferred work — avspec 0.3 restart

Accumulated during slice 1, the sutra dry run, and the review loop.
(When sutra is built, these become sutra issues — until then this file is
the backlog of record.)

## Slice 2 — recommended cut

1. **Constitution `check:` attachments.** A constitution entry may carry a
   machine check (e.g. `check: contract-mutations-have-idempotency-key`),
   making principles verifier-enforced. Closes the tech-spec's original
   promise ("clean-code limitations verified by running a command") and
   formalizes the ratchet: review findings that turn out mechanical migrate
   into checks permanently. Motivating case: the idempotency-key findings
   from the sutra review loop.
2. **Cross-manifest links.** `modules[].spec: <path>` so a system-level
   manifest (spork's root avspec.yaml) follows into child specs and the
   verifier checks the system, not just each app. Today the link is only a
   comment on MOD-sutra.
3. **`avspec render`.** Mermaid module graph, ER diagram, and screen graph
   generated from any manifest. Diagramability is a standing requirement
   (owner needs the big picture on demand); hand-built once this session.

## Slice 2 — ride-alongs if room

4. **`envelope:` section.** Operating facts (concurrent users, data
   volumes, latency tolerance) as first-class fields — facts to build
   with, not testable requirements. Concept approved in retro; currently
   prose inside `project.description`.
5. **`shows:` relation support.** `ENT-issue.labels` is inexpressible
   because labels is a relation, not a field. Found via the kanban card
   (VIEW-board carries a comment marking the gap).
6. **`UI_UNDECLARED`-class rule.** A module whose responsibility implies a
   user surface but declares no `ui:` block passes silently (MOD-web slid
   through the dry run until manually caught).

## Later slices

7. **Code-level boundary enforcement.** `avspec verify` (or a sibling
   command) runs `stack.commands.arch` so declared boundaries are checked
   against real imports. Slice 1 deliberately verifies the spec only.
8. **Cleanup batch.** `templates/` still speaks the 0.1 format;
   `verification/architecture-rules.yaml` still uses `CMP-` vocabulary;
   wellformed unit tests assert codes but not severities.

## Sutra contract polish (non-blocking, from the final review rounds)

9. 400-response enumeration is asymmetric across mutations that take
   identity refs (some declare it, most rely on the shared BadRequest).
10. Mixed nullability conventions: `Thread.session` and
    `Document.current_version` are `nullable` while sibling fields use
    omit-when-absent.
11. `searchThreads` carries a sentence about review-session matching that
    belongs on `listReviews`/`/search` only.
12. Risk/spike representation: kriya's risk-first planning needs risk items
    and spike tickets visible in sutra. Convention for now — a `risk` /
    `spike` label plus blocks-relations making risky items block their
    dependents; promote to a first-class field only if the convention
    proves insufficient.

## Decisions to record when slice 2 starts

- **Sutra took the ATS's place.** The ATS was chosen as the medium example
  because it is an app with a well-understood feature set; sutra serves
  the same purpose (issue trackers are an equally well-understood genre)
  while also being a real spork component, and its live interview
  validated more (multi-module, data, apps, UI, full AC set). Three tiers
  are now: linkshort (small, hand-written fixture), sutra (medium,
  interview-authored), spork (large, system-level).
