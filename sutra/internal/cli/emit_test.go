package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestEmitIssueListRejectsMalformed pins that the streaming listing
// walker VALIDATES the response's shape. It consumes delimiters as it
// goes, so without explicit checks a truncated response — one ending
// right after the issues array — printed as a complete, successful
// listing (review 1938).
func TestEmitIssueListRejectsMalformed(t *testing.T) {
	const issues = `{"feed_watermark":"1","issues":[{"id":"a"},{"id":"b"}]}`
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "well formed", body: issues},
		{name: "no envelope close", body: `{"feed_watermark":"1","issues":[{"id":"a"}]`, wantErr: true},
		{name: "truncated mid array", body: `{"issues":[{"id":"a"},`, wantErr: true},
		{name: "issues is not an array", body: `{"issues":{"id":"a"}}`, wantErr: true},
		{name: "envelope is not an object", body: `["issues"]`, wantErr: true},
		{name: "trailing data", body: issues + `{"more":1}`, wantErr: true},
		{name: "no issues field", body: `{"feed_watermark":"1"}`, wantErr: true},
		{name: "empty listing", body: `{"issues":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			c := &client{env: Env{Stdout: &out}, flags: map[string]string{}}
			err := c.emitIssueList(strings.NewReader(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("emitIssueList(%s) error = %v, wantErr %v", tc.body, err, tc.wantErr)
			}
		})
	}
}
