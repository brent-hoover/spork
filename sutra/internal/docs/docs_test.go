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
