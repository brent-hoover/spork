Feature: Review lifecycle
  Agents prepare a Review — branch or document — and humans read,
  discuss, and pass verdict in the web UI. Sutra records state and
  publishes events; acting on them is the subscriber's job.

  Scenario: agent submits a review
    Given agent "claude" finished work on issue SUT-1 on branch "sut-1-fix" at commit "a1b2c3d"
    When it creates a review for SUT-1 with deliverable branch "sut-1-fix" pinned at commit "a1b2c3d" and session "sess-42"
    Then the review exists in state "open"
    And it is listed for reviewers

  Scenario: reviewer sees the deliverable
    Given an open review with deliverable branch "sut-1-fix" pinned at commit "a1b2c3d"
    When a human opens it in the web UI
    Then the deliverable content is shown for reading
    And later commits pushed to "sut-1-fix" do not change the reviewed content

  Scenario: feedback threads on the review
    Given an open review
    When a human comments and another replies
    Then both comments anchor to the review
    And the reply is threaded under the first comment

  Scenario: verdict changes state and is recorded
    Given an open review
    When a human sets it to "changes-requested"
    Then the review state is "changes-requested"
    And an event records the actor and time

  Scenario: approval publishes an event
    Given an open review for issue SUT-1
    When a human sets it to "approved"
    Then an event of kind "review.approved" referencing the review and SUT-1 is published

  Scenario: rework routes back with session context
    Given a review created with session "sess-42" is set to "changes-requested"
    Then the emitted event carries session "sess-42" and the issue ref
    When the agent resubmits the deliverable pinned at commit "e4f5a6b"
    Then the review returns to state "open"
    And the review's pinned commit is "e4f5a6b"
