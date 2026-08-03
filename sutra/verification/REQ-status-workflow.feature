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

  Scenario: unknown statuses are rejected
    When issue SUT-1 is set to status "someday"
    Then the operation is rejected
