Feature: Planning doc templates

  Scenario: templates are managed by name
    Given no template named "tech-spec" exists
    When template "tech-spec" is created with content
    Then it appears in the template list
    When it is updated and then removed
    Then the list reflects each change

  Scenario: template seeds the first version
    Given template "tech-spec" exists
    When document "parser-spec" is created in project "SUT" from "tech-spec"
    Then version 1 of "parser-spec" has the template's content
    And saving new content appends version 2 as usual
