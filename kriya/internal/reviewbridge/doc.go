// Package reviewbridge is the roborev seam — enqueue, poll verdicts, respond, close; buy-over-build
// boundary in one place.
//
// Owns ReviewRound, EnqueueAttempt, and OrphanObservation.
//
// roborev's enqueue accepts no correlation key and does not dedupe by SHA
// (risk R1), so this package acts only on ids returned by its own enqueue
// calls: no adoption, no cancellation of unproven jobs.
package reviewbridge
