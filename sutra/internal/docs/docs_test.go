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
