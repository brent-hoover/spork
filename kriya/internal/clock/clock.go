package clock

import "time"

// Clock reports the current time.
//
// Production code takes a Clock rather than calling time.Now directly, so
// recovery, fencing, and expiry tests can control it. time.Now is banned
// everywhere outside this package by lint; see .golangci.yml.
type Clock interface {
	Now() time.Time
}

// System is the real clock, reading the host's wall time.
type System struct{}

// Now returns the current wall time.
func (System) Now() time.Time { return time.Now() }
