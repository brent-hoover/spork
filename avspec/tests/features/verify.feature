Feature: avspec verify
  The CI gate. Exit 0 iff the spec passes for its declared status.

  Scenario: a directory without a manifest fails
    Given an empty spec directory
    When I run avspec verify --json
    Then the exit code is 1
    And the report contains a "MANIFEST_MISSING" finding with severity "error"

  Scenario: a draft spec with only todos passes
    Given a minimal draft spec
    When I run avspec verify --json
    Then the exit code is 0
    And the report contains a "NO_REQUIREMENTS" finding with severity "todo"

  Scenario: a ready spec with todos fails
    Given a minimal spec with status "ready"
    When I run avspec verify --json
    Then the exit code is 1
