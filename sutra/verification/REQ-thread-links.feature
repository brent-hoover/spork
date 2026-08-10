Feature: Threads tied to work

  Scenario: threads anchor to their work
    Given a thread imported anchored only to project "SUT"
    When it is tied to issue SUT-1 by "human-brent"
    Then SUT-1 lists the thread
    And a "thread.anchor-changed" event with actor "human-brent", a timestamp, and subject SUT-1 is recorded
    And that event's payload carries the old and new anchors
    When it is retargeted to project "SUT" by "human-brent"
    Then "SUT" lists the thread and SUT-1 no longer does
    And a "thread.anchor-changed" event with actor "human-brent", a timestamp, subject "SUT", and the old and new anchors in its payload is recorded

  # Two ways an anchor can be wrong that the scenario above cannot see. An
  # anchor naming both a project and an issue has two halves that can
  # disagree, and the thread would end up governed by a project its own
  # anchor does not name. And a retarget touches TWO projects — the one it
  # leaves and the one it joins — so guarding only the destination would let
  # a thread walk out of an archived project while everything else in there
  # stayed frozen.
  Scenario: an anchor is guarded at both ends
    Given a thread imported anchored only to project "SUT"
    When it is anchored to project "SUT" and an issue in another project
    Then the anchor is refused as a bad request
    When "SUT" is archived and the thread is retargeted to the other project
    Then the anchor is refused as a conflict and the thread still belongs to "SUT"

  # The converse, and the half the case above cannot see: the project the
  # thread JOINS. There its own project is live, so the request arrives
  # routed under a writable project — a guard that asked only about that
  # one would move a thread into a frozen project without noticing.
  Scenario: a thread cannot be retargeted into an archived project
    Given a thread imported anchored only to project "SUT"
    When the other project is archived and the thread is retargeted into it
    Then the anchor is refused as a conflict and the thread still belongs to "SUT"
