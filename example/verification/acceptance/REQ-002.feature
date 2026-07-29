Feature: Resolve a short link (REQ-002)

  Scenario: known code redirects
    Given a previously created code for "https://example.com/target"
    When GET /{code} is requested
    Then the response status is 302
    And the Location header is "https://example.com/target"

  Scenario: unknown code is not found
    Given a code that was never created
    When GET /{code} is requested
    Then the response status is 404
