package context

import (
	"fmt"
	"time"
)

// timeLayout is how this module writes timestamps.
//
// RFC3339 with nanoseconds, in UTC, because SQLite stores text and a
// comparison between two differently-formatted timestamps is a silent
// ordering bug rather than a parse error.
const timeLayout = time.RFC3339Nano

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}
