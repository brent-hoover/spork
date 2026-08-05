Feature: Closing requires an approved review
  Work — code or docs, docs especially — is not done until a human
  has approved the deliverable.

  Scenario: approved review allows close
    Given issue SUT-1 has a review of branch "sut-1-fix" pinned at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4" in state "approved"
    When SUT-1 is transitioned to "complete" naming that review at its approved revision
    Then the transition succeeds
    And the recorded approval names commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"
    And the review is stamped close-used and its verdict frozen in the same transaction
    And a later verdict on that review is rejected with no mutation

  Scenario: a stale revision cannot authorize a close
    Given issue SUT-7 has a review approved at revision 2 after an earlier revision was reviewed
    When SUT-7 is transitioned to "complete" naming that review at revision 1
    Then the transition is rejected with a conflict
    And the approval is not consumed and SUT-7 is not complete

  Scenario: a reversed approval cannot authorize a close
    Given issue SUT-2 has an approved review whose verdict is reversed to "changes-requested" before the close commits
    When SUT-2 is transitioned to "complete" naming that review
    Then the transition is rejected naming the missing approval

  Scenario: a merge-consumed approval still closes its issue
    Given issue SUT-3 has an approved review whose approval was consumed by a subscriber before merging
    When SUT-3 is transitioned to "complete" naming that review
    Then the transition succeeds — verdict consumption is a fence, not a spend

  Scenario: a spent review cannot close a reopened issue
    Given issue SUT-4 closed under its approved review and was later reopened
    When SUT-4 is transitioned to "complete" naming the same review
    Then the transition is rejected — the review is already close-used
    Given a fresh review of SUT-4 is approved
    When SUT-4 is transitioned to "complete" naming the fresh review
    Then the transition succeeds

  Scenario: naming the wrong review cannot close
    Given issue SUT-5 has a changes-requested review and issue SUT-6 has an approved one
    When SUT-5 is transitioned to "complete" naming its changes-requested review
    Then the transition is rejected
    When SUT-5 is transitioned to "complete" naming SUT-6's review
    Then the transition is rejected — the review belongs to another issue

  Scenario: no approval no close
    Given issue SUT-1 has no review in state "approved"
    When a transition of SUT-1 to "complete" is attempted
    Then it is rejected
    And the error names the missing approval

  Scenario: doc deliverables gate like code
    Given issue SUT-2 has a review whose deliverable is a document version
    And that review is in state "approved"
    When SUT-2 is transitioned to "complete" naming that review at its approved revision
    Then the transition succeeds
