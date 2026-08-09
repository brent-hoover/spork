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

  # Every scenario above writes its flag last, which is the one position
  # that asks nothing of the parser. A format flag ahead of its positional
  # must not swallow it, and a value-taking flag written last has nothing
  # after it to take — so the end of the line is a boundary the parser has
  # to notice rather than read past.
  Scenario: flags read the same wherever they sit on the line
    Given issue SUT-1 exists with title, status, assignee, and labels
    When "sutra issue show --json SUT-1" is run
    Then the output parses as JSON
    And it contains every field of SUT-1
    When "sutra issue list --project" is run with nothing after the flag
    Then it reports no project keyed "true", having taken the flag as boolean

  Scenario: output shape matches the api contract
    Given the API contract defines the issue schema
    When "sutra issue show SUT-1 --json" is run
    Then the output validates against the contract's issue schema
