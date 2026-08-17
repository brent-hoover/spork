Feature: Thread catalog
  Agent conversations are work artifacts worth indexing and citing.

  Scenario: import preserves the transcript
    Given a session transcript file from agent "claude" with session "sess-42"
    When it is imported as "parser debugging session"
    Then a thread exists with that title, session "sess-42", and an import time
    And its content matches the file verbatim

  Scenario: threads read like conversations
    Given an imported thread
    When it is opened in the web UI
    Then it renders turn by turn with speakers distinguished

  # Two matches, because "matching threads" is a set: at cardinality one a
  # search that streams every hit and one that stops after the first return
  # the same bytes. Results are ordered by import time, so the second import
  # is exactly the row a truncating search drops. The third thread is what
  # keeps the assertion from also passing for a search that ignores the
  # query and hands back the whole catalog.
  Scenario: thread content is searchable
    Given a thread containing the phrase "race condition in pop"
    And another thread containing the phrase "race condition in claim"
    And a thread that mentions neither
    When threads are searched for "race condition"
    Then each matching thread is returned with surrounding context
