Feature: Closing requires an approved review
  Work — code or docs, docs especially — is not done until a human
  has approved the deliverable.

  Scenario: approved review allows close
    Given issue SUT-1 has a review of branch "sut-1-fix" pinned at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4" in state "approved"
    When SUT-1 is transitioned to "complete"
    Then the transition succeeds
    And the recorded approval names commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"

  Scenario: no approval no close
    Given issue SUT-1 has no review in state "approved"
    When a transition of SUT-1 to "complete" is attempted
    Then it is rejected
    And the error names the missing approval

  Scenario: doc deliverables gate like code
    Given issue SUT-2 has a review whose deliverable is a document version
    And that review is in state "approved"
    When SUT-2 is transitioned to "complete"
    Then the transition succeeds
