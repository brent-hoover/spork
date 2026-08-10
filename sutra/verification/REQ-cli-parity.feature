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
  #
  # No project is seeded, deliberately. With "SUT" on the server, SUT-x and
  # SUT-0 read the same whether the shape check runs before the lookup or
  # after it; with no such project, a check that ran second would answer
  # about a project instead of about the reference.
  Scenario Outline: a reference that cannot name an issue is refused on its shape
    When "sutra issue show" is given the reference <reference>
    Then it is refused, naming <reference> and "<complaint>"

    Examples:
      | reference | complaint                     |
      | SUT1      | must be KEY-N                 |
      | -1        | must be KEY-N                 |
      | SUT-      | must be KEY-N                 |
      | SUT-x     | must end in a positive number |
      | SUT-0     | must end in a positive number |

  # `sutra api` is what backs the coverage promise above for every
  # operation with no bespoke wrapper — and every invocation the suite ever
  # made was a bare operation id on a parameterless read that succeeded.
  # Nothing had ever been put THROUGH it: not a query, not a body, not a
  # refusal. A command that exists but drops half the request covers the
  # operation on paper only.
  Scenario: the generic invoker carries the whole request
    Given project "SUT" has two issues
    When "sutra api listIssues" is run for "SUT" with --query.number 2
    Then only the second issue is listed, on a line of its own
    When "sutra api createIssue" is run for "SUT" with a body carrying a title
    Then the issue is created with that title
    When "sutra api createIssue" is run for "SUT" with a body carrying no title
    Then it exits nonzero and reports the status the server refused it with
