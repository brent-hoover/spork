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

  Scenario: pop wins the race over a conditional defer
    Given issue SUT-1 is assigned to "claude" with status "open"
    And a pop by "claude" claims SUT-1 first
    When a transition to "deferred" with expected status "open" is attempted
    Then it is rejected with a conflict
    And SUT-1 still has status "in-progress"

  Scenario: conditional defer wins the race over a pop
    Given issue SUT-1 is assigned to "claude" with status "open"
    And a transition to "deferred" with expected status "open" lands first
    When "claude" pops its work stack
    Then it receives an explicit empty result
    And SUT-1 still has status "deferred"

  Scenario: simultaneous writers mutate exactly once
    Given issue SUT-1 is assigned to "claude" with status "open"
    And two clients have each read status "open" client-side, before either server operation starts
    When a pop by "claude" and a conditional transition to "deferred" expecting "open" are released together at a synchronization barrier so the server executes them overlapping
    Then exactly one mutation is applied to SUT-1
    And exactly one status event is recorded for SUT-1
    And the losing operation receives an explicit conflict response reflecting the winner's committed status, never its stale client-side read

  Scenario: unknown statuses are rejected
    When issue SUT-1 is set to status "someday"
    Then the operation is rejected
