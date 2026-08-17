Feature: Documents tied to issues

  Scenario: link and unlink after creation
    Given document "notes" exists in project "SUT" with no issue
    When "notes" is tied to issue SUT-1 by "human-brent"
    Then SUT-1 lists "notes"
    And a "doc.linked" event with actor "human-brent" and a timestamp is recorded for SUT-1
    When "notes" is untied from SUT-1 by "human-brent"
    Then SUT-1 lists no documents
    And a "doc.unlinked" event with actor "human-brent" and a timestamp is recorded for SUT-1

  # A relink is a move, not an overwrite: the issue losing the document
  # records the departure, so a document never silently vanishes from an
  # issue's history.
  Scenario: relinking to another issue records the departure
    Given document "notes" is tied to issue SUT-1
    When "notes" is tied to issue SUT-2 by "human-brent"
    Then SUT-2 lists "notes"
    And SUT-1 lists no documents
    And a "doc.unlinked" event with actor "human-brent" and a timestamp is recorded for SUT-1
    And a "doc.linked" event with actor "human-brent" and a timestamp is recorded for SUT-2

  # Two documents, because the criterion says "documents" and a listing is
  # a set. Every other assertion in this feature is made against an issue
  # holding zero documents or exactly one, and at those cardinalities a
  # listing that streams every row is indistinguishable from one that
  # stops after the first. The listing is ordered by title, so "design"
  # comes back either way and only "rollout" can tell them apart.
  Scenario: both sides see the link
    Given document "design" is tied to issue SUT-1
    And document "rollout" is tied to issue SUT-1
    When SUT-1's documents are requested
    Then "design" is listed
    And "rollout" is listed
    And "design" shows SUT-1 as its issue
