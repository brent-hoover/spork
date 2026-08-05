Feature: Risk-first planning
  Every identified risk becomes a spike or research ticket that blocks
  its dependents. Spikes are worked first, retire only on documented
  evidence, and an invalidating finding escalates to the operator via
  the amended-spec path instead of letting dependent work proceed.

  Scenario: each risk becomes a blocking spike
    Given a snapshot whose plan carries risk "can the review tool run headless" with two dependent requirements
    When the PM agent decomposes it
    Then a spike ticket exists for the risk labeled per the risk/spike convention
    And the spike blocks every ticket depending on the risk's answer
    And tickets not depending on the risk are not blocked by it

  Scenario: spikes surface ahead of ordinary work
    Given an open spike ticket and an unblocked ordinary ticket of the same age
    When an agent pops work
    Then it receives the spike first

  Scenario: a finding retires the risk and unblocks dependents
    Given an in-progress spike ticket
    When the agent attaches a documented finding to the ticket and completes it
    Then the dependent tickets become workable
    And a spike completing without an attached finding is rejected

  Scenario: an invalidating finding escalates instead of proceeding
    Given a spike whose documented finding invalidates the planned approach
    When the agent records the finding
    Then the spike is not completed and its dependents stay blocked
    And the escalation reaches the operator naming the invalidated approach
    And the remedy offered is spec amendment, re-intake, and supersession
