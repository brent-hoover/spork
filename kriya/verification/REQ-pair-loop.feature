Feature: Pair-programming loop
  Every commit is reviewed as it lands. Rounds are enqueued through the
  write-ahead ReviewRound protocol, findings are fixed and recommitted,
  and the loop exits only when the branch head passes clean. Kriya
  never touches a review job it cannot prove it created.

  Scenario: every commit gets its own review round
    Given a dev agent working a ticket in its workspace
    When it lands two commits
    Then each commit has its own ReviewRound, written ahead of its enqueue
    And no end-stage review covering the accumulated branch replaces them

  Scenario: findings are fixed and recommitted until clean
    Given a round returns findings for commit "C1"
    Then the findings are handed to the dev agent
    When the agent fixes and lands commit "C2"
    Then a new round is enqueued for "C2"
    And the loop continues until a round on the branch head returns no findings

  Scenario: only provably owned jobs are touched
    Given a roborev job listed for the same commit that kriya did not record from its own enqueue return
    Then kriya never adopts, responds to, cancels, or closes it
    Given a claim older than the lease window
    Then it is never auto-reclaimed and surfaces to the operator per the claim-generation protocol

  Scenario: the exit is a clean pass on the head
    Given commit "C1" passed review but the branch head is now "C2"
    Then the pair loop is not satisfied
    When a round on "C2" returns no findings
    Then the loop exits and the round records verdict "pass" at "C2" — the durable record the gate chain reads

  Scenario: the review trail is complete
    Given a round's findings have been addressed
    When kriya closes the round
    Then its response is recorded on the job as a comment before the close
    And the job's history holds the full conversation
