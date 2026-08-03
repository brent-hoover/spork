Feature: Threads tied to work

  Scenario: threads anchor to their work
    Given an imported thread with no anchor
    When it is tied to issue SUT-1
    Then SUT-1 lists the thread
    When it is retargeted to project "SUT"
    Then "SUT" lists the thread and SUT-1 no longer does
