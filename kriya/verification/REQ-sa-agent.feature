Feature: System architect agent
  The escape valve when dev and review cannot converge. The architect
  directs but never codes; scope decisions always rise to the operator;
  every impasse and its resolution survive in the run's history.

  Scenario: an impasse pauses the run and reaches the architect
    Given a round limit of 4 was snapshotted onto the BuildRun at its creation
    And the operator has since changed the configured limit to 2, which does not affect this run
    And three consecutive rounds have returned findings since the last clean pass
    When the fourth consecutive round returns findings
    Then the run pauses and the impasse reaches the SA agent
    And the SA receives the findings history, the ticket's acceptance criteria, and the bound snapshot
    Given only two consecutive rounds have returned findings
    Then the loop continues without pausing
    Given a clean pass lands mid-count
    Then the counter resets
    Given a dev agent declares an impasse before the limit
    Then the same pause and handoff occur

  Scenario: the architect directs but never codes
    Given an impasse before the SA agent
    When the SA resolves it
    Then a durable Intervention row records the trigger, the findings handed over, the direction, and its lifecycle state
    And resume and recovery read that row, never transient state
    And no commit authored by the SA appears in the workspace

  Scenario: scope changes escalate to the operator
    Given an impasse whose resolution would change the ticket's acceptance criteria or the spec
    Then the SA does not decide it and escalates to the operator naming the change
    When the operator approves a spec change
    Then cancelling is recorded durably on the paused run before any external call
    And the run's sutra ticket is conditionally transitioned out of in-progress to "deferred", never to "open"
    And an agent popping in the window before the supersession fence cannot claim the obsolete ticket — deferred is not poppable
    And only then is the run stamped cancelled — terminal, before replacement work proceeds
    And the cancelled run never resumes, merges, or resubmits
    And retirement classifies the released ticket as released work, never issued work that will finish
    And the change enters the pipeline through re-intake and supersession
    And no ticket or spec content is changed through a side channel

  Scenario: direction resumes the loop and survives in history
    Given the SA recorded a direction for a paused run
    When the run resumes
    Then the pair loop continues with the direction in the dev agent's context
    And the Intervention row preserves the trigger, direction, and eventual outcome through resume, restart, and recovery
