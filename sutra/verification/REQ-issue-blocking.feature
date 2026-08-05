Feature: Blocking relationships

  Scenario: both sides see the block
    Given issues SUT-1 and SUT-2 exist
    When SUT-1 is marked as blocking SUT-2 by "human-brent"
    Then SUT-1 shows "blocks SUT-2"
    And SUT-2 shows "blocked by SUT-1"
    And an "issue.relation-added" event with actor "human-brent" and a timestamp is recorded for SUT-1
    When the relationship is removed by "human-brent"
    Then neither side shows it
    And an "issue.relation-removed" event with actor "human-brent" and a timestamp is recorded for SUT-1

  Scenario: blocker completion unblocks
    Given SUT-2 is blocked by open issue SUT-1, both assigned to "claude"
    And SUT-1 has an approved review
    When SUT-1 is transitioned to "complete" naming that review at its approved revision
    Then popping "claude"'s stack can return SUT-2

  Scenario: cycles are rejected
    Given SUT-1 blocks SUT-2 and SUT-2 blocks SUT-3
    When SUT-3 is marked as blocking SUT-1
    Then the operation is rejected as a cycle
