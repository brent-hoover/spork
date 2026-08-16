package api

// SetBodyLimitForTest overrides every route's body bound; 0 restores
// the real limits. Compiled only into test binaries.
func SetBodyLimitForTest(limit int64) { testBodyLimit = limit }

// SetBetweenPrepareAndCommitForTest installs a hook that runs after the
// out-of-transaction prepare stage and before the write transaction, so
// a test can make the prepare-to-commit window deterministic. nil clears
// it. Compiled only into test binaries.
func SetBetweenPrepareAndCommitForTest(hook func()) { testBetweenPrepareAndCommit = hook }
