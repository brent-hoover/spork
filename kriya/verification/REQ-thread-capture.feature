Feature: Thread capture
  Every agent conversation that produced code is preserved in sutra's
  thread catalog — session-stamped, tied to its ticket, and reachable
  by search. Audit trail and learning-loop fuel in one.

  Scenario: transcripts land in the thread catalog
    Given a run with session id "sess-42" working ticket "KRI-7"
    When the run ends
    Then its dev-agent transcript is imported into sutra's thread catalog
    And the thread is stamped with "sess-42" and tied to "KRI-7"

  Scenario: no outcome loses its transcript
    Given runs that ended completed, retired, and crashed
    Then each run's transcript is imported
    And recovery imports whatever transcript exists for the crashed run

  Scenario: an import crash recovers to exactly one thread
    Given the import key and transcript reference were persisted with state "importing" and the crash hit before sutra accepted the import
    When recovery replays the persisted request under the same key
    Then sutra creates the thread and exactly one thread exists for the session
    Given sutra accepted the import but the crash hit before the thread id was recorded
    When recovery replays under the same key
    Then sutra returns the original thread and the recorded id matches it
    And exactly one thread exists for the session

  Scenario: a resumed run continues under a fresh session
    Given a run crashed mid-session and recovery terminated the DevSession, importing its transcript
    When the run resumes
    Then it continues under a fresh DevSession with its own session id and import key
    And post-recovery conversation imports as the new session's thread
    And the crashed session's thread is never appended to or overwritten

  Scenario: threads surface in session search
    Given a thread imported from session "sess-42" that also produced a review
    When sutra is searched by session id "sess-42"
    Then the thread and the review both appear, linked to their issue
