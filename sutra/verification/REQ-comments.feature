Feature: Comments on issues

  Scenario: comment lands on the issue
    Given issue SUT-1 exists
    When "human-brent" comments "looks wrong" on SUT-1
    Then the comment appears in SUT-1's comments with author and timestamp

  Scenario: replies nest without depth limit
    Given a comment thread five levels deep on SUT-1
    When a reply is added at the deepest level
    Then it nests under its parent as level six

  Scenario: no artificial caps
    Given issue SUT-1 has one thousand comments
    When another comment is added
    Then it succeeds like the first

  Scenario: a reader comments from the issue page
    Given issue SUT-1 exists
    And SUT-1 is open in a reader's browser
    When they comment "looks wrong" from the page
    Then the comment appears in SUT-1's discussion on the page
