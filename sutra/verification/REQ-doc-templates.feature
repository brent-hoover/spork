Feature: Planning doc templates

  # Two templates, because the catalog is a set. With one member a listing
  # that streams every row and one that stops after the first return the
  # same bytes, so every claim below about "the list" was being made at a
  # cardinality that cannot distinguish them. The catalog is ordered by
  # name, so "tech-spec" — the one this scenario goes on to update and
  # remove — is exactly the row a truncating listing drops.
  Scenario: templates are managed by name
    Given no template named "tech-spec" exists
    When template "tech-spec" is created with content
    And template "onboarding" is created with content
    Then each appears in the template list
    When "tech-spec" is updated and then removed
    Then the list holds only "onboarding"

  Scenario: template seeds the first version
    Given template "tech-spec" exists
    When document "parser-spec" is created in project "SUT" from "tech-spec"
    Then version 1 of "parser-spec" has the template's content
    And saving new content appends version 2 as usual
