Feature: Search and filter

  # The three matches are created in reverse rank order, so issue number runs
  # opposite to the ranking. Created in rank order the two sequences coincide,
  # and "returned ranked" is then satisfied by a search that only sorts by
  # number — which is to say, by one that does not rank at all.
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

  # The filters intersect, so omitting one WIDENS the scope rather than
  # narrowing it, and omitting all of them widens it to the whole database.
  # Every other scenario here names a project, a term, or a session, which
  # means a handler that answered an unfiltered query with nothing at all
  # would look exactly like one that answered it correctly. The reviews
  # column is the asymmetry worth naming: a review carries no text of its
  # own, so it enumerates under a scope but never under a term.
  Scenario Outline: each filter omitted widens the scope
    Given content in "SUT" and a second project, both mentioning "retry policy"
    When the search names <filters>
    Then it returns <scope>
    # The one surface is assembled from several independent passes — text,
    # session, review enumeration — so without an explicit order the same
    # query would return the same issues in whatever sequence the passes
    # happened to reach them. Grouping is the guarantee, not a particular
    # project going first: a reader scanning results should never find a
    # project's issues split apart by another project's.
    And each project's issues arrive together and in ascending number order

    Examples:
      | filters          | scope                                                                     |
      | nothing at all   | every issue, review, doc, and thread from both projects                   |
      | only the term    | the matching issues, docs, and threads from both projects, and no reviews |
      | only the project | everything in "SUT" and nothing from the second project                   |

  Scenario: session id joins an instance's work
    Given a review whose revision 1 was submitted under session "sess-42" and revision 2 under session "sess-43"
    And an unrelated review whose submissions never carried "sess-42"
    And an imported thread carrying session "sess-42"
    When "sess-42" is searched
    Then the review, the thread, and their linked issues are returned
    And the unrelated review is not returned
    And listing reviews filtered by session "sess-42" also returns the review and excludes the unrelated one
    # A session names work, not a place. Scoping the same session to a project
    # that work never touched must come back empty — the filters intersect, so
    # a session reaching an issue outside the named project is out of scope,
    # and a handler that let the session's own reach decide would return it.
    And scoping that same session search to a project the work never touched returns nothing
