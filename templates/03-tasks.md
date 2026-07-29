# Tasks

The agent's build plan. Ordered work items; each declares which acceptance
criteria it satisfies and which components/contracts it touches. Mirrors
`avspec.yaml` under `tasks:`. `[P]` = safe to parallelize.

> Verifier checks: every `AC-*` is satisfied by ≥1 task, every referenced
> `AC/CMP/CTR` ID exists, and `depends_on` forms no cycle.

- **TSK-001 — <title>.** Satisfies: AC-001. Touches: CMP-001, CTR-001.
  Depends on: —
- **TSK-002 — <title>.** Satisfies: AC-002. Touches: CMP-001. Depends on: TSK-001.
- **TSK-003 `[P]` — <title>.** Satisfies: AC-003. Touches: CMP-002. Depends on: —

Ordering rule: a task may only depend on tasks that appear before it once the
graph is topologically sorted. Tasks with no path between them and marked `[P]`
may run concurrently.
