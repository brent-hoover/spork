Feature: Threads tied to work

  Scenario: threads anchor to their work
    Given a thread imported anchored only to project "SUT"
    When it is tied to issue SUT-1 by "human-brent"
    Then SUT-1 lists the thread
    And a "thread.anchor-changed" event with actor "human-brent", a timestamp, and subject SUT-1 is recorded
    And that event's payload carries the old and new anchors
    When it is retargeted to project "SUT" by "human-brent"
    Then "SUT" lists the thread and SUT-1 no longer does
    And a "thread.anchor-changed" event with subject "SUT" and the old and new anchors in its payload is recorded
