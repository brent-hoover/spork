Feature: TUI overview
  The operator's most critical surface. One screen shows what runs,
  what waits, and what needs a human — and every protocol path that
  says "surfaces to the operator" surfaces here.

  Scenario: one screen shows the build's live state
    Given a build with two running BuildRuns, three queued tickets, one blocked ticket, and one run awaiting human review
    And an unplanned queued ticket and a reopened ticket whose only plan membership is consumed history
    When the operator opens the TUI
    Then the running runs appear with their position in the gate chain
    And the queued and blocked tickets are distinguishable
    And the unplanned and consumed-history tickets appear too — the live tracker queue is the universe, not the current plan's rows
    And the run awaiting human review is shown as such

  Scenario: everything needing the operator lands in one inbox
    Given an SA scope escalation, an invalidating spike finding, an awaiting-operator plan, an awaiting-operator BuildRun carrying its durable error cause, a terminal run whose workspace refused cleanup with a recorded cause, an expired review-round claim, and a build stall all exist
    When the operator opens the inbox
    Then each item appears with its cause
    And the cleanup refusal shows from the workspace row while its run keeps its terminal state
    And no such item is discoverable only in a log file

  Scenario: operator actions run from the TUI
    Given an awaiting-operator plan in the inbox
    Then the operator can restore or retry it from the TUI
    Given an unresolved review round
    Then the operator can reconcile and reset it from the TUI
    Given a pending enqueue attempt, and a completed attempt whose id is not its round's accepted external id
    Then both appear in the inbox with their round, and the operator can inspect and reconcile each — stamping resolved after checking roborev
    Given an enqueue attempt whose claimant is still live
    Then reconciliation and reset are refused until the claimant is conclusively dead
    Given an escalated scope change
    Then the operator can approve or reject it from the TUI
    Given a conflicting new-work parentage in the inbox
    When the operator cedes it from the TUI
    Then the advance stamps terminal ceded
    Given another conflicting parentage
    When the operator reclaims it from the TUI
    Then the reclaim lifecycle persists write-ahead and the keyed removal and attachment reconcile to completion
    Given a permanent-failure conflict shown with its kind in the inbox
    When the operator retries after correcting the condition
    Then the generation and key rotate atomically and the advance returns to pending
    Given another permanent-failure conflict
    When the operator dismisses it from the TUI
    Then the advance stamps terminal dismissed while the ticket still blocks completion through attribution
    Given an ambiguity whose adoption is mid-reconciliation and another whose reclaim removal failed with a recorded cause
    When the operator opens the inbox
    Then both rows appear — the adopted one with its state, the failed one with its reclaim state and failure details
    Given a conflicting attribution ambiguity in the inbox, showing its observed parent and cause
    When the operator reselects a target from the TUI
    Then the resolution generation and key rotate and the fresh keyed resolution proceeds
    Given an open attribution ambiguity in the inbox
    When the operator attributes the ticket to a target from the TUI
    Then the selected target and the parenting mutation's key persist with state resolving before sutra is called
    And a crash before the resolved stamp is recovered by replaying the keyed mutation, stamping resolved with the outcome

  Scenario: the TUI reads the records recovery reads
    Given a completed run
    When the operator drills into it
    Then its commits, review rounds, gate results, verdicts, and escalations are shown
    And every displayed fact comes from the durable records recovery uses
    And no state exists only in the TUI
