package api

import "net/http"

// SetBodyLimitForTest overrides every route's body bound; 0 restores
// the real limits. Compiled only into test binaries.
func SetBodyLimitForTest(limit int64) { testBodyLimit = limit }

// SetBetweenPrepareAndCommitForTest installs a hook on ONE server that
// runs after the out-of-transaction prepare stage and before the write
// transaction, so a test can make that window deterministic. nil clears
// it. Scoped to the handler passed in, so concurrent tests with their
// own servers cannot fire each other's hooks. Compiled only into test
// binaries.
func SetBetweenPrepareAndCommitForTest(h http.Handler, hook func()) {
	s := h.(handler).server
	if hook == nil {
		s.betweenPrepareAndCommit.Store(nil)
		return
	}
	s.betweenPrepareAndCommit.Store(&hook)
}
