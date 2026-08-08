package api

import (
	"fmt"
	"reflect"
	"testing"
)

// TestChunkIDs pins the batching that keeps import's id probe under
// SQLite's bound-variable limit. The case that matters is the exact
// multiple: a trailing empty chunk builds `IN (?` + Repeat(",?", -1) + `)`,
// which panics before it can query anything. Real payloads never hold a
// multiple of 500 ids, so only a direct test reaches it.
func TestChunkIDs(t *testing.T) {
	ids := make([]string, 5)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	for _, tc := range []struct {
		name string
		in   []string
		size int
		want [][]string
	}{
		{"no ids at all", nil, 2, nil},
		{"fewer than one chunk", ids[:1], 2, [][]string{{"id-0"}}},
		{"exactly one chunk", ids[:2], 2, [][]string{{"id-0", "id-1"}}},
		{"an exact multiple leaves no empty tail", ids[:4], 2,
			[][]string{{"id-0", "id-1"}, {"id-2", "id-3"}}},
		{"a ragged tail keeps its remainder", ids[:5], 2,
			[][]string{{"id-0", "id-1"}, {"id-2", "id-3"}, {"id-4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkIDs(tc.in, tc.size)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("chunkIDs(%v, %d) = %v, want %v", tc.in, tc.size, got, tc.want)
			}
			for i, chunk := range got {
				if len(chunk) == 0 {
					t.Fatalf("chunk %d is empty — the IN (...) probe would build a repeat count of -1", i)
				}
			}
		})
	}
}
