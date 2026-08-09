package docs_test

import (
	"fmt"
	"strings"
	"testing"

	"sutra/internal/docs"
)

func version(number int64, content string) docs.Version {
	return docs.Version{Number: number, Content: content}
}

// TestUnifiedDiffNewlineSemantics pins real-line counting and the
// no-newline markers across terminated, unterminated, and empty
// contents.
func TestUnifiedDiffNewlineSemantics(t *testing.T) {
	// Terminated → terminated: no phantom empty line, no markers.
	diff := docs.UnifiedDiff(version(1, "a\nb\n"), version(2, "a\nc\n"))
	if strings.Contains(diff, "No newline") {
		t.Fatalf("terminated contents must carry no markers:\n%s", diff)
	}
	if !strings.Contains(diff, "@@ -1,2 +1,2 @@") {
		t.Fatalf("counts must exclude the terminator, got:\n%s", diff)
	}

	// Unterminated old, terminated new: same visible text, real change.
	diff = docs.UnifiedDiff(version(1, "a\nb"), version(2, "a\nb\n"))
	if !strings.Contains(diff, "-b\n\\ No newline at end of file\n") || !strings.Contains(diff, "+b\n") {
		t.Fatalf("termination change must show marker on the old side:\n%s", diff)
	}

	// Both unterminated and equal tail: context line carries one marker.
	diff = docs.UnifiedDiff(version(1, "x\nend"), version(2, "y\nend"))
	if !strings.Contains(diff, " end\n\\ No newline at end of file\n") {
		t.Fatalf("shared unterminated tail must carry the marker:\n%s", diff)
	}

	// Empty to content.
	diff = docs.UnifiedDiff(version(1, ""), version(2, "a\n"))
	if !strings.Contains(diff, "@@ -0,0 +1,1 @@") || !strings.Contains(diff, "+a\n") {
		t.Fatalf("empty-to-content diff must start the empty range at 0:\n%s", diff)
	}

	// Content to empty: the empty side's range starts at 0 too.
	diff = docs.UnifiedDiff(version(1, "a\n"), version(2, ""))
	if !strings.Contains(diff, "@@ -1,1 +0,0 @@") || !strings.Contains(diff, "-a\n") {
		t.Fatalf("content-to-empty diff malformed:\n%s", diff)
	}
}

func TestUnifiedDiffMinimalPath(t *testing.T) {
	diff := docs.UnifiedDiff(version(1, "a\nb\nc"), version(3, "a\nX\nc"))
	for _, want := range []string{"--- v1", "+++ v3", " a", "-b", "+X", " c"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

// TestUnifiedDiffLCSWalk pins the LCS path's WHOLE output, and does it
// at a middle bigger than one line against one line.
//
// Both of those matter. Every other diff test above asserts fragments
// with strings.Contains, which cannot see a line that is emitted twice,
// emitted in the wrong order, or emitted from the wrong side of the
// file; and every one of them leaves a middle of at most 1x1, where the
// LCS matrix is a single cell, the walk takes one step, and skipping
// the matrix entirely produces byte-identical output. At that size the
// minimal-diff path and the pure-replacement path are the same
// function, so nothing distinguishes them.
//
// These cases are built so that they do differ: a line that MOVES
// across the edit is context in a minimal diff and a delete-plus-add in
// a replacement. Each keeps a common first and last line, so the middle
// sits at a nonzero offset and an index computed from the wrong origin
// reads the wrong line rather than coincidentally the right one.
func TestUnifiedDiffLCSWalk(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     string
	}{
		// "gamma" moves ahead of "alpha". A minimal diff keeps it as
		// context; the walk must end on the a-side and drain b.
		{
			name: "a line moves earlier",
			from: "head\nalpha\nbeta\ngamma\ntail\n",
			to:   "head\ngamma\nalpha\ndelta\ntail\n",
			want: "--- v1\n+++ v2\n@@ -1,5 +1,5 @@\n head\n-alpha\n-beta\n gamma\n+alpha\n+delta\n tail\n",
		},
		// "x" moves later, so the FIRST step of the walk has to take
		// the b-side — the only thing that reads the matrix row the
		// fill loop visits last. Here the walk drains b and exits with
		// the a-side unfinished, the mirror of the case above.
		{
			name: "a line moves later",
			from: "head\nx\ny\ntail\n",
			to:   "head\nz\nx\ntail\n",
			want: "--- v1\n+++ v2\n@@ -1,4 +1,4 @@\n head\n+z\n x\n-y\n tail\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := docs.UnifiedDiff(version(1, tc.from), version(2, tc.to)); got != tc.want {
				t.Fatalf("diff mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestUnifiedDiffCellBoundary pins WHICH input gets a minimal diff and
// which gets the linear fallback, by sitting on the bound rather than
// far past it. TestUnifiedDiffBoundedFallback proves the fallback is
// correct; it does not prove the switch happens in the right place, and
// a bound off by one in either direction still passes it.
//
// Each case shares a line across the middle, so the two paths are
// distinguishable: minimal keeps it as context, the fallback restates
// it on both sides.
func TestUnifiedDiffCellBoundary(t *testing.T) {
	docs.SetMaxDiffCellsForTest(9)
	defer docs.SetMaxDiffCellsForTest(4 << 20)

	// Middles of 2 and 2: (2+1) cells wide against 9/(2+1) — exactly
	// at the bound, so the minimal diff still runs.
	got := docs.UnifiedDiff(version(1, "head\np\nq\ntail\n"), version(2, "head\nq\nr\ntail\n"))
	want := "--- v1\n+++ v2\n@@ -1,4 +1,4 @@\n head\n-p\n q\n+r\n tail\n"
	if got != want {
		t.Fatalf("at the bound the minimal diff must still run\n got: %q\nwant: %q", got, want)
	}

	// One line more on the a-side is one cell too many: the same
	// shared "q" is now restated instead of kept as context.
	got = docs.UnifiedDiff(version(1, "head\np\nq\nr\ntail\n"), version(2, "head\nq\ns\ntail\n"))
	want = "--- v1\n+++ v2\n@@ -1,5 +1,4 @@\n head\n-p\n-q\n-r\n+q\n+s\n tail\n"
	if got != want {
		t.Fatalf("one line past the bound must fall back\n got: %q\nwant: %q", got, want)
	}
}

// TestUnifiedDiffBoundedFallback pins linear memory: a middle whose
// LCS matrix would exceed the cell bound falls back to one exact
// replacement hunk — still a correct old→new diff.
func TestUnifiedDiffBoundedFallback(t *testing.T) {
	var a, b strings.Builder
	a.WriteString("shared head\n")
	b.WriteString("shared head\n")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&a, "old line %d\n", i)
		fmt.Fprintf(&b, "new line %d\n", i)
	}
	a.WriteString("shared tail")
	b.WriteString("shared tail")

	diff := docs.UnifiedDiff(version(1, a.String()), version(2, b.String()))
	for _, want := range []string{" shared head", "-old line 0", "-old line 2999", "+new line 0", "+new line 2999", " shared tail"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("fallback diff missing %q", want)
		}
	}
	if strings.Count(diff, "-old line ") != 3000 || strings.Count(diff, "+new line ") != 3000 {
		t.Fatalf("fallback diff dropped lines")
	}
}

// TestUnifiedDiffNULContent pins that termination state lives outside
// line text: documents containing NUL bytes diff faithfully, with no
// false no-newline markers and the NUL preserved in output.
func TestUnifiedDiffNULContent(t *testing.T) {
	// Terminated line ending in NUL: no marker, NUL intact.
	diff := docs.UnifiedDiff(version(1, "a\x00\n"), version(2, "b\n"))
	if !strings.Contains(diff, "-a\x00\n+b\n") {
		t.Fatalf("NUL-terminated line must diff without a marker:\n%q", diff)
	}
	if strings.Contains(diff, "No newline") {
		t.Fatalf("terminated NUL line must not carry a marker:\n%q", diff)
	}

	// Shared context line ending in NUL stays a plain context line.
	diff = docs.UnifiedDiff(version(1, "x\x00\nold\n"), version(2, "x\x00\nnew\n"))
	if !strings.Contains(diff, " x\x00\n") || strings.Contains(diff, "No newline") {
		t.Fatalf("NUL context line corrupted:\n%q", diff)
	}

	// Unterminated final line containing NUL: marker follows it.
	diff = docs.UnifiedDiff(version(1, "head\n"), version(2, "head\ntail\x00"))
	if !strings.Contains(diff, "+tail\x00\n\\ No newline at end of file\n") {
		t.Fatalf("unterminated NUL tail must carry the marker:\n%q", diff)
	}
}

// TestCheckDiffableLineBound pins the line-density guard: within the
// byte bound, newline-dense content still refuses to split.
func TestCheckDiffableLineBound(t *testing.T) {
	docs.SetMaxDiffLinesForTest(8)
	defer docs.SetMaxDiffLinesForTest(8 << 20)
	if err := docs.CheckDiffable(version(1, "a\nb\n"), version(2, "a\nc\n")); err != nil {
		t.Fatalf("sparse content must pass: %v", err)
	}
	dense := strings.Repeat("\n", 10)
	err := docs.CheckDiffable(version(1, dense), version(2, "x\n"))
	if _, ok := err.(*docs.DiffTooDenseError); !ok {
		t.Fatalf("dense content must be rejected, got %v", err)
	}
	// The same pair the other way round. The bound is on the SUM of the
	// two sides, and the case above trips it from the larger side alone
	// — so it is equally satisfied by a check that subtracts the new
	// side from the old instead of adding it.
	err = docs.CheckDiffable(version(1, "x\n"), version(2, dense))
	if _, ok := err.(*docs.DiffTooDenseError); !ok {
		t.Fatalf("density must count both sides, got %v", err)
	}
}

// TestCheckDiffableCountsUnterminatedAtLimit pins the unterminated-tail
// increment at the boundary. TestCheckDiffableCountsLines exercises
// unterminated content, but only far inside the limit, where counting
// that last line and not counting it reach the same verdict.
func TestCheckDiffableCountsUnterminatedAtLimit(t *testing.T) {
	docs.SetMaxDiffLinesForTest(8)
	defer docs.SetMaxDiffLinesForTest(8 << 20)

	// Seven newlines plus an unterminated eighth line: exactly at the
	// limit, and only if that last line counts.
	atLimit := strings.Repeat("x\n", 7) + "x"
	if err := docs.CheckDiffable(version(1, atLimit), version(2, "")); err != nil {
		t.Fatalf("eight lines is the limit, not past it: %v", err)
	}
	if err := docs.CheckDiffable(version(1, atLimit+"\ny"), version(2, "")); err == nil {
		t.Fatal("nine lines must be rejected")
	}
}

// TestCheckDiffableCountsLines pins the line count against what a diff
// actually produces: empty content is zero lines, and newline-terminated
// content has exactly its newline count. Adding one per side
// unconditionally rejected valid input at the limit (review 1934).
func TestCheckDiffableCountsLines(t *testing.T) {
	line := strings.Repeat("x\n", 1) // one terminated line
	cases := []struct {
		name     string
		from, to string
		wantErr  bool
	}{
		{name: "both empty", from: "", to: ""},
		{name: "one terminated line each", from: line, to: line},
		{name: "unterminated counts its last line", from: "a\nb", to: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := docs.CheckDiffable(docs.Version{Content: tc.from}, docs.Version{Content: tc.to})
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckDiffable(%q, %q) error = %v, wantErr %v", tc.from, tc.to, err, tc.wantErr)
			}
		})
	}
	// Exactly at the documented limit passes; one line beyond does not.
	const limit = 8 << 20 // maxDiffLines
	atLimit := strings.Repeat("x\n", limit)
	if err := docs.CheckDiffable(docs.Version{Content: atLimit}, docs.Version{}); err != nil {
		t.Fatalf("content at the documented limit rejected: %v", err)
	}
	if err := docs.CheckDiffable(docs.Version{Content: atLimit + "y\n"}, docs.Version{}); err == nil {
		t.Fatal("content past the limit accepted")
	}
}
