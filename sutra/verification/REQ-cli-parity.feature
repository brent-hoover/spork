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

  # Every reference the suite ever wrote was SUT-1, where the first hyphen
  # and the last are the same hyphen and the split rule decides nothing.
  Scenario: an issue reference splits at the last hyphen
    Given project "MY-PROJ" exists with one issue
    When "sutra issue show" is given the reference MY-PROJ-1
    Then it shows that issue

  # Each row is a way the alias can fail to name anything. They must be
  # caught here, on shape: past this point the key becomes a lookup, and a
  # reference the caller mistyped comes back as a complaint about a
  # project they never named.
  Scenario Outline: a reference that cannot name an issue is refused on its shape
    Given project "SUT" exists
    When "sutra issue show" is given the reference <reference>
    Then it is refused, naming <reference> and "<complaint>"

    Examples:
      | reference | complaint                     |
      | SUT1      | must be KEY-N                 |
      | -1        | must be KEY-N                 |
      | SUT-      | must be KEY-N                 |
      | SUT-x     | must end in a positive number |
      | SUT-0     | must end in a positive number |
