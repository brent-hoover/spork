Feature: Documents tied to issues

  Scenario: link and unlink after creation
    Given document "notes" exists in project "SUT" with no issue
    When "notes" is tied to issue SUT-1
    Then SUT-1 lists "notes"
    When "notes" is untied from SUT-1
    Then SUT-1 lists no documents

  Scenario: both sides see the link
    Given document "design" is tied to issue SUT-1
    When SUT-1's documents are requested
    Then "design" is listed
    And "design" shows SUT-1 as its issue
