Feature: Search and filter

  Scenario: text search over issues
    Given issues exist mentioning "tokenizer" in title, body, or comments
    When project "SUT" is searched for "tokenizer"
    Then those issues are returned ranked

  Scenario: filters compose
    Given a mix of issues in project "SUT"
    When issues are listed with status "open", assignee "claude", label "bug"
    Then only issues matching all three are returned
    And adding a text term narrows the same result set

  Scenario: one surface over all content
    Given a doc and a thread in "SUT" mention "retry policy"
    When "SUT" is searched for "retry policy" across all content
    Then the doc and the thread appear alongside matching issues

  Scenario: session id joins an instance's work
    Given a review and an imported thread both carry session "sess-42"
    When "sess-42" is searched
    Then the review, the thread, and their linked issues are returned
