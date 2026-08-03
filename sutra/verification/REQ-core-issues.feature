Feature: Core issue tracking

  Scenario: create mints uuid and display number
    Given project "SUT" exists
    When an issue "fix the parser" is created in "SUT"
    Then it has a UUIDv7 id
    And its display number is the next sequential number in "SUT"
    And its status is "open"
    And a creation event is recorded

  Scenario: updates persist
    Given issue SUT-1 exists
    When its title is changed to "fix the lexer"
    Then reading SUT-1 shows the new title
    And its updated timestamp changed

  Scenario: labels attach and detach
    Given issue SUT-1 exists and label "bug" exists
    When "bug" is attached to SUT-1 and then detached
    Then SUT-1's label list reflects each change
    And an event of kind "issue.unlabeled" with actor and timestamp is recorded for SUT-1

  Scenario: assignment to any identity
    Given identities "human-brent" and "claude" exist
    When SUT-1 is assigned to "claude"
    Then SUT-1 shows assignee "claude"
    And SUT-1 is on "claude"'s work stack
    When SUT-1 is unassigned by "human-brent"
    Then SUT-1 shows no assignee
    And SUT-1 is no longer on "claude"'s work stack
    And an "issue.unassigned" event with actor "human-brent" and a timestamp is recorded for SUT-1
