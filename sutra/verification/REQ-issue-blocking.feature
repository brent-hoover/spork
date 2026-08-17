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

  # Removal is routed under an issue, so the path issue carries meaning:
  # it asserts which issue the caller believes the relation touches. Both
  # ends see the block, so either must be able to clear it — and an issue
  # that is neither end must not, or the nesting is decoration and any
  # issue id in the path removes any relation in the system. Only the
  # blocked end tells those two apart: from the blocking end, a check that
  # examines one endpoint and a check that examines both agree.
  Scenario: a relation is removed from either end, and from nowhere else
    Given issues SUT-1 and SUT-2 exist
    And issue SUT-3 exists, involved in no relation
    When SUT-1 is marked as blocking SUT-2 by "human-brent"
    And removing that relation is attempted with SUT-3 as the path issue
    Then it is rejected as not found, naming the relation and SUT-3
    And SUT-1 shows "blocks SUT-2"
    When the relationship is removed from the blocked end by "human-brent"
    Then neither side shows it

  Scenario: blocker completion unblocks
    Given SUT-2 is blocked by open issue SUT-1, both assigned to "claude"
    And SUT-1 has an approved review
    When SUT-1 is transitioned to "complete" naming that review at its approved revision with its approval's verdict event
    Then popping "claude"'s stack can return SUT-2

  # A blocks relation may span two projects — it never cascades, so no walk
  # leaves one project for the other. parent_of may not, for exactly that
  # reason. What spanning costs is that one end can be archived while the
  # other stays live, and the request is routed under a single issue: guard
  # only the project named in the path and a live issue becomes a lever for
  # writing into a frozen one. Two projects are what tell the guards apart —
  # when both ends sit in one project, guarding one end and guarding both
  # agree on every input.
  Scenario: a cross-project block is guarded at both ends
    Given issue SUT-1 blocks issue OTH-1 in another project
    When "OTH" is archived after the block exists
    Then adding another block into "OTH" is rejected as read-only
    And removing the existing block from SUT-1's side is rejected as read-only
    And both sides still show the block

  Scenario: cycles are rejected
    Given SUT-1 blocks SUT-2 and SUT-2 blocks SUT-3
    When SUT-3 is marked as blocking SUT-1
    Then the operation is rejected as a cycle
