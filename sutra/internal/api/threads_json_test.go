package api

import (
	"encoding/json"
	"testing"

	"sutra/internal/threads"
)

// TestThreadJSONSizesItsBufferExactly pins the one resource promise this
// splice makes. A transcript is unbounded, and threadJSON is the ONE path
// that must materialize it in memory (the idempotency store records
// mutation responses verbatim). If the capacity is off in either
// direction the transcript is copied twice — too small and append grows,
// too large and the slack is carried around — and no response body would
// look any different, so only the buffer itself can say.
func TestThreadJSONSizesItsBufferExactly(t *testing.T) {
	project := "11111111-1111-1111-1111-111111111111"
	session := "sess-7"
	for _, tc := range []struct {
		name       string
		transcript string
	}{
		{"a small transcript", `[{"speaker":"claude","text":"hi"}]`},
		{"whitespace the import preserved", "[\n  {\n    \"speaker\": \"claude\"\n  }\n]"},
		{"a null transcript", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, apiErr := threadJSON(threads.Thread{
				ID:         "22222222-2222-2222-2222-222222222222",
				Title:      "a thread",
				Transcript: json.RawMessage(tc.transcript),
				Session:    &session,
				Project:    &project,
				ImportedAt: "2026-01-01T00:00:00Z",
			})
			if apiErr != nil {
				t.Fatalf("threadJSON: %v", apiErr.message)
			}
			if cap(out) != len(out) {
				t.Errorf("buffer grew or carried slack: cap=%d len=%d", cap(out), len(out))
			}
			// The bytes still have to be the right bytes: an exactly
			// sized buffer holding the wrong splice is no better.
			var decoded struct {
				Transcript json.RawMessage `json:"transcript"`
			}
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("decode %s: %v", out, err)
			}
			if string(decoded.Transcript) != tc.transcript {
				t.Errorf("transcript changed: want %s, got %s", tc.transcript, decoded.Transcript)
			}
		})
	}
}
