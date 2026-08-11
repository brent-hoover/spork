Feature: Identities without auth
  Plain strings, no auth model — single-user system.

  # Both kinds, because the criterion names two and one identity discharges
  # neither the choice nor the roster it lands in: the roster is ordered by
  # handle, and at cardinality one a listing that streams every row and one
  # that stops after the first hand back the same bytes.
  Scenario: identity is just a named kind
    When identity "claude" is created with kind "agent"
    And identity "human-brent" is created with kind "human"
    Then "claude" exists with kind "agent"
    And "human-brent" exists with kind "human"
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

  # One rule, three roads. An identifier arrives in a body field, in a path
  # segment, or in a query filter, and each road is different code: the
  # scenario above only ever drove the first. A road that skips the check
  # does not fail loudly — it answers about an entity that is not there,
  # for an id that is, and a filter's version of that answer is an empty
  # listing that looks like a legitimate result.
  Scenario Outline: every road into the server refuses a non-canonical identifier
    Given an existing identity id rendered in uppercase
    When that id is supplied <road>
    Then the request is refused for the identifier's form

    Examples:
      | road                                       |
      | as the author of a new comment             |
      | as the identity in a work-stack pop path   |
      | as the assignee filter on a listing        |
      | as the actor of an assignment              |
      | as the doc version of a new review         |
      | as the template of a new document          |
      | as the label attached to an issue          |

  # An empty filter is not an absent one: `?issue=` supplied an identifier
  # and it is not one. Read as absence, it widens the listing from one
  # issue's reviews to every review on the server — and that is an answer,
  # which nobody goes looking for the cause of.
  Scenario: a supplied but empty identifier filter is refused
    When reviews are listed with an empty issue filter
    Then the request is refused for the identifier's form

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
