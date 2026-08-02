Feature: avspec next
  The interview's brain: the gap queue in authoring order.

  Scenario: the stack question comes first on an empty spec
    Given a minimal draft spec
    When I run avspec next --json
    Then the exit code is 0
    And the first finding code is "NO_STACK"
    And every finding has a question
