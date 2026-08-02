Feature: Resolve a short link

  Scenario: known code redirects
    Given a link exists for "https://example.com" with code "abc123"
    When I GET /abc123
    Then the response status is 302
    And the Location header is "https://example.com"

  Scenario: unknown code is not found
    Given no link exists with code "nope"
    When I GET /nope
    Then the response status is 404
