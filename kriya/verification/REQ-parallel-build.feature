Feature: Parallel build with deterministic pop binding
  Independent tickets build concurrently, one BuildRun each. Every pop
  is write-ahead, admission is fenced while any head plan is
  unactivated, and a popped ticket binds to exactly one plan and
  snapshot by the state-aware precedence algorithm — across crashes,
  supersessions, and reopens.

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
    Given a replacement has incremented the fence and the new head has not yet activated
    When an agent attempts to pop
    Then admission is refused by a CAS write on the fence, not a read-assert
    When the head activates and decrements the fence
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
    Given a queued BuildRun whose ticket field is unfilled at recovery
    When retirement begins
    Then the BuildRun is bound by pop replay before any ownership classification
    And the in-flight claim is classified as issued work, never retired as pending
