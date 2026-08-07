package api

import (
	"fmt"
	"strings"
	"testing"
)

// TestScanDuplicateKeys pins the recursive duplicate-property scan:
// repeats reject at ANY depth, escaped names resolve to the text the
// decoder will read, and valid JSON — including duplicate-looking
// strings and repeats across SIBLING objects — passes untouched.
func TestScanDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		dup  string // expected duplicated key, "" when the body is valid
		bad  bool   // expected malformed (not a duplicate)
	}{
		{name: "empty object", body: `{}`},
		{name: "distinct keys", body: `{"a":1,"b":2,"c":3}`},
		{name: "top-level duplicate", body: `{"a":1,"a":2}`, dup: "a"},
		{name: "duplicate after nesting", body: `{"a":{"x":1},"a":2}`, dup: "a"},
		{name: "nested duplicate", body: `{"a":{"x":1,"x":2}}`, dup: "x"},
		{name: "duplicate inside array element", body: `{"a":[{"x":1},{"y":1,"y":2}]}`, dup: "y"},
		{name: "deeply nested duplicate", body: `{"a":[[{"b":{"c":[{"d":1,"d":2}]}}]]}`, dup: "d"},
		{name: "siblings may repeat a name", body: `{"a":{"x":1},"b":{"x":2}}`},
		{name: "array of same-shaped objects", body: `[{"x":1},{"x":2},{"x":3}]`},
		{name: "duplicate-looking string value", body: `{"a":"\"b\":1,\"b\":2"}`},
		{name: "escaped name equals plain name", body: `{"ab":1,"ab":2}`, dup: "ab"},
		{name: "escaped names differ", body: `{"ab":1,"ac":2}`},
		{name: "escaped quote in name", body: `{"a\"b":1,"a\"b":2}`, dup: `a"b`},
		{name: "escaped backslash then quote", body: `{"a\\":1,"b":2}`},
		{name: "nulls and literals", body: `{"a":null,"b":true,"c":false,"d":-1.5e10}`},
		{name: "whitespace everywhere", body: "{\n  \"a\" : 1 ,\n  \"a\" : 2\n}", dup: "a"},
		{name: "empty array and object values", body: `{"a":[],"b":{}}`},
		{name: "bare literal", body: `null`},
		{name: "trailing data", body: `{"a":1} {"b":2}`, bad: true},
		{name: "truncated object", body: `{"a":1`, bad: true},
		{name: "missing colon", body: `{"a" 1}`, bad: true},
		{name: "non-string key", body: `{1:2}`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := scanDuplicateKeys(strings.NewReader(tc.body))
			switch {
			case tc.dup != "":
				if apiErr == nil {
					t.Fatalf("duplicate %q accepted", tc.dup)
				}
				// The message quotes the name with %q, so compare against
				// the quoted form — otherwise an escaped name never matches.
				if !strings.Contains(apiErr.message, "repeats property "+fmt.Sprintf("%q", tc.dup)) {
					t.Fatalf("expected a duplicate-%q rejection, got %q", tc.dup, apiErr.message)
				}
			case tc.bad:
				if apiErr == nil {
					t.Fatal("malformed body accepted")
				}
				if strings.Contains(apiErr.message, "repeats property") {
					t.Fatalf("malformed body reported as a duplicate: %q", apiErr.message)
				}
			default:
				if apiErr != nil {
					t.Fatalf("valid body rejected: %q", apiErr.message)
				}
			}
		})
	}
}

// TestScanDuplicateKeysStreamsValues pins the memory property the scan
// exists to preserve: a large value is walked, never retained. The
// reader counts what it hands out so the test fails if the scan starts
// buffering the whole body.
func TestScanDuplicateKeysStreamsValues(t *testing.T) {
	const huge = 8 << 20
	body := `{"a":"` + strings.Repeat("x", huge) + `","b":1}`
	if apiErr := scanDuplicateKeys(strings.NewReader(body)); apiErr != nil {
		t.Fatalf("large value rejected: %q", apiErr.message)
	}
	// The same body with a repeat still rejects, and names the key.
	dup := `{"a":"` + strings.Repeat("x", huge) + `","a":1}`
	apiErr := scanDuplicateKeys(strings.NewReader(dup))
	if apiErr == nil || !strings.Contains(apiErr.message, `repeats property "a"`) {
		t.Fatalf("duplicate past a large value not caught: %v", apiErr)
	}
}
