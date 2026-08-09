package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRequestHeadersMatchTheRequest pins the two headers the CLI decides
// per request. Neither is visible through sutra's own API — the daemon
// reads neither one back to the caller, and it accepts a body whether or
// not it is labelled — so nothing an acceptance scenario can observe
// distinguishes a correct header from a missing one. Only the wire says.
//
// Both still matter off loopback. The contract declares request bodies as
// application/json, and a body sent unlabelled is at the mercy of whatever
// the receiving server chooses to assume; an idempotency key on a GET
// would ask a proxy to remember a read.
func TestRequestHeadersMatchTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name            string
		method          string
		body            any
		wantContentType string
		wantKey         bool
	}{
		{
			name:            "a mutation carries a labelled body and a key",
			method:          http.MethodPost,
			body:            map[string]string{"title": "a"},
			wantContentType: "application/json",
			wantKey:         true,
		},
		{
			// A mutation need not carry a body — archive takes none —
			// and there is then nothing to label, but it is still a
			// mutation and still needs its key.
			name:    "a bodiless mutation still carries a key",
			method:  http.MethodPost,
			wantKey: true,
		},
		{
			name:   "a read carries neither",
			method: http.MethodGet,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := &client{base: srv.URL}
			if _, _, err := c.do(tc.method, "/projects", tc.body); err != nil {
				t.Fatalf("do: %v", err)
			}
			if ct := got.Get("Content-Type"); ct != tc.wantContentType {
				t.Errorf("Content-Type = %q, want %q", ct, tc.wantContentType)
			}
			if hasKey := got.Get("Idempotency-Key") != ""; hasKey != tc.wantKey {
				t.Errorf("Idempotency-Key present = %v, want %v", hasKey, tc.wantKey)
			}
		})
	}
}
