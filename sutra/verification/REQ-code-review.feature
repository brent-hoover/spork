Feature: Review lifecycle
  Agents prepare a Review — branch or document — and humans read,
  discuss, and pass verdict in the web UI. Sutra records state and
  publishes events; acting on them is the subscriber's job.

  Scenario: agent submits a review
    Given agent "claude" finished work on issue SUT-1 on branch "sut-1-fix" at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"
    When it creates a review for SUT-1 with deliverable branch "sut-1-fix" pinned at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4" and session "sess-42"
    Then the review exists in state "open"
    And it is listed for reviewers

  Scenario: reviewer sees the deliverable
    Given an open review with deliverable branch "sut-1-fix" pinned at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"
    When a human opens it in the web UI
    Then the deliverable content is shown for reading
    And later commits pushed to "sut-1-fix" do not change the reviewed content

  Scenario: pinned base survives default branch movement
    Given a code submission for SUT-1 with base_commit "b1b2b3b4b1b2b3b4b1b2b3b4b1b2b3b4b1b2b3b4" and commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"
    When new commits are merged onto the project's default branch
    Then the submission's stored base_commit is still "b1b2b3b4b1b2b3b4b1b2b3b4b1b2b3b4b1b2b3b4"
    And the rendered diff is identical to before the default branch moved

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

  Scenario: verdicts can be revised for the current revision
    Given a review approved at its current revision
    When a human sets it to "changes-requested" carrying the same revision
    Then the review state is "changes-requested"
    And a verdict event is recorded
    When a human sets it to "approved" carrying the same revision
    Then the review state is "approved"
    And another verdict event is recorded

  Scenario: approval publishes an event
    Given an open review for issue SUT-1
    When a human sets it to "approved"
    Then an event of kind "review.approved" referencing the review and SUT-1 is published

  Scenario: rework routes back with session context
    Given a review created with session "sess-42" is set to "changes-requested"
    And a reviewer commented on the review before resubmission
    When the agent resubmits the deliverable pinned at commit "e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1"
    Then the review returns to state "open"
    And the review's pinned commit is "e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1"
    And the review's revision is 2
    And the earlier comment remains associated with revision 1
    And the emitted event carries session "sess-42" and the issue ref
    And revision 1's submission still exists and resolves to its original deliverable
    And revision 2's submission exists and carries the deliverable pinned at commit "e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1"

  Scenario: stale feedback is rejected
    Given a review at revision 1 open in a reviewer's browser
    When the agent resubmits the deliverable, advancing the review to revision 2
    And the reviewer submits an approval carrying revision 1
    Then the verdict is rejected
    And the review remains unapproved

  Scenario: stale comments are rejected
    Given a review at revision 1 rendered for a reviewer
    When the agent resubmits the deliverable, advancing the review to revision 2
    And the reviewer submits a comment carrying revision 1
    Then the comment is rejected
    And no comment is created

  Scenario Outline: unresolvable repository rejects submission
    Given a project with <failure>
    When an agent <operation>s a code deliverable for SUT-1
    Then the <operation> is rejected with a conflict
    And no new submission, review, or event results
    And any pre-existing review's state, revision, deliverable, submissions, and events are unchanged

    Examples:
      | operation | failure                       |
      | create    | an unset repo_path            |
      | resubmit  | an unset repo_path            |
      | create    | inaccessible repository path  |
      | create    | unknown commit                |
      | create    | unavailable merge base        |
      | resubmit  | inaccessible repository path  |
      | resubmit  | unknown commit                |
      | resubmit  | unavailable merge base        |
