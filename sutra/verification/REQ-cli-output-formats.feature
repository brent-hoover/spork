Feature: Machine-readable CLI output
  Agent-consumable output is a first-class contract, not a scrape.

  Scenario: json output is complete and valid
    Given issue SUT-1 exists with title, status, assignee, and labels
    When "sutra issue show SUT-1 --json" is run
    Then the output parses as JSON
    And it contains every field of SUT-1

  Scenario: yaml output matches json content
    Given issue SUT-1 exists
    When "sutra issue show SUT-1 --yaml" is run
    Then the output parses as YAML
    And its content equals the --json output for SUT-1

  Scenario: output shape matches the api contract
    Given the API contract defines the issue schema
    When "sutra issue show SUT-1 --json" is run
    Then the output validates against the contract's issue schema
