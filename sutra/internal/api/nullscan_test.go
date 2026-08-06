package api

import (
	"strings"
	"testing"
)

// TestScanExplicitNulls pins the byte-lexer's null detection: top-level
// nulls found, nested nulls and null-looking strings ignored, escapes
// honored, valid JSON never rejected.
func TestScanExplicitNulls(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		nulls []string
		bad   bool
	}{
		{name: "empty object", body: `{}`},
		{name: "no nulls", body: `{"a":1,"b":"x","c":true,"d":false,"e":2.5e3}`},
		{name: "one null", body: `{"a":null}`, nulls: []string{"a"}},
		{name: "null among members", body: `{"a":1,"b":null,"c":"x"}`, nulls: []string{"b"}},
		{name: "nested null ignored", body: `{"a":{"inner":null},"b":[null,1]}`},
		{name: "null-like string ignored", body: `{"a":"null"}`},
		{name: "escaped quote in value", body: `{"a":"he said \"null\"","b":null}`, nulls: []string{"b"}},
		{name: "escaped quote in key", body: `{"a\"b":null}`, nulls: []string{`a"b`}},
		{name: "escaped backslash then quote", body: `{"a":"c:\\","b":null}`, nulls: []string{"b"}},
		{name: "whitespace everywhere", body: "{\n  \"a\" : null ,\n  \"b\" : 2\n}", nulls: []string{"a"}},
		{name: "deep nesting", body: `{"a":[[{"x":[null]}]],"b":null}`, nulls: []string{"b"}},
		{name: "unicode-escaped key null", body: `{"expected\u005fdefault\u005fhead":null}`, nulls: []string{"expected_default_head"}},
		{name: "escaped key no null", body: `{"a\u0062c":"x","d":null}`, nulls: []string{"d"}},
		{name: "not an object", body: `[1,2]`, bad: true},
		{name: "bad literal", body: `{"a":nope}`, bad: true},
		{name: "truncated", body: `{"a":`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nulls, apiErr := scanExplicitNulls(strings.NewReader(tc.body))
			if tc.bad {
				if apiErr == nil {
					t.Fatalf("expected rejection, got %v", nulls)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("valid body rejected: %s", apiErr.message)
			}
			if len(nulls) != len(tc.nulls) {
				t.Fatalf("nulls %v, want %v", nulls, tc.nulls)
			}
			for _, k := range tc.nulls {
				if !nulls[k] {
					t.Fatalf("missing null key %q in %v", k, nulls)
				}
			}
		})
	}
}
