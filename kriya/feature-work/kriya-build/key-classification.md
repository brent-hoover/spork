# Key classification — kriya's 37 `_key` columns

Required by `scope.md`: *"each must be classified as an idempotency key or
an identity/scoping reference before it is implemented."* Generated from
`kriya/avspec.yaml`, not hand-listed.

The distinction is load-bearing, not bookkeeping. An **idempotency key** is
a crash window: a value written ahead of an external call so recovery can
replay under it exactly once. An **identity/scoping reference** names or
groups a row and is often deliberately non-unique. Treating a reference as
an idempotency key would fence work that should proceed; treating an
idempotency key as a reference would let a replayed call apply twice.

`unique` below is the spec's own declaration. Note that most idempotency
keys are NOT declared unique — uniqueness is generally scoped to a parent
rather than global, so the implementation must add the scoped constraint
rather than assume the flag carries it.

| Entity | Owner | Column | Kind | `unique` |
|---|---|---|---|---|
| PlannedTicket | planner | `idempotency_key` | idempotency | yes |
| IntakeAttempt | planner | `intake_key` | idempotency | yes |
| IntakeAttempt | planner | `target_key` | identity/scoping | no |
| SpecMapping | planner | `mapping_key` | identity/scoping | yes |
| SpecMapping | planner | `project_key` | identity/scoping | no |
| PopFence | planner | `singleton_key` | identity/scoping | yes |
| PlanHead | planner | `target_key` | identity/scoping | yes |
| CompletionAdvance | planner | `advance_key` | idempotency | yes |
| CompletionAdvance | planner | `target_key` | identity/scoping | no |
| CompletionAdvance | planner | `relation_key` | identity/scoping | no |
| CompletionAdvance | planner | `reclaim_removal_key` | idempotency | no |
| BuildTarget | planner | `target_key` | identity/scoping | yes |
| BuildTarget | planner | `completion_submission_key` | idempotency | no |
| BuildTarget | planner | `completion_resubmission_key` | idempotency | no |
| BuildTarget | planner | `completion_report_key` | idempotency | no |
| BuildTarget | planner | `completion_operation_key` | idempotency | no |
| Plan | planner | `decomposition_key` | idempotency | yes |
| Plan | planner | `project_key` | identity/scoping | no |
| BuildRun | orchestrator | `pop_key` | idempotency | yes |
| BuildRun | orchestrator | `review_submission_key` | idempotency | no |
| BuildRun | orchestrator | `ticket_close_key` | idempotency | no |
| BuildRun | orchestrator | `finding_doc_key` | idempotency | no |
| BuildRun | orchestrator | `review_resubmission_key` | idempotency | no |
| MergeAttempt | orchestrator | `attempt_key` | idempotency | yes |
| MergeAttempt | orchestrator | `target_key` | identity/scoping | no |
| MergeAttempt | orchestrator | `consume_key` | idempotency | no |
| AttributionAmbiguity | orchestrator | `ambiguity_key` | idempotency | yes |
| AttributionAmbiguity | orchestrator | `project_key` | identity/scoping | no |
| AttributionAmbiguity | orchestrator | `resolution_key` | idempotency | no |
| AttributionAmbiguity | orchestrator | `reclaim_removal_key` | idempotency | no |
| Stall | orchestrator | `stall_key` | idempotency | yes |
| Stall | orchestrator | `target_key` | identity/scoping | no |
| DevSession | dev-loop | `thread_import_key` | idempotency | no |
| ReviewRound | review-bridge | `operation_key` | idempotency | yes |
| OrphanObservation | review-bridge | `observation_key` | idempotency | yes |
| GateResult | gates | `result_key` | idempotency | yes |
| Learning | context | `project_key` | identity/scoping | no |

**37 columns total — 24 idempotency keys, 13 identity/scoping references, 15 declared `unique`.**

Every idempotency key above is a crash window a scenario exercises. The
identity/scoping set is `target_key`, `project_key`, `singleton_key`,
`mapping_key`, and `relation_key` — the first four name or group a row,
and `relation_key` identifies a sutra relation rather than fencing a call.
