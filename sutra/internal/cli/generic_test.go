package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGenericSendsTheContractPath pins the URL `sutra api` puts on the
// wire. What the query DOES is an acceptance concern and is covered there
// — a dropped filter comes back as the wrong issues. What it looks like
// when there is no query is not: sutra's own daemon reads "/projects" and
// "/projects?" as the same request, so nothing the API can say
// distinguishes them.
//
// It still matters off loopback. An empty query component is a distinct
// URI, and a cache keyed on the request line will hold two entries for
// one resource — the same class of promise as the request headers next
// door, and testable in the same place.
func TestGenericSendsTheContractPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    string
		flags map[string]string
		want  string
	}{
		{
			name: "no query leaves no trace",
			op:   "listProjects",
			want: "/projects",
		},
		{
			name:  "a path parameter is substituted",
			op:    "listIssues",
			flags: map[string]string{"path.projectId": "019fe000-0000-7000-8000-000000000001"},
			want:  "/projects/019fe000-0000-7000-8000-000000000001/issues",
		},
		{
			name: "queries are appended in sorted order",
			op:   "listIssues",
			flags: map[string]string{
				"path.projectId": "p",
				"query.status":   "open",
				"query.assignee": "a b",
			},
			want: "/projects/p/issues?assignee=a+b&status=open",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.RequestURI()
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			flags := tc.flags
			if flags == nil {
				flags = map[string]string{}
			}
			c := &client{
				base:  srv.URL,
				env:   Env{Stdout: &bytes.Buffer{}, Getenv: func(string) string { return "" }},
				flags: flags,
			}
			if err := c.generic([]string{tc.op}); err != nil {
				t.Fatalf("generic: %v", err)
			}
			if got != tc.want {
				t.Errorf("request URI = %q, want %q", got, tc.want)
			}
		})
	}
}
