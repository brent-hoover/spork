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
    Then the umbrella epic can close
    And kriya durably declares the build complete and stops popping for that target

  Scenario: supersession moves the finish line
    Given a build mid-flight when a supersession activates a new head plan
    Then completion is judged against the new head's ticket set
    And a superseded plan's ticket set never satisfies completion

  Scenario: a stall surfaces with its cause
    Given no ticket is workable, none are in flight, and the epic cannot close
    Then the stall surfaces to the operator naming its cause
    And kriya neither spins nor declares the build done
