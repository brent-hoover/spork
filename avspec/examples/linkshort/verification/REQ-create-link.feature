Feature: Create a short link

  Scenario: valid url is shortened
    Given the service is running
    When I POST "https://example.com/a/very/long/path" to /links
    Then the response status is 201
    And the body contains a short code

  Scenario: malformed url is rejected
    Given the service is running
    When I POST "not-a-url" to /links
    Then the response status is 400
    And no link is persisted
