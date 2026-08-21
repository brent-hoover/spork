// Package trackerclient is the one module that speaks the issue tracker's API — sutra today, the seam
// where other trackers would plug in.
//
// Every mutation carries an idempotency key persisted before the call.
package trackerclient
