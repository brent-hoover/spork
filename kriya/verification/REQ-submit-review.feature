Feature: Review submission and merge
  Finished work reaches the human as a sutra Review. The human verdict
  alone releases the merge, rework routes back to the session that did
  the work, and the ticket closes only through sutra's own gate.

  Scenario: a PO pass becomes a sutra Review
    Given a run that passed PO validation at head commit "C2"
    When kriya submits the work
    Then the commit-scoped submission key and state "submitting" are persisted before sutra is called
    And a sutra Review exists naming the ticket's branch pinned at "C2"
    And it is stamped with the run's session id
    And the state records "submitted" with the returned review id
    And the run waits on review events

  Scenario: a crash before sutra accepts the submission recovers to one review
    Given the submission key and state "submitting" were persisted and the crash hit before sutra accepted the request
    When recovery replays the persisted request under the same idempotency key
    Then sutra creates the review and exactly one review exists for the run

  Scenario: a crash after sutra accepts the submission recovers to one review
    Given sutra accepted the review but the crash hit before the id was recorded
    When recovery replays the persisted request under the same idempotency key
    Then sutra returns the original response and the recorded id matches the review sutra already holds
    And exactly one review exists for the run

  Scenario: rework routes back by session
    Given a review receives a changes-requested verdict
    Then the feedback reaches the run identified by the review's session id — the same agent instance where possible
    And the pair loop resumes
    When the agent finishes rework at a new head commit
    And every machine gate — review, test, structure, typing, arch, branch coverage, mutation — and PO validation pass again at the new head commit
    Then resubmission goes through sutra's resubmit path and the review's revision increments
    And no resubmission happens before those gates pass

  Scenario: approval merges exactly once
    Given a review enters approved and the event is published
    And the branch head still equals the approved current revision's pinned commit
    When kriya consumes the event
    Then it re-reads the review before acting
    And what merges is that immutable pinned commit, never the mutable branch reference
    And the merge happens exactly once
    When the same approval event is replayed
    Then no second merge occurs

  Scenario: a moved head refuses the merge
    Given a review enters approved and the event is published
    And the branch head moved past the pinned commit before kriya processes the event
    When kriya consumes the event
    Then nothing merges and the unreviewed-commits mismatch surfaces

  Scenario: a reversed verdict refuses the merge
    Given a review enters approved and the event is published
    And the verdict is reversed to changes-requested before kriya processes the event
    When kriya consumes the event and re-reads the review
    Then no merge happens and the mismatch surfaces

  Scenario: merge completes the ticket through sutra
    Given the branch merged successfully
    When kriya terminates the run's dev session — the branch's only in-protocol writer — before the check
    And kriya re-reads the branch head and it still equals the merged commit
    And kriya transitions the ticket to complete
    Then the transition goes through sutra, which enforces its approved-review gate
    And the completed head commit is durably recorded
    And the run's workspace becomes eligible for cleanup

  Scenario: a pre-completion head advance returns the run to review
    Given the branch merged successfully
    And a commit landed on the branch before kriya's completion check
    When kriya re-reads the branch head
    Then the ticket is not completed and the run returns to the pair loop for the unreviewed commits
    And once the gates pass again the new head is submitted as a fresh review, durably recorded on the run — the approved review is never resubmitted
    And no work is stranded on a terminal run

  Scenario: a post-completion advance surfaces to the operator
    Given the ticket completed with its head commit durably recorded
    When an out-of-band commit lands on the branch afterward
    Then the advance is detected against the recorded head commit and surfaces to the operator with sutra's reopen path
