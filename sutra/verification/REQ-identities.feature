Feature: Identities without auth
  Plain strings, no auth model — single-user system.

  Scenario: identity is just a named kind
    When identity "claude" is created with kind "agent"
    Then it exists with handle "claude" and kind "agent"
    And creating another "claude" is rejected as a duplicate

  Scenario: unknown identity ids are rejected
    Given an identity id that names no identity
    When issue SUT-1 is assigned to that identity id
    Then the operation is rejected
    And nothing is created

  # Identifiers are compared as case-sensitive text at every layer below
  # the API, so the contract fixes them at one spelling — canonical
  # lowercase — and the shape check at each door is what holds them
  # there. An existing id sent in uppercase is the only input that tells
  # that check from a lookup: unheld, it reaches the store, misses, and
  # the server answers that an identity which plainly exists names no
  # identity.
  Scenario: an identifier in non-canonical casing is refused for its form
    Given an existing identity id rendered in uppercase
    When issue SUT-1 is assigned to that identity id
    Then the operation is rejected for the identifier's form, not as an unknown identity
    And nothing is created

  Scenario Outline: uniqueness collisions name their colliding resource
    Given a request that would create <duplicate>
    When it is rejected with a conflict
    Then the error's code is "unique-violation" and conflicts names the colliding resource

    Examples:
      | duplicate                 |
      | a duplicate project key   |
      | a duplicate handle        |
      | a duplicate label name    |
      | a duplicate template name |

  Scenario: renaming a template into an existing name collides
    Given templates "daily-standup" and "retro" exist
    When "retro" is renamed to "daily-standup"
    Then the update is rejected with code "unique-violation"
    And conflicts names the existing "daily-standup" template's id
