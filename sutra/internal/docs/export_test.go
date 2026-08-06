package docs

// SetMaxDiffLinesForTest overrides the line-density bound so tests
// exercise the guard without newline-gigabyte fixtures.
func SetMaxDiffLinesForTest(n int64) { maxDiffLines = n }
