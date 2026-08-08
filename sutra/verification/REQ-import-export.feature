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

  Scenario: a malformed consumed review is rejected at import
    Given an export payload whose consumed review — not close-used — is in state "changes-requested"
    When it is imported
    Then the import is rejected as malformed — consumption fences the verdict, in imports as in the API
    Given an export payload whose consumed review's consumed revision lags its current revision
    When it is imported
    Then the import is rejected as malformed and nothing is created

  Scenario: a resubmission-superseded verdict is rejected at import
    Given an export payload whose review carries a verdict superseded by a later resubmission
    When it is imported
    Then the import is rejected as malformed and nothing is created

  Scenario: a verdict-bearing review without its latest verdict event is rejected at import
    Given an export payload whose approved review carries no latest verdict event
    When it is imported
    Then the import is rejected as malformed — such a review could never be closed, consumed, or resubmitted
    Given an export payload whose review names a latest verdict event that is not its actual latest
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

  # A repeated property is the one input that could store content no
  # export could faithfully reproduce: a decoder keeping the last value
  # and one keeping the first read the same bytes differently. Depth
  # matters as much as the top level — a transcript is stored exactly
  # as it arrived, so an ambiguity buried in it is served back forever.
  Scenario Outline: an ambiguous body never enters the system
    When a thread transcript repeating "<property>" <where> is imported
    Then the import is rejected as malformed, naming "<property>"
    And no thread was created

    Examples:
      | property | where                         |
      | speaker  | at the transcript's top level |
      | speaker  | inside a nested object        |
      | speaker  | inside an object in an array  |

  # The POST /threads door already refuses a thread with no transcript;
  # import must refuse the same thing, or the one path that writes
  # threads without going through that door becomes the way around it.
  Scenario: a thread without its transcript is rejected at import
    Given an export payload whose thread carries no transcript
    When it is imported
    Then the import is rejected as malformed and nothing is created

  # A review comment is pinned to the revision it was written against —
  # the API refuses to anchor one to any other. Import writes comments
  # and reviews from the same payload, so nothing outside it constrains
  # that pin: unchecked, an import can seat a comment on a revision the
  # review never had, and it renders forever against work never written.
  Scenario: a comment naming a nonexistent review revision is rejected at import
    Given an export payload whose comment names a review revision the review never had
    When it is imported
    Then the import is rejected as malformed and nothing is created

  # A work stack's order is internal state the export never carries as a
  # field of its own, so import has to reconstruct each assignee's queue
  # position. Reconstruct it wrong and the queue silently falls back to
  # issue-number order: every record round-trips intact, the import
  # reports success, and the next agent to pop gets the wrong issue.
  Scenario: an imported work stack pops in its original order
    Given an export of project "SUT" whose work stack was filled against issue-number order
    When it is imported into an empty server
    Then the imported work stack pops in the order the issues were assigned

  # Display numbers are project-scoped and unique. Import writes them
  # verbatim but they are minted from a sequence the payload never
  # carries, so unless that sequence is carried over too, the next
  # create restarts at 1 and collides with an issue the import wrote.
  Scenario: issue numbering continues past an import
    Given an export of project "SUT"
    When it is imported into an empty server
    And an issue is created on the imported project
    Then it gets a number no imported issue already holds

  Scenario: unknown import actor is rejected
    Given an export of project "SUT"
    And identity "outsider" exists on the server but not in the export's identities
    When the export is imported with "outsider" as the actor
    Then the import is rejected as a bad request
    And nothing is written
