package planner_test

import (
	"context"
	"testing"
)

func TestABuildsOwnCompletionEventsDoNotDestroyItsClaim(t *testing.T) {
	// Every ticket completing during a build emits a status change. Those
	// events sit in the feed until consumed — and a standing claim does not
	// prove an event happened AFTER the claim was captured. Treating them all
	// as reopens makes the epoch advance the moment a claim exists, so no
	// build could ever complete.
	//
	// The claim's WATERMARK settles it: the detection that armed the attempt
	// already saw the queue state those events produced.
	w := watcher(t, reopenOf("issue-1"), nil).stamped(t).plans(t, "issue-1")
	if err := w.work.SyncTo(context.Background(), "9"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if at := w.cursors.at[w.work.Name()]; at != "9" {
		t.Fatalf("the cursor is at %q, not the claim's watermark", at)
	}
	n, _, err := w.work.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if n != 0 {
		t.Errorf("events the claim already accounted for advanced %d times", n)
	}
	if w.epoch(t) != 0 {
		t.Error("a build's own completion events destroyed its claim")
	}
}

func TestTheCursorOnlyEverMovesForward(t *testing.T) {
	// A watermark BEHIND the cursor is an older claim's. Rewinding would
	// re-read a page for nothing.
	w := watcher(t, nil, nil)
	if err := w.work.SyncTo(context.Background(), "20"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := w.work.SyncTo(context.Background(), "5"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if at := w.cursors.at[w.work.Name()]; at != "20" {
		t.Errorf("the cursor rewound to %q", at)
	}
}

func TestAnEmptyWatermarkMovesNothing(t *testing.T) {
	w := watcher(t, nil, nil)
	if err := w.work.SyncTo(context.Background(), ""); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if at := w.cursors.at[w.work.Name()]; at != "" {
		t.Errorf("an empty watermark wrote %q", at)
	}
}

func TestAnUnparseableWatermarkIsRefused(t *testing.T) {
	// sutra's watermark and its cursor are the same decimal position.
	// Something else is a contract mismatch, and writing it would leave the
	// cursor unreadable on the next pass.
	w := watcher(t, nil, nil)
	if err := w.work.SyncTo(context.Background(), "not-a-position"); err == nil {
		t.Fatal("an unparseable watermark was written as a cursor")
	}
}

func TestEventsAfterTheWatermarkStillAdvance(t *testing.T) {
	// The control. Syncing past a claim's watermark must not deafen the
	// watcher to work that returns afterwards.
	w := watcher(t, nil, nil).stamped(t).plans(t, "issue-1")
	if err := w.work.SyncTo(context.Background(), "9"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	w.feedPage("9", reopenOf("issue-1"), "10")
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 1 {
		t.Error("work returning after the watermark did not advance the epoch")
	}
}
