Feature: CLI parity with the API
  Agents are first-class users; no capability is UI-only.

  Scenario: every api operation has a command
    Given the API contract's list of operations
    When the parity check maps operations to CLI commands
    Then every operation is covered by at least one command
    And the check fails if a new operation lacks one

  Scenario: remote server behaves like loopback
    Given a sutra server running on another host
    When each CLI command runs with --server pointing at it
    Then results are identical to running against the local daemon
