Feature: Review submission and merge
  Finished work reaches the human as a sutra Review. The human verdict
  alone releases the merge, rework routes back to the session that did
  the work, and the ticket closes only through sutra's own gate.

  Scenario: a PO pass becomes a sutra Review
    Given a run that passed PO validation at head commit "C2"
    When kriya submits the work
    Then a sutra Review exists naming the ticket's branch pinned at "C2"
    And it is stamped with the run's session id
    And the run waits on review events

  Scenario: rework routes back by session
    Given a review receives a changes-requested verdict
    Then the feedback reaches the run identified by the review's session id — the same agent instance where possible
    And the pair loop resumes
    When the agent finishes rework at a new head commit
    And the review, mutation, and PO-validation gates pass again at the new head commit
    Then resubmission goes through sutra's resubmit path and the review's revision increments
    And no resubmission happens before those gates pass

  Scenario: approval merges exactly once
    Given a review enters approved and the event is published
    When kriya consumes the event
    Then it re-reads the review before acting
    And what merges is the immutable commit pinned by the still-approved current revision, never the mutable branch reference
    And commits pushed to the branch after the check cannot change what lands
    And the merge happens exactly once
    When the same approval event is replayed
    Then no second merge occurs
    Given the verdict was reversed since the event was published
    Then no merge happens and the mismatch surfaces

  Scenario: merge completes the ticket through sutra
    Given the branch merged successfully
    When kriya transitions the ticket to complete
    Then the transition goes through sutra, which enforces its approved-review gate
    And the run's workspace becomes eligible for cleanup
