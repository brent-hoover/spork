Feature: Status workflow

  Scenario: free transitions are recorded
    Given issue SUT-1 is "open"
    When it moves to "in-progress", then "deferred", then "blocked"
    Then each transition succeeds
    And each is recorded as an event with actor and time

  Scenario: complete can reopen
    Given issue SUT-1 is "complete" with an approved review
    When it is reopened
    Then its status is "open"
    And the reopening is recorded
    And the approved review remains in history

  Scenario: conditional transitions guard racing writers
    Given issue SUT-1 has status "in-progress"
    When a transition to "deferred" with expected status "open" is attempted
    Then it is rejected with a conflict
    And SUT-1 still has status "in-progress"

  Scenario: racing pop and defer resolve to one winner
    Given issue SUT-1 is assigned to "claude" with status "open"
    When a pop by "claude" and a transition to "deferred" with expected status "open" race
    Then exactly one operation mutates SUT-1
    And if the pop wins the deferral receives a conflict
    And if the deferral wins the pop receives an explicit empty result
    And SUT-1's status reflects only the winner's mutation

  Scenario: unknown statuses are rejected
    When issue SUT-1 is set to status "someday"
    Then the operation is rejected
