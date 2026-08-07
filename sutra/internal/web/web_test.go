package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTokenText pins that a re-serialized transcript is valid JSON
// with its separators intact and its numbers unshifted (review 1889).
func TestTokenText(t *testing.T) {
	for _, src := range []string{
		`{"a":1}`,
		`{"a":1,"b":2}`,
		`{"a":[1,2],"b":{"c":"d"}}`,
		`{"big":12345678901234567890}`,
		`"plain string"`,
		`42`,
		`null`,
		`{}`,
		`{"nested":{"deep":[{"x":true},null]}}`,
	} {
		dec := json.NewDecoder(strings.NewReader(src))
		dec.UseNumber()
		first, err := dec.Token()
		if err != nil {
			t.Fatalf("%s: first token: %v", src, err)
		}
		got := tokenText(first, dec)
		var reparsed any
		if err := json.Unmarshal([]byte(got), &reparsed); err != nil {
			t.Fatalf("%s re-serialized to invalid JSON %q: %v", src, got, err)
		}
		if got != src {
			t.Fatalf("%s re-serialized as %q", src, got)
		}
	}
}
