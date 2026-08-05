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

  Scenario: completion is the head plan done and the epic closable
    Given every ticket of the activated head plan is complete
    Then kriya records review-submitting with a deterministic submission key and the completion-report doc version before calling sutra
    And kriya submits a build-completion review on the umbrella epic with the completion-report document as its deliverable
    And a crash before the review id lands is recovered by querying sutra with the submission key and adopting the found review, never submitting twice
    And retired tickets resting deferred under the epic do not block its close
    When the human approves the review
    Then the epic close is attempted through sutra, whose no-open-children gate is the authoritative check
    And after the close succeeds, completion is stamped on the BuildTarget row — a CAS requiring the current epoch to still equal the immutable claim epoch recorded at submission — and popping stops for that target

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
