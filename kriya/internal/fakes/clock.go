// Package fakes holds test doubles for kriya's seams.
//
// It is a TEST-ONLY package. Nothing in production reaches it by design, so
// the lint gate strips it from the deadcode production pass — the same
// treatment sutra gives internal/faultsql. The -test pass is NOT stripped:
// a double that no test exercises is dead code and must still fail.
//
// Doubles live here rather than in _test.go files because internal/acceptance
// is a separate package and cannot see another package's test files.
package fakes

import "time"

// Clock is a clock.Clock whose time is set explicitly.
type Clock struct{ t time.Time }

// NewClock returns a Clock reading t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now returns the currently set time.
func (c *Clock) Now() time.Time { return c.t }

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) { c.t = c.t.Add(d) }
