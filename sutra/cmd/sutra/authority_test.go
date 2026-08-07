package main

import "testing"

// TestBrowserAuthority pins semantic wildcard and port detection:
// equivalent unspecified addresses and zero ports never become
// canonical hosts (review 1893).
func TestBrowserAuthority(t *testing.T) {
	for _, addr := range []string{
		":7358", "0.0.0.0:7358", "[::]:7358", "[0:0:0:0:0:0:0:0]:7358",
		"127.0.0.1:0", "127.0.0.1:00", "nonsense",
	} {
		if got := browserAuthority(addr); got != "" {
			t.Fatalf("%q must yield no canonical authority, got %q", addr, got)
		}
	}
	for addr, want := range map[string]string{
		"127.0.0.1:7358":   "127.0.0.1:7358",
		"[::1]:7358":       "[::1]:7358",
		"sutra.local:8080": "sutra.local:8080",
		"127.0.0.1:07358":  "127.0.0.1:7358",
	} {
		if got := browserAuthority(addr); got != want {
			t.Fatalf("%q -> %q, want %q", addr, got, want)
		}
	}
}
