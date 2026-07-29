Feature: Create a short link (REQ-001)

  Scenario: valid url is shortened
    Given a valid URL "https://example.com/some/very/long/path"
    When it is POSTed to /links
    Then the response status is 201
    And the body contains a short code
    And the code resolves back to the original URL

  Scenario: malformed url is rejected
    Given a malformed URL "not-a-url"
    When it is POSTed to /links
    Then the response status is 400
    And nothing is persisted
