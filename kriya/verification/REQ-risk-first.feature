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
    Given decomposition produced a mixed set of spike and implementation tickets
    When the assignment phase runs
    Then every spike assignment completes before any implementation assignment begins
    Given an older assignment from another plan already sits in the identity's queue
    When an agent pops work under the tracker's FIFO ordering
    Then the pre-existing independent ticket may pop first, which is safe — it depends on none of this plan's risks
    And among this plan's tickets the spike pops ahead of its implementation work
    And a ticket depending on the risk cannot pop at all while the spike is open, regardless of queue position
    And no tracker-side priority mechanism is assumed

  Scenario: a finding retires the risk and unblocks dependents
    Given an in-progress spike ticket with a documented finding attached
    When the finding is submitted as a document-deliverable Review on the spike ticket
    And a human approves it
    Then the spike closes through sutra's approved-review gate
    And the dependent tickets become workable
    And a spike with no approved finding review cannot close

  Scenario: an invalidating finding escalates instead of proceeding
    Given a spike whose documented finding invalidates the planned approach
    When the agent records the finding
    Then the spike is not completed and its dependents stay blocked
    And the escalation reaches the operator naming the invalidated approach
    And the spike's run records cancelling, conditionally defers its ticket — never to open — and lands terminal cancelled before any supersession
    And retirement classifies the released spike ticket as released work, never issued work that will finish
    And the remedy offered is spec amendment, re-intake, and supersession

  Scenario: a finding submission crash recovers exactly once
    Given the finding doc key and pending reference were persisted and the crash hit before the document version existed
    When recovery runs
    Then the keyed doc mutation replays and creates the version, and the finding review follows through the ordinary submission machinery
    Given the document version was created and the crash hit before the review-create call
    When recovery replays the keyed doc mutation
    Then sutra returns the same version — no duplicate finding — and the keyed review creation runs exactly once
