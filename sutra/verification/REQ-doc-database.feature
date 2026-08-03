Feature: Doc database
  Planning artifacts live with the work they describe.

  Scenario: doc files under its project
    Given project "SUT" and issue SUT-1 exist
    When document "design" is created in "SUT" tied to SUT-1
    Then it is listed under "SUT" and under SUT-1
    And reading it returns title and content

  Scenario: saves append immutable versions
    Given document "design" has one version
    When new content is saved by "claude"
    Then version 2 exists with author "claude" and a timestamp
    And version 1 is still readable and unchanged

  Scenario: latest by default
    Given document "design" has three versions
    When the document is requested without a version
    Then version 3's content is returned

  Scenario: history lists and diffs versions
    Given document "design" has three versions
    When its history is requested
    Then all versions are listed in order with author and time
    When versions 1 and 3 are compared
    Then a diff of their content is returned
