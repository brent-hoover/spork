package docs

// SetMaxDiffLinesForTest overrides the line-density bound so tests
// exercise the guard without newline-gigabyte fixtures.
func SetMaxDiffLinesForTest(n int64) { maxDiffLines = n }

// SetMaxDiffCellsForTest overrides the LCS matrix bound so tests can
// sit exactly ON it — the boundary is where the choice between a
// minimal diff and the linear fallback is actually made, and reaching
// it honestly costs two ~2048-line fixtures per assertion.
func SetMaxDiffCellsForTest(n int) { maxDiffCells = n }
