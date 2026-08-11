package api

// SetBodyLimitForTest overrides every route's body bound; 0 restores
// the real limits. Compiled only into test binaries.
func SetBodyLimitForTest(limit int64) { testBodyLimit = limit }
