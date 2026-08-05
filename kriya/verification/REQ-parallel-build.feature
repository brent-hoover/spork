Feature: Parallel build with deterministic pop binding
  Independent tickets build concurrently, one BuildRun each. Every pop
  is write-ahead and admission is fenced while any head plan is
  unactivated. A planned ticket binds to exactly one plan and snapshot
  by the state-aware precedence algorithm — across crashes,
  supersessions, and reopens; an unplanned ticket binds the mapping's
  intake-pinned snapshot with no plan, and an ambiguous one parks
  awaiting-operator.

  Scenario: independent tickets build in parallel
    Given two unblocked tickets with no relation between them
    When two agents pop work concurrently
    Then each agent receives a different ticket in its own BuildRun
    And a blocked ticket is never handed out
    And sutra's conditional claim rejects a second claim on an already-claimed ticket

  Scenario: pops are written ahead and never orphaned
    Given an agent pops a ticket
    Then the BuildRun record is durable before any other side effect
    When the process crashes between the claim and the binding
    Then recovery discovers the claimed-but-unbound pop and binds it by replaying the binding rules
    And no claim remains without a BuildRun that owns it

  Scenario: the pop fence holds until activation
    Given an agent read the fence at version V with the unactivated-heads counter at zero
    When a replacement transaction concurrently increments the counter and the fence version
    And the agent attempts admission using its stale read
    Then the admission CAS on the fence version fails — a plain read-assert would have admitted it
    When the agent retries against the fresh fence while the head is still unactivated
    Then admission is refused because the counter is nonzero
    When the head activates and decrements the counter
    Then the retried pop succeeds

  Scenario: binding follows the state-aware precedence
    Given a supersession chain with a superseded plan holding an unstamped row for the ticket
    Then a pop mid-retirement binds the deepest unstamped superseded row
    Given the head has not yet activated and no unstamped superseded row exists
    Then the pop binds the nearest predecessor row on the chain and never the unactivated head
    Given the head is activated and lists the ticket
    Then the pop binds the head's own row
    Given the ticket's only membership is consumed rows
    Then the pop binds the nearest consumed row's plan — the decomposition that produced its acceptance criteria — never a head that omitted it
    And every walked-past row is stamped by the binding

  Scenario: queued pops reconcile before classification
    Given a queued BuildRun whose ticket field is unfilled at recovery because a claim landed but the ticket was never bound
    When retirement begins
    Then the BuildRun is bound by pop replay before any ownership classification
    And the in-flight claim is classified as issued work, never retired as pending

  Scenario: an empty pop reconciles to no-work
    Given a queued BuildRun whose ticket field is unfilled because the pop never claimed anything
    When recovery replays the pop and the replay returns an explicit empty result
    Then the BuildRun transitions to no-work
    And no work is invented for it and nothing is classified as issued
