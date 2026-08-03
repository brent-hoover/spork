Feature: Import and export

  Scenario: export captures the whole project
    Given project "SUT" has identities, issues, comments, docs with versions, threads, reviews, and events
    When "SUT" is exported
    Then the export contains every one of those records

  Scenario: import round-trips losslessly
    Given an export of project "SUT"
    When it is imported into an empty server
    Then the project's content matches the original
    And every record keeps its original UUID

  Scenario: colliding import is rejected whole
    Given project "SUT" already exists on the server
    When the same export is imported again
    Then the import is rejected naming the conflicting records
    And no records were partially written
