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
    Then kriya submits a build-completion review on the umbrella epic with a completion-report document as its deliverable
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

  Scenario: a stall surfaces with its cause
    Given no ticket is workable, none are in flight, and the epic cannot close
    Then the stall surfaces to the operator naming its cause
    And kriya neither spins nor declares the build done
