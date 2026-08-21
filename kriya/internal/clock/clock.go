package clock

import "time"

// Clock reports the current time.
//
// Production code takes a Clock rather than calling time.Now directly, so
// recovery, fencing, and expiry tests can control it. time.Now is banned
// everywhere outside this package by lint; see .golangci.yml.
//
// The real implementation lands with the composition root (M1 step 5c),
// which is the first thing that can reach it from main. Shipping it earlier
// makes it unreachable production code, and kriya's lint gate runs deadcode
// twice — the second pass without -test — with no allowlist.
type Clock interface {
	Now() time.Time
}

// System is the real clock, reading the host's wall time. It lands here now
// that the composition root can reach it; shipped earlier it was unreachable
// production code and the no-allowlist deadcode gate rejected it.
type System struct{}

// Now returns the current wall time.
func (System) Now() time.Time { return time.Now() }
