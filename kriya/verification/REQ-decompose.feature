Feature: Spec decomposition
  The PM agent turns a pinned SpecSnapshot into tracer-bullet tickets in
  sutra under one umbrella epic per build target. Re-decomposition after
  a spec amendment supersedes the old plan through a durable, crash-safe
  protocol: phased creation behind barriers, state-aware idempotent
  retries, retirement with consumed/disposition stamps, and forward-only
  operator recovery.

  Scenario: tracer bullets carry traceable acceptance criteria
    Given a pinned SpecSnapshot for project "shorty"
    When the PM agent decomposes it
    Then every created ticket is a thin end-to-end slice
    And the first workable ticket is a walking skeleton touching every layer
    And every ticket's acceptance criteria cite REQ and AC ids present in the snapshot

  Scenario: one epic umbrellas the build target
    Given no epic exists yet for the build target
    When the first decomposition runs
    Then an umbrella epic is created and every ticket is parented under it
    When a later decomposition supersedes the plan
    Then carried-forward and new tickets sit under the same epic with no re-parenting
    And the epic can close only when the build completes

  Scenario: relations wire risk ordering and parallelism
    Given the snapshot carries a risk item with dependent requirements
    When the PM agent decomposes it
    Then the spike ticket blocks every ticket that depends on the risk
    And tickets with no dependency between them carry no relation
    And an agent popping work receives an unblocked ticket

  Scenario: phases run behind plan-wide barriers
    Given a decomposition producing tickets, relations, and assignments
    When the plan executes
    Then every ticket is created before any relation is wired
    And every relation is wired before any assignment is made
    And no ticket is workable before the relation phase completes
    And completed is stamped only after the final assignment barrier

  Scenario: same-key retries are state-aware
    Given a plan exists for decomposition key "K"
    When a request with key "K" arrives again
    Then a pending plan resumes from its durable progress
    And an active plan without completed resumes its remaining phases
    And a completed plan returns the historical result with no mutation
    And a superseded plan returns the historical result with no mutation
    And an awaiting-operator plan is preserved untouched for the operator

  Scenario: supersession retires the predecessor before activating
    Given an activated head plan "P1" and an amended snapshot
    When decomposition runs with a new key producing candidate "P2"
    Then one atomic transaction points "P2" predecessor at "P1", marks "P1" superseded, increments the pop fence, and moves PlanHead with a generation bump
    And every "P1" row is stamped consumed with disposition "retired" or "carried-forward" before "P2" activates
    And activation decrements the pop fence in the head-moving transaction

  Scenario: retirement distinguishes pending from issued work
    Given predecessor rows with a pending ticket, an issued ticket, and a row whose ticket was never created
    When retirement runs
    Then the pending ticket is deferred in sutra via an expected-status "open" conditional transition with a generation-scoped defer key
    And a fresh read showing the ticket already deferred counts as fence established with no retry
    And the row whose ticket was never created retires with no sutra call
    And the issued ticket is carried forward, not deferred

  Scenario: every parked state recovers forward
    Given a head plan parked awaiting-operator before activation
    When the operator restores it
    Then it returns to pending with predecessor rows reset from durable step progress and retirement resumes
    Given a head plan parked awaiting-operator after activation
    When the operator restores it
    Then it returns to active with ticket-row states recomputed from per-step progress
    Given a non-head candidate parked after losing the replacement CAS
    When the operator retries it
    Then it re-enters the replacement CAS and on winning atomically repoints its predecessor to the head it beat
