Feature: Threads tied to work

  Scenario: threads anchor to their work
    Given a thread imported anchored only to project "SUT"
    When it is tied to issue SUT-1
    Then SUT-1 lists the thread
    When it is retargeted to project "SUT"
    Then "SUT" lists the thread and SUT-1 no longer does
