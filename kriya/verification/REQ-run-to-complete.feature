Feature: Run to complete
  A build runs until the current head plan's every ticket is complete.
  The finish line moves with supersession, idling is not exiting, and a
  stall is reported with its cause instead of spinning.

  Scenario: the loop runs until nothing is workable
    Given a head plan with workable tickets
    Then kriya keeps popping and building
    Given every remaining ticket is blocked or in flight
    Then kriya idles without exiting
    When a ticket unblocks
    Then popping resumes

  Scenario: completion never fires against a partial plan
    Given an activated head plan still mid-phase, without its completed stamp
    And every ticket created so far is complete
    When completion detection runs
    Then no completion attempt starts — the ticket set is not yet whole
    When decomposition passes its final assignment barrier and stamps completed
    Then completion detection arms

  Scenario: a rejected completion review reworks within its attempt
    Given a build-completion review receives changes-requested
    When kriya records review-resubmitting — persisting the revision-scoped resubmission key and the pending report reference before any external call
    And kriya regenerates the completion report
    Then the resubmission goes through sutra's resubmit path with a new document version
    And completion_report_doc and completion_revision update transactionally within the same attempt — key and epoch unchanged — and the pending fields reconcile
    And the epic closes on the new revision's approval, never stranding the target

  Scenario: an initial submission crash before the report document recovers exactly once
    Given review-submitting rotated the claim set with its doc key and pending report, and the crash hit before the document version existed
    When recovery runs
    Then the keyed doc mutation replays and creates the version, records it, and the keyed review creation follows
    And exactly one document version and one review exist

  Scenario: an initial submission crash between document and review recovers exactly once
    Given the report document was created under its key and the crash hit before the review-create call
    When recovery replays the keyed doc mutation
    Then sutra returns the same version — no second version appends
    And the keyed review creation then runs, exactly once

  Scenario: a rework crash before the document version recovers exactly once
    Given review-resubmitting was recorded with its keys and the crash hit before the new document version existed
    When recovery runs
    Then the keyed doc mutation replays and creates the version, and the keyed resubmission follows
    And exactly one document version and one resubmission exist

  Scenario: a rework crash between document and resubmission recovers exactly once
    Given the document version was created under its key and the crash hit before the review resubmission
    When recovery replays the keyed doc mutation
    Then sutra returns the same version — no second version appends
    And the keyed resubmission then runs, exactly once

  Scenario: a rework crash after resubmission recovers exactly once
    Given sutra accepted the resubmission and the crash hit before completion_report_doc and completion_revision updated
    When recovery replays the keyed resubmission
    Then sutra returns the original response and the fields reconcile from it
    And nothing double-submits and nothing is lost

  Scenario: completion is the head plan done and the epic closable
    Given the activated head plan bears its completed stamp
    And every ticket of the activated head plan is complete
    Then kriya records review-submitting with the deterministic submission key, the doc key, and the pending report reference before any sutra call
    And the keyed doc mutation creates the report document and its returned version is persisted before the review-create call
    And kriya submits a build-completion review on the umbrella epic with that recorded document version as its deliverable
    And a crash before the review id lands is recovered by replaying the persisted request under the same submission key — sutra returns the original review if it landed and creates it otherwise, never a second one
    And retired tickets resting deferred under the epic do not block its close
    When the human approves the review
    Then the epic close is attempted through sutra naming the attempt's completion review as its authorizer, under the closing state's persisted idempotency key, and sutra's no-open-children gate is the authoritative check
    And a crash after the close succeeds but before kriya records it recovers by replaying under the same key — sutra returns the original success, never a close-used conflict
    And after the close succeeds, completion is stamped on the BuildTarget row — a CAS requiring the current epoch to still equal the immutable claim epoch recorded at submission — and popping stops for that target

  Scenario: a reopen-and-recomplete before the event is consumed still fences
    Given a completion review was submitted with the epic's subtree revision recorded as 7
    And a child ticket reopens and recompletes before kriya consumes any reopen event, advancing the revision to 9
    When the approval event arrives and kriya attempts the epic close with expected subtree revision 7
    Then sutra rejects the close — history moved even though current state matches
    And kriya advances its epoch and a fresh attempt with fresh approval is required

  Scenario: a stale approval never stamps an unfinished target
    Given a build-completion review recorded with claim epoch 3
    And a supersession or a ticket reopen has since advanced the current epoch to 4
    When the approval event arrives
    Then the completion CAS fails, nothing is stamped, and the stale approval surfaces to the operator

  Scenario: a reopen between validation and stamping cannot slip through
    Given kriya's advisory read saw every head-plan ticket complete
    And a child ticket reopens in sutra before the epic close executes
    When kriya attempts the epic close
    Then sutra refuses it server-atomically under its no-open-children gate
    And nothing is stamped and the epoch advances when kriya observes the reopen

  Scenario: a reopen after the close heals the stamp
    Given the epic close succeeded and the completion stamp has not yet landed
    When a child ticket reopens in sutra
    Then sutra's cascade reopens the epic in the same transaction — the epic is never closed over an open child
    And a stamp that lands in the window is cleared and the epoch advanced when the reopen event arrives
    And popping resumes from the epic reopen itself, not from the mirror healing

  Scenario: a stale close is serialized or compensated, never left standing
    Given an epic close is in flight for claim epoch 3
    When a supersession advances the epoch to 4
    Then the supersession first resolves the in-flight close's outcome before issuing its reopen
    And if the stale close landed, a durable compensating reopen is driven through the same lifecycle
    And the current target's epic is never left closed by a claim whose CAS failed

  Scenario: completion clears when work returns
    Given a build target whose completion is stamped
    When a supersession installs a new head plan or a completed ticket reopens
    Then the completion epoch advances and the completion stamp is atomically cleared
    And the epic reopens through sutra
    And popping resumes for the target

  Scenario: any epic reopen forces a fresh completion
    Given a completed target whose epic reopened because deferred work activated
    Then the completion epoch advances and the stamp clears when the reopen event is consumed
    And recompletion runs under a new epoch-scoped key with a fresh review — the spent one cannot replay
    Given a completed target whose epic reopened because an active subtree was attached
    Then the same epoch advance, clear, and fresh-review requirement apply

  Scenario: supersession moves the finish line
    Given a build mid-flight when a supersession activates a new head plan
    Then completion is judged against the new head's ticket set
    And a superseded plan's ticket set never satisfies completion

  Scenario: state is inspectable from the CLI
    Given a build with runs in flight and a completed build alongside it
    When the operator runs the status command
    Then builds, runs, and their gate positions are shown, including the completed build's durable completion declaration
    When the status command runs with --json
    Then the same state is emitted machine-readably for agents and scripts

  Scenario: recompletion needs a fresh review
    Given a completed target whose epoch advanced after a ticket reopened and new work merged
    When every head-plan ticket is complete again
    Then a new completion attempt rotates the whole claim-field set with a key derived from the new epoch
    And a new build-completion review is submitted for fresh human approval
    And recovery can never adopt the prior epoch's approved review — its key names the old epoch

  Scenario: a stall surfaces with its cause
    Given no ticket is workable, none are in flight, and the epic cannot close
    Then a durable Stall row records the condition and its cause
    And the stall appears in the operator inbox from that row
    And resolution stamps the row rather than deleting it
    And kriya neither spins nor declares the build done
