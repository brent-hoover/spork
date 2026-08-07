package issues

import (
	"testing"
	"time"
)

// TestTimestampLayoutSortsLexically pins the property the agent queue
// depends on: timestamps are stored as TEXT and ordered by SQLite's
// string comparison, so chronological order MUST equal lexical order.
// time.RFC3339Nano trims trailing zeros and breaks exactly that — the
// pairs below are the shapes it gets wrong (review 1898).
func TestTimestampLayoutSortsLexically(t *testing.T) {
	base := time.Date(2026, 8, 7, 5, 49, 18, 0, time.UTC)
	// Nanosecond offsets chosen so each successive instant would lose a
	// different number of trailing zeros under RFC3339Nano.
	offsets := []int{0, 1, 10, 100, 992_647_000, 992_647_500, 999_999_990, 999_999_999}

	var prev string
	for i, ns := range offsets {
		got := base.Add(time.Duration(ns)).UTC().Format(tsLayout)
		if i > 0 && prev >= got {
			t.Fatalf("later instant does not sort later: %q >= %q", prev, got)
		}
		if _, err := time.Parse(time.RFC3339Nano, got); err != nil {
			t.Fatalf("%q is not a valid RFC 3339 date-time: %v", got, err)
		}
		prev = got
	}

	// The bug this guards against, stated as fact: the trimming layout
	// really does invert one of those pairs.
	a := base.Add(992_647_000).UTC().Format(time.RFC3339Nano)
	b := base.Add(992_647_500).UTC().Format(time.RFC3339Nano)
	if a < b {
		t.Fatalf("RFC3339Nano no longer inverts %q/%q — this guard is stale", a, b)
	}
}
