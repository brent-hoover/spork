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
    Given a deferred child of SUT-1 itself has a descendant in status "open"
    When SUT-1 is transitioned to "complete"
    Then the transition is rejected naming the active descendant — deferred nodes cannot hide active work

  Scenario: subtree revision fences history, not just state
    Given SUT-1's subtree_revision is 7 as observed by a caller
    And a child of SUT-1 reopens and recompletes, advancing SUT-1's subtree_revision to 9
    When SUT-1 is transitioned to "complete" with expected_subtree_revision 7
    Then the transition is rejected with a conflict — current state matches but history moved
    When the caller re-reads and retries with expected_subtree_revision 9
    Then the transition succeeds under its ordinary gates

  Scenario: reopening a child reopens a complete parent
    Given SUT-1 is "complete" and its child SUT-2 is "complete"
    When SUT-2 is reopened to "open"
    Then SUT-1 returns to "open" in the same transaction
    And a status event is recorded for both SUT-1 and SUT-2
    Given a complete grandparent above SUT-1
    When a nested child of SUT-1 reopens
    Then every complete ancestor up the chain reopens in the same transaction
    Given SUT-1 is "complete" with a child in status "deferred"
    When the deferred child transitions to "in-progress"
    Then SUT-1 reopens in the same transaction
    Given SUT-1 is "complete"
    When an open issue is attached as a child of SUT-1
    Then SUT-1 reopens in the same transaction
