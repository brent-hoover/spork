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

  Scenario: threads surface in session search
    Given a thread imported from session "sess-42" that also produced a review
    When sutra is searched by session id "sess-42"
    Then the thread and the review both appear, linked to their issue
