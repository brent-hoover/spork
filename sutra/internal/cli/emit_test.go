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
//
// Where a case sets wantMsg the refusal must also SAY what was wrong.
// A walker that merely fails somewhere is not the same as one that
// names the fault: every one of these streams goes on to break again
// further downstream, so "it errored" can be satisfied by a checker
// that skipped the real problem and tripped over the wreckage after
// it, reporting a symptom three steps from the cause.
func TestEmitIssueListRejectsMalformed(t *testing.T) {
	const issues = `{"feed_watermark":"1","issues":[{"id":"a"},{"id":"b"}]}`
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantMsg string
	}{
		{name: "well formed", body: issues},
		{name: "no envelope close", body: `{"feed_watermark":"1","issues":[{"id":"a"}]`, wantErr: true},
		{name: "truncated mid array", body: `{"issues":[{"id":"a"},`, wantErr: true},
		{name: "issues is not an array", body: `{"issues":{"id":"a"}}`, wantErr: true, wantMsg: `expected "[", found {`},
		{name: "issues is a scalar", body: `{"issues":"x"}`, wantErr: true, wantMsg: `expected "[", found x`},
		{name: "envelope is not an object", body: `["issues"]`, wantErr: true, wantMsg: `expected "{", found [`},
		{name: "trailing data", body: issues + `{"more":1}`, wantErr: true, wantMsg: "trailing data"},
		{name: "no issues field", body: `{"feed_watermark":"1"}`, wantErr: true},
		{name: "empty listing", body: `{"issues":[]}`},
		// Field order is insignificant in JSON: a server may emit the
		// watermark AFTER the issues array, and draining it requires
		// consuming the key before the value (review 1944).
		{name: "property after issues", body: `{"issues":[{"id":"a"}],"feed_watermark":"1"}`},
		{name: "several properties after issues", body: `{"issues":[],"a":1,"b":{"c":[2]},"d":"x"}`},
		{name: "truncated in a trailing property", body: `{"issues":[],"feed_watermark":`, wantErr: true},
		// Draining the trailing properties is not the end of the walk —
		// the envelope still has to close and the stream still has to
		// end. Above, every stream that carried a trailing property was
		// otherwise well formed, so a drain that stopped early and
		// skipped both remaining checks looked exactly like one that
		// did not.
		{name: "envelope never closes after a trailing property", body: `{"issues":[],"feed_watermark":"1"`, wantErr: true},
		{name: "trailing data after a trailing property", body: `{"issues":[],"a":1}{"more":2}`, wantErr: true, wantMsg: "trailing data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			c := &client{env: Env{Stdout: &out}, flags: map[string]string{}}
			err := c.emitIssueList(strings.NewReader(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("emitIssueList(%s) error = %v, wantErr %v", tc.body, err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not name the fault %q", err, tc.wantMsg)
			}
		})
	}
}
