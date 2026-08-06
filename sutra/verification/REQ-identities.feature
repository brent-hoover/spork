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
