Feature: System architect agent
  The escape valve when dev and review cannot converge. The architect
  directs but never codes; scope decisions always rise to the operator;
  every impasse and its resolution survive in the run's history.

  Scenario: an impasse pauses the run and reaches the architect
    Given a pair loop that has run its bounded rounds without progress
    Then the run pauses and the impasse reaches the SA agent
    And the SA receives the findings history, the ticket's acceptance criteria, and the bound snapshot
    Given a dev agent declares an impasse before the bound
    Then the same pause and handoff occur

  Scenario: the architect directs but never codes
    Given an impasse before the SA agent
    When the SA resolves it
    Then the resolution is a recorded direction on the run
    And no commit authored by the SA appears in the workspace

  Scenario: scope changes escalate to the operator
    Given an impasse whose resolution would change the ticket's acceptance criteria or the spec
    Then the SA does not decide it and escalates to the operator naming the change
    When the operator approves a spec change
    Then the paused run is cancelled through a durable, terminal BuildRun transition before replacement work proceeds
    And the cancelled run never resumes, merges, or resubmits
    And the change enters the pipeline through re-intake and supersession
    And no ticket or spec content is changed through a side channel

  Scenario: direction resumes the loop and survives in history
    Given the SA recorded a direction for a paused run
    When the run resumes
    Then the pair loop continues with the direction in the dev agent's context
    And the impasse, the direction, and the outcome are all readable in the run's history
