Feature: Context assembly
  Each dev agent starts a ticket with exactly what it needs: its ACs,
  its modules' law from the bound snapshot, the operator's hand-crafted
  instructions, kriya's process playbook, a ticket-scoped toolset, and
  the learnings already earned. What was given is recorded.

  Scenario: context is assembled from the bound snapshot
    Given a BuildRun bound to a snapshot and a ticket touching modules "api" and "store"
    When the context manager assembles the dev agent's context
    Then it contains the ticket's acceptance criteria
    And the boundaries, contracts, and constitution of "api" and "store"
    And it is not a dump of the whole spec

  Scenario: hand-crafted instructions ride along
    Given the project carries an operator-authored instruction file
    When the context is assembled
    Then the operator's instructions are included verbatim
    And kriya's process playbook is included — driving roborev, the pair-loop protocol, and commit conventions

  Scenario: out-of-boundary modules appear as contracts only
    Given the ticket's modules may call module "billing" but not import its internals
    When the context is assembled
    Then "billing" appears only as its contract
    And no internal source of "billing" is included

  Scenario: the toolset matches the ticket
    Given the snapshot resolves commands for the ticket's modules
    When the toolset is assembled
    Then the touched modules' resolved test, lint, typecheck, coverage, and mutation commands are wired in
    And tools irrelevant to the ticket are absent

  Scenario: relevant learnings are injected
    Given persistent learnings exist for module "api" and for a failure pattern the ticket matches
    When the context is assembled
    Then those learnings are present in the context
    And learnings for unrelated modules are not

  Scenario: the assembled context is recorded
    Given a run's context was assembled
    Then the run durably records what the agent was given
    And a reviewer can reconstruct exactly what the agent knew when it acted
