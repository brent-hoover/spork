package docs

// SetMaxDiffLinesForTest overrides the line-density bound so tests
// exercise the guard without newline-gigabyte fixtures. It returns the
// bound it replaced: a test restores what it was handed rather than
// re-stating the default, so changing the default in docs.go cannot
// leave later tests in the package running against a stale value.
func SetMaxDiffLinesForTest(n int64) int64 {
	prev := maxDiffLines
	maxDiffLines = n
	return prev
}

// MaxDiffLinesForTest reports the bound in force, so a boundary test
// asserts against the real value instead of a copy of it.
func MaxDiffLinesForTest() int64 { return maxDiffLines }

// SetMaxDiffCellsForTest overrides the LCS matrix bound so tests can
// sit exactly ON it — the boundary is where the choice between a
// minimal diff and the linear fallback is actually made, and reaching
// it honestly costs two ~2048-line fixtures per assertion. Like the
// line bound, it returns what it replaced.
func SetMaxDiffCellsForTest(n int) int {
	prev := maxDiffCells
	maxDiffCells = n
	return prev
}
