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

  Scenario: thread content is searchable
    Given a thread containing the phrase "race condition in pop"
    When threads are searched for "race condition"
    Then that thread is returned with surrounding context
