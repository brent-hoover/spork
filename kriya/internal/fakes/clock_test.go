package fakes_test

import (
	"testing"
	"time"

	"kriya/internal/clock"
	"kriya/internal/fakes"
)

func TestClockReportsTheTimeItWasGiven(t *testing.T) {
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	var c clock.Clock = fakes.NewClock(at)
	if got := c.Now(); !got.Equal(at) {
		t.Fatalf("Now() = %v, want %v", got, at)
	}
}

func TestClockAdvances(t *testing.T) {
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	c := fakes.NewClock(at)
	c.Advance(90 * time.Minute)
	if got, want := c.Now(), at.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", got, want)
	}
}
