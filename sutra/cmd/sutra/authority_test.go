package main

import (
	"net"
	"strings"
	"testing"
)

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

// TestDialableAddr pins that the UI is told an address it can actually
// connect to: the resolved port for an ephemeral bind, and loopback in
// place of a wildcard, which is an accept-any address and not a
// destination (review 1926).
func TestDialableAddr(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{name: "explicit host and port", addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7357}, want: "127.0.0.1:7357"},
		{name: "ipv4 wildcard", addr: &net.TCPAddr{IP: net.IPv4zero, Port: 7357}, want: "127.0.0.1:7357"},
		{name: "ipv6 wildcard", addr: &net.TCPAddr{IP: net.IPv6unspecified, Port: 7357}, want: "[::1]:7357"},
		{name: "no host", addr: &net.TCPAddr{Port: 7357}, want: "127.0.0.1:7357"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialableAddr(tc.addr); got != tc.want {
				t.Fatalf("dialableAddr(%v) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}

	// The ephemeral case end to end: the resolved port is never 0.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	got := dialableAddr(ln.Addr())
	if strings.HasSuffix(got, ":0") {
		t.Fatalf("ephemeral bind reported as %q; the UI would dial port 0", got)
	}
	if _, err := net.Dial("tcp", got); err != nil {
		t.Fatalf("address %q is not dialable: %v", got, err)
	}
}
