package api

import (
	"context"
	"testing"
	"time"
)

// TestAdmitLargeBodyLosesRaceSafely drives the case the select alone
// cannot decide: the slot frees at the same moment the request is
// cancelled, so BOTH cases are ready and Go chooses at random. Every
// iteration must refuse admission and leave the slot free — without the
// post-acquisition recheck roughly half of them would admit a cancelled
// request and go on to read a body nobody will receive (review 1898).
func TestAdmitLargeBodyLosesRaceSafely(t *testing.T) {
	for i := range 200 {
		largeBodySlot <- struct{}{} // another large request holds it

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client is already gone

		<-largeBodySlot // ...and the slot frees right now

		release, admitted := admitLargeBody(ctx)
		if admitted {
			release()
			t.Fatalf("iteration %d: admitted a cancelled request", i)
		}
		if len(largeBodySlot) != 0 {
			t.Fatalf("iteration %d: refused admission but kept the slot", i)
		}
	}
}

// TestAdmitLargeBodyWaitsForTheSlot pins the ordinary path: a live
// request queues behind the holder and is admitted once released.
func TestAdmitLargeBodyWaitsForTheSlot(t *testing.T) {
	largeBodySlot <- struct{}{}
	admittedAt := make(chan time.Time, 1)
	go func() {
		release, admitted := admitLargeBody(context.Background())
		if !admitted {
			admittedAt <- time.Time{}
			return
		}
		at := time.Now()
		// Release BEFORE signalling: signalling first would let the
		// assertion inspect the slot while this goroutine still
		// legitimately holds it (review 1900).
		release()
		admittedAt <- at
	}()

	released := time.Now()
	<-largeBodySlot
	got := <-admittedAt
	if got.IsZero() {
		t.Fatal("a live request was refused admission")
	}
	if got.Before(released) {
		t.Fatal("admitted before the holder released the slot")
	}
	if len(largeBodySlot) != 0 {
		t.Fatal("slot leaked after the admitted request finished")
	}
}

// TestAdmitLargeBodyCancelledWhileQueued pins the waiting branch: a
// client that hangs up while queued releases its goroutine instead of
// blocking until every earlier large request completes.
func TestAdmitLargeBodyCancelledWhileQueued(t *testing.T) {
	largeBodySlot <- struct{}{}
	defer func() { <-largeBodySlot }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, admitted := admitLargeBody(ctx)
		done <- admitted
	}()
	cancel()

	select {
	case admitted := <-done:
		if admitted {
			t.Fatal("admitted while the slot was held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request stayed queued behind the slot holder")
	}
}
