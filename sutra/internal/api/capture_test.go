package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCaptureBodyRoutesByContentLength straddles largeBodyThreshold and
// pins the UNKNOWN-length case to the same side as an oversized one.
// Both routes produce identical bytes, so no scenario can tell them
// apart — but the choice is the whole point of the threshold: a body
// held in RAM when it should have spooled costs memory the server does
// not have, and a chunked upload declares no length at all.
func TestCaptureBodyRoutesByContentLength(t *testing.T) {
	cases := []struct {
		name    string
		length  int64
		spooled bool
	}{
		{name: "empty body", length: 0},
		{name: "exactly at the threshold", length: largeBodyThreshold},
		{name: "one byte past the threshold", length: largeBodyThreshold + 1, spooled: true},
		{name: "unknown length", length: -1, spooled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The declared length drives the routing; the actual bytes
			// only have to survive it, so they stay small.
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
			req.ContentLength = tc.length

			body, err := captureBody(httptest.NewRecorder(), req, 8<<20)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			defer body.close()

			switch {
			case tc.spooled && body.file == nil:
				t.Fatal("body was held in RAM; a large or unmeasured body must spool to disk")
			case !tc.spooled && body.file != nil:
				t.Fatal("body spooled to disk; one that fits in RAM must not touch the filesystem")
			}
		})
	}
}
