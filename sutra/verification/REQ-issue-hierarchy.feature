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
    Given SUT-1 has an approved review and a child in status "in-progress"
    When SUT-1 is transitioned to "complete" naming its approved review at its current revision
    Then the transition is rejected naming the open child, not the review gate
    Given the child moves to status "deferred"
    When SUT-1 is transitioned to "complete" naming its approved review at its current revision
    Then the transition succeeds — deferred children are parked, not open
    Given a deferred child of SUT-1 itself has a descendant in status "open"
    When SUT-1 is transitioned to "complete" naming a fresh approved review at its current revision
    Then the transition is rejected naming the active descendant — deferred nodes cannot hide active work

  Scenario: subtree revision fences history, not just state
    Given SUT-1's subtree_revision is 7 as observed by a caller
    And a child of SUT-1 reopens and recompletes — two API transactions, each moving SUT-1's revision exactly once, to 9
    And SUT-1 has an unspent approved review named, with its current revision, on every attempt
    When SUT-1 is transitioned to "complete" with expected_subtree_revision 7
    Then the transition is rejected with a conflict — current state matches but history moved
    When the caller re-reads and retries with expected_subtree_revision 9
    Then the transition succeeds under its ordinary gates
    Given a complete-free hierarchy where nothing can reopen
    When a child is attached beneath a parent
    Then subtree_revision still increments on the parent and every ancestor

  Scenario: detaching a child cannot leave a stale close fence
    Given parent SUT-50 has an approved review and no other descendant, so it is otherwise closable
    And a caller observed SUT-50's subtree_revision while its child SUT-51 was complete
    When SUT-51 is detached from SUT-50 and then reopened
    Then the removal incremented SUT-50's subtree_revision at detach time
    And completing SUT-50 with the previously observed expected_subtree_revision is rejected with a conflict naming the revision, not the review gate

  Scenario: reopening a child reopens a complete parent
    Given SUT-1 is "complete" and its child SUT-2 is "complete"
    When SUT-2 is reopened to "open"
    Then SUT-1 returns to "open" in the same transaction
    And a status event is recorded for both SUT-1 and SUT-2

  Scenario: a nested reopen cascades to every complete ancestor
    Given a fresh hierarchy where grandparent SUT-10, parent SUT-11, and leaf SUT-12 are all "complete"
    When SUT-12 is reopened to "open"
    Then SUT-11 and SUT-10 both return to "open" in the same transaction
    And a status event is recorded for each reopened ancestor

  Scenario: a deferred child activating reopens its complete parent
    Given a fresh hierarchy where SUT-20 is "complete" with a child SUT-21 in status "deferred"
    When SUT-21 transitions to "in-progress"
    Then SUT-20 reopens in the same transaction

  Scenario: a child becoming blocked reopens its complete parent
    Given a fresh hierarchy where SUT-25 is "complete" with a child SUT-26 in status "deferred"
    When SUT-26 transitions to "blocked"
    Then SUT-25 reopens in the same transaction — blocked is active work

  Scenario: a blocked descendant holds the parent open
    Given a fresh hierarchy where SUT-27 has an approved review and a descendant in status "blocked" beneath a deferred child
    When SUT-27 is transitioned to "complete" naming its approved review at its current revision
    Then the transition is rejected naming the blocked descendant

  Scenario: attaching an open child reopens a complete parent
    Given a fresh issue SUT-30 in status "complete" and an unrelated issue SUT-31 in status "open"
    When SUT-31 is attached as a child of SUT-30
    Then SUT-30 reopens in the same transaction

  Scenario: an attached deferred subtree carrying active work reopens the parent
    Given a fresh issue SUT-40 in status "complete"
    And a deferred issue SUT-41 whose own subtree contains an issue in status "open"
    When SUT-41 is attached as a child of SUT-40
    Then SUT-40 reopens in the same transaction — a deferred root cannot hide active work it carries in
