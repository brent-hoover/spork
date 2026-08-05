Feature: Issue hierarchy

  Scenario: parent and child see each other
    Given issues SUT-1 and SUT-2 exist
    When SUT-2 becomes a child of SUT-1 by "human-brent"
    Then SUT-1 lists SUT-2 among its children
    And SUT-2 shows SUT-1 as its parent
    And an "issue.relation-added" event with actor "human-brent" and a timestamp is recorded for SUT-1
    And making SUT-1 a child of SUT-2 is rejected as a cycle

  Scenario: parent rolls up child progress
    Given SUT-1 has five children of which three are complete
    When SUT-1 is viewed
    Then it shows progress "3 of 5 complete"

  Scenario: open children hold the parent open
    Given SUT-1 has a child in status "in-progress"
    When SUT-1 is transitioned to "complete"
    Then the transition is rejected naming the open child
    Given the child moves to status "deferred"
    When SUT-1 is transitioned to "complete" with an approved review
    Then the transition succeeds — deferred children are parked, not open

  Scenario: reopening a child reopens a complete parent
    Given SUT-1 is "complete" and its child SUT-2 is "complete"
    When SUT-2 is reopened to "open"
    Then SUT-1 returns to "open" in the same transaction
    And a status event is recorded for both SUT-1 and SUT-2
