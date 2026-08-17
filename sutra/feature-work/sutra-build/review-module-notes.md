# Review module — implementation notes

Working notes for MOD-review (task #30). Contract: lines 1234–1496
(endpoints) and 2272–2450 (schemas) of contracts/sutra.openapi.yaml.

## Boundary-forced design

- review may_import [identity, events] ONLY. It cannot read projects, so
  the api layer passes `repo_path` and `default_branch` into
  create/resubmit; review owns the git invocations against that path.
- issues may_import review — the close gate calls review's
  ConsumeForClose from the issues transition (via api orchestration).

## Store shape

reviews: id, issue, author, state (open|changes-requested|approved),
revision (1+), session, summary, branch, commit, doc_version (latest
mirror), latest_verdict_event, consumed, consumed_revision, close_used,
created. review_submissions: id, review, revision, branch, commit,
base_commit, doc_version, session, created — immutable, one per
revision.

## Git resolution (code deliverables)

At create/resubmit, inside the handler (NOT the store): resolve
`base_commit` = `git -C repo_path merge-base <commit>
refs/heads/<default_branch>`; `expected_default_head` validates against
`git rev-parse refs/heads/<default_branch>`; `expected_base_commit`
validates against the resolved base. Unset repo_path or unresolvable
refs → 409. Git runs BEFORE the DB transaction opens (network-free,
local, but still I/O — keep transactions short); fences re-validated
inside the transaction is unnecessary since git state is external to
the DB — the pinned values are whatever resolution returned.

## Fences (all atomic in the mutating transaction)

- verdict: rejected once consumed (conflict, no mutation); each verdict
  emits an event and replaces latest_verdict_event.
- consume: state approved AND expected_revision == revision AND
  expected_verdict_event == latest_verdict_event; stamps consumed +
  consumed_revision + review.consumed event. Distinct-key retry against
  consumed → 409 review-consumed (original key replays via idempotency).
- resubmit: state changes-requested AND expected_revision match;
  revision++, new submission row, state open.
- close (issues transition): review belongs to issue, state approved,
  review_revision == revision, review_verdict_event ==
  latest_verdict_event, close_used absent → stamp close_used (and it
  implies consumed per schema: close stamps consumed+consumed_revision
  too if not already consumed? Contract: close_used implies both
  consumption fields — the close stamps ALL THREE when unconsumed, or
  just close_used when already consumed at the same revision).

## Deliverable kinds

Code first (git). doc_version deliverables reject 404/409 truthfully
until the docs module exists (same pattern as the pre-review close
gate).

## Test strategy

Unit tests build throwaway git repos (git init, two commits, branch) in
t.TempDir(). Acceptance scenarios needing approved reviews come green in
stage 3 with the close-gate wiring.
