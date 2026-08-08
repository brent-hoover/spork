Feature: Review lifecycle
  Agents prepare a Review — branch or document — and humans read,
  discuss, and pass verdict in the web UI. Sutra records state and
  publishes events; acting on them is the subscriber's job.

  Scenario: agent submits a review
    Given agent "claude" finished work on issue SUT-1 on branch "sut-1-fix" at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"
    When it creates a review for SUT-1 with deliverable branch "sut-1-fix" pinned at commit "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4" and session "sess-42"
    Then the review exists in state "open"
    And it is listed for reviewers

  # A review pins its deliverable to an immutable commit, and only a full
  # object id is one: an abbreviation names whatever it currently
  # disambiguates to, and a repository that grows can make that prefix name
  # a second object, or none. Git runs two object formats, so "full" is two
  # widths, and the pair is the whole rule — sample one and a check for
  # either width reads exactly like a check for that one. The two refusals
  # are what carry the distinction outward: a bad request means the id is
  # not an id, a conflict means it is one and this repository has no such
  # object.
  Scenario Outline: a deliverable is pinned only by a full object id
    Given a git-backed project and issue SUT-1
    When an agent creates a review pinned at <commit>
    Then the submission is <outcome>

    Examples:
      | commit                                  | outcome                            |
      | an unknown object id of the other width | refused as a conflict              |
      | a real object id cut to twelve digits   | refused as a bad request, naming the form |
      | a full-width string that is not hex     | refused as a bad request, naming the form |

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
    Then the emitted event carries session "sess-42" and the issue ref
    Given a reviewer commented on the review before resubmission
    When the agent resubmits the deliverable pinned at commit "e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1" under session "sess-43"
    Then the review returns to state "open"
    And the review's pinned commit is "e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1e4f5a6b1"
    And the review's revision is 2
    And submission 1 still carries session "sess-42" while submission 2 carries "sess-43"
    And the review's session mirrors the latest submission, "sess-43"
    Given a human sets the review to "changes-requested" carrying revision 2
    Then that event carries session "sess-43", the session of revision 2
    When the agent resubmits the deliverable pinned at commit "0123abcd0123abcd0123abcd0123abcd0123abcd" with no session
    Then submission 3 carries no session
    And the review returns to state "open" at revision 3
    And the review's pinned commit is "0123abcd0123abcd0123abcd0123abcd0123abcd"
    And the review's session is absent, mirroring the latest submission
    And submissions 1 and 2 retain "sess-42" and "sess-43"
    And the earlier comment remains associated with revision 1
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

  Scenario: approval consumption fences reversal
    Given an approved review at revision 2
    When a subscriber consumes the approval expecting revision 2 and naming its approval's verdict event
    Then the consumption is durably recorded and replaying it under the same idempotency key returns the original success
    And a "review.consumed" event records the actor, the review, and revision 2
    And a second subscriber's distinct consumption attempt against the consumed approval is rejected with a conflict
    And a later verdict on the review is rejected with no mutation
    Given an approved review whose verdict was reversed before consumption
    When the subscriber attempts consumption
    Then it fails with a conflict and consumes nothing
    Given a review that advanced to a newer approved revision since the subscriber's read
    When the subscriber attempts consumption expecting the previous revision
    Then it fails with a conflict and no consumption is stamped

  Scenario: a verdict ABA fences delayed operations
    Given a review whose verdict went changes-requested, then approved, then changes-requested again at the same revision
    When a delayed resubmission arrives naming the FIRST changes-requested event
    Then it is rejected with a conflict — that event is no longer the review's latest verdict

  Scenario: a delayed consumption naming a superseded approval conflicts even against approved state
    Given a review approved by event "A", reversed, then approved again by event "B" at the same revision
    When a delayed consumption arrives expecting verdict event "A"
    Then it is rejected with a conflict and consumes nothing — the current approved state does not excuse the superseded event

  Scenario: a delayed close naming a superseded approval conflicts even against approved state
    Given an issue whose review was approved by event "A", reversed, then approved again by event "B" at the same revision
    When a delayed complete transition arrives naming the review, its revision, and verdict event "A"
    Then it is rejected with a conflict — the review is not close-used and the issue is not complete

  Scenario: a stale resubmission cannot land on a newer revision
    Given a review at revision 3 in state "changes-requested"
    When a delayed resubmission arrives expecting revision 2, naming the changes-requested event it answers
    Then it is rejected with a conflict and no mutation

  Scenario: resubmission requires changes-requested
    Given an open review
    When the agent resubmits the deliverable
    Then the resubmission is rejected with a conflict
    And no submission, state change, or event results
    Given a review approved at its current revision
    When the agent resubmits the deliverable
    Then the resubmission is rejected with a conflict
    And no submission, state change, or event results

  Scenario Outline: both fences guard creation and resubmission
    Given a caller performs a <operation> expecting <field> "D1"
    And sutra resolves the actual <field> as "D2"
    When the request is processed
    Then it is rejected with a conflict — nothing is created or mutated, no new submission or event exists, and for a resubmission the existing review, revision, and verdict remain unchanged

    Examples:
      | operation    | field               |
      | submission   | expected base       |
      | submission   | expected default head |
      | resubmission | expected base       |
      | resubmission | expected default head |

  Scenario: a stale expected base rejects submission before the review exists
    Given a caller submits a code deliverable expecting base "D1"
    And sutra resolves the merge base as "D2"
    When the submission is processed
    Then it is rejected with a conflict and no review, submission, or event exists

  Scenario: a moved default head rejects submission even when the merge base is unchanged
    Given a caller submits a code deliverable expecting default head "D1"
    And the default branch fast-forwarded to "D2" while the merge base remains "D1"
    When the submission is processed
    Then it is rejected with a conflict and nothing is created — the fence is the head, not the merge base

  Scenario: resubmission enforces the same base and head fences
    Given a changes-requested review whose caller resubmits a code deliverable expecting base "D1"
    And sutra resolves the resubmission's merge base as "D0"
    When the resubmission is processed
    Then it is rejected with a conflict and no mutation
    Given the caller resubmits expecting default head "D1"
    And the default branch now stands at "D2"
    When the resubmission is processed
    Then it is rejected with a conflict and no mutation

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
