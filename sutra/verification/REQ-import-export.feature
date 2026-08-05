Feature: Import and export

  Scenario: export captures the whole project
    Given project "SUT" has identities, issues, comments, docs with versions, threads, reviews — including one with a consumed approval — and events
    When "SUT" is exported
    Then the export contains every one of those records

  Scenario: import round-trips losslessly
    Given an export of project "SUT"
    When it is imported into an empty server
    Then the project's content matches the original, excluding the import audit event
    And every record keeps its original UUID
    And the consumed review's consumption stamp and revision survive the round-trip

  Scenario: a half-consumed review is rejected at import
    Given an export payload whose review carries a consumption stamp without its consumed revision
    When it is imported
    Then the import is rejected as malformed and nothing is created

  Scenario: a malformed close-used review is rejected at import
    Given an export payload whose review is close-used but carries no consumption fields
    When it is imported
    Then the import is rejected as malformed and nothing is created
    Given an export payload whose close-used review is in state "changes-requested" or whose consumed revision lags its current revision
    When it is imported
    Then the import is rejected as malformed — a spent review's verdict cannot be rewritten through import

  Scenario: an invariant-violating hierarchy is rejected at import
    Given an export payload containing a complete parent whose deferred child hides a descendant in status "open"
    When it is imported
    Then the import is rejected whole as malformed — imported state gets no cascade to repair it
    And nothing is written

  Scenario: colliding import is rejected whole
    Given project "SUT" already exists on the server
    When the same export is imported again
    Then the import is rejected naming the conflicting records
    And no records were partially written

  Scenario: unknown import actor is rejected
    Given an export of project "SUT"
    And identity "outsider" exists on the server but not in the export's identities
    When the export is imported with "outsider" as the actor
    Then the import is rejected as a bad request
    And nothing is written
