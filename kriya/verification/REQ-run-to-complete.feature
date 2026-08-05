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
    Then the epic closes through sutra's approved-review gate
    And completion is stamped on the BuildTarget row — a CAS requiring the head generation recorded at submission to still be current — and popping stops for that target

  Scenario: a stale approval never stamps a newer head
    Given a build-completion review submitted under head generation 3
    And a supersession has since moved the head to generation 4
    When the approval event arrives
    Then the completion CAS fails, nothing is stamped, and the stale approval surfaces to the operator

  Scenario: completion clears when work returns
    Given a build target whose completion is stamped
    When a supersession installs a new head plan or a completed ticket reopens
    Then the completion stamp is atomically cleared and the epic reopens through sutra
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
