Feature: Identities without auth
  Plain strings, no auth model — single-user system.

  Scenario: identity is just a named kind
    When identity "claude" is created with kind "agent"
    Then it exists with handle "claude" and kind "agent"
    And creating another "claude" is rejected as a duplicate

  Scenario: unknown handles are rejected
    Given no identity "claud" exists
    When issue SUT-1 is assigned to "claud"
    Then the operation is rejected
    And no identity "claud" was created
