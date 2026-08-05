Feature: Learning loop
  Mistakes become persistent learnings the moment they are corrected —
  and the operator can add their own by hand. Learnings are scoped,
  traceable to their origin, and fed into the next run they match, so
  the system stops repeating itself.

  Scenario: learnings are captured at the moment of correction
    Given a review finding is fixed in the pair loop
    Then a learning is recorded then and there, stating what went wrong and how to avoid it
    And it is tagged with the module and the failure pattern
    Given a gate failure is diagnosed, an SA direction lands, or the PO rejects a run
    Then each produces its learning the same way, not in a post-mortem

  Scenario: the operator adds learnings by hand
    Given the operator writes a learning from their own experience with the project
    When they add it manually
    Then it carries the same scope and tags as a captured learning
    And the feed-forward path treats it exactly like a captured one

  Scenario: learnings outlive their build
    Given a learning scoped project-specific and another scoped cross-project
    When the run and the build that produced them complete
    Then both learnings persist durably
    And the cross-project learning is visible outside its project of origin

  Scenario: learnings feed the next matching run
    Given a learning tagged with module "api" was captured in an earlier run
    When a new run starts whose ticket touches "api"
    Then the learning is injected into that run's context
    Given a cross-project learning matching a later build's ticket
    Then it is injected there too

  Scenario: every learning knows its origin
    Given a captured learning from a code-backed correction
    Then it records the run, the triggering commit, and the finding or event that produced it
    Given a captured learning from a research-backed correction
    Then it records the run, the finding document version, and the event that produced it
    Given a manual learning
    Then it records the operator as its origin
