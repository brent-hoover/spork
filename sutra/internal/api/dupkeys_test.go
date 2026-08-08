package api

import (
	"bufio"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestScanDuplicateKeys pins the recursive duplicate-property scan:
// repeats reject at ANY depth, escaped names resolve to the text the
// decoder will read, and valid JSON — including duplicate-looking
// strings and repeats across SIBLING objects — passes untouched.
func TestScanDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		dup  string // expected duplicated key, "" when the body is valid
		bad  bool   // expected malformed (not a duplicate)
	}{
		{name: "empty object", body: `{}`},
		{name: "distinct keys", body: `{"a":1,"b":2,"c":3}`},
		{name: "top-level duplicate", body: `{"a":1,"a":2}`, dup: "a"},
		{name: "duplicate after nesting", body: `{"a":{"x":1},"a":2}`, dup: "a"},
		{name: "nested duplicate", body: `{"a":{"x":1,"x":2}}`, dup: "x"},
		{name: "duplicate inside array element", body: `{"a":[{"x":1},{"y":1,"y":2}]}`, dup: "y"},
		{name: "deeply nested duplicate", body: `{"a":[[{"b":{"c":[{"d":1,"d":2}]}}]]}`, dup: "d"},
		{name: "siblings may repeat a name", body: `{"a":{"x":1},"b":{"x":2}}`},
		{name: "array of same-shaped objects", body: `[{"x":1},{"x":2},{"x":3}]`},
		{name: "duplicate-looking string value", body: `{"a":"\"b\":1,\"b\":2"}`},
		{name: "escaped name equals plain name", body: `{"ab":1,"ab":2}`, dup: "ab"},
		{name: "escaped names differ", body: `{"ab":1,"ac":2}`},
		{name: "escaped quote in name", body: `{"a\"b":1,"a\"b":2}`, dup: `a"b`},
		{name: "escaped backslash then quote", body: `{"a\\":1,"b":2}`},
		{name: "nulls and literals", body: `{"a":null,"b":true,"c":false,"d":-1.5e10}`},
		{name: "whitespace everywhere", body: "{\n  \"a\" : 1 ,\n  \"a\" : 2\n}", dup: "a"},
		{name: "empty array and object values", body: `{"a":[],"b":{}}`},
		{name: "bare literal", body: `null`},
		{name: "trailing data", body: `{"a":1} {"b":2}`, bad: true},
		{name: "truncated object", body: `{"a":1`, bad: true},
		{name: "missing colon", body: `{"a" 1}`, bad: true},
		{name: "non-string key", body: `{1:2}`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := scanDuplicateKeys(context.Background(), strings.NewReader(tc.body))
			switch {
			case tc.dup != "":
				if apiErr == nil {
					t.Fatalf("duplicate %q accepted", tc.dup)
				}
				// The message quotes the name with %q, so compare against
				// the quoted form — otherwise an escaped name never matches.
				if !strings.Contains(apiErr.message, "repeats property "+fmt.Sprintf("%q", tc.dup)) {
					t.Fatalf("expected a duplicate-%q rejection, got %q", tc.dup, apiErr.message)
				}
			case tc.bad:
				if apiErr == nil {
					t.Fatal("malformed body accepted")
				}
				if strings.Contains(apiErr.message, "repeats property") {
					t.Fatalf("malformed body reported as a duplicate: %q", apiErr.message)
				}
			default:
				if apiErr != nil {
					t.Fatalf("valid body rejected: %q", apiErr.message)
				}
			}
		})
	}
}

// TestScanDuplicateKeysBoundsKeyMemory pins that the budget covers all
// OPEN objects together and is released as each closes. Sibling objects
// may therefore carry unlimited names in total — only one nesting path
// is ever live — while a single object cannot exceed the budget.
func TestScanDuplicateKeysBoundsKeyMemory(t *testing.T) {
	const keyLen = 600
	name := func(i int) string { return fmt.Sprintf("%0*d", keyLen, i) }

	// Sequential siblings, far past the budget in total: each closes
	// before the next opens, so the live set is one key.
	var siblings strings.Builder
	siblings.WriteByte('[')
	for i := range 4000 { // 4000 * 600B = 2.4 MiB of names, budget 1 MiB
		if i > 0 {
			siblings.WriteByte(',')
		}
		fmt.Fprintf(&siblings, `{%q:1}`, name(i))
	}
	siblings.WriteByte(']')
	if apiErr := scanDuplicateKeys(context.Background(), strings.NewReader(siblings.String())); apiErr != nil {
		t.Fatalf("sequential objects rejected: %q", apiErr.message)
	}

	// One object holding more than the budget at once.
	var fat strings.Builder
	fat.WriteByte('{')
	for i := range 4000 {
		if i > 0 {
			fat.WriteByte(',')
		}
		fmt.Fprintf(&fat, `%q:1`, name(i))
	}
	fat.WriteByte('}')
	apiErr := scanDuplicateKeys(context.Background(), strings.NewReader(fat.String()))
	if apiErr == nil {
		t.Fatal("an object exceeding the key budget was accepted")
	}
	if !strings.Contains(apiErr.message, "scan budget") {
		t.Fatalf("expected a budget rejection, got %q", apiErr.message)
	}
}

// TestScanDuplicateKeysStreamsValues MEASURES the memory property the
// scan exists to preserve, rather than asserting it: scanning a body
// whose single string value is 64 MiB must allocate a bounded amount,
// because values stream past unretained. A tokenizer that materialized
// string values — the failure mode this guards — would allocate on the
// order of the value itself and blow the ceiling by ~100x.
func TestScanDuplicateKeysStreamsValues(t *testing.T) {
	const huge = 64 << 20
	body := `{"a":"` + strings.Repeat("x", huge) + `","b":1}`
	reader := strings.NewReader(body)

	// TotalAlloc is cumulative, so GC activity cannot mask a large
	// transient allocation.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	apiErr := scanDuplicateKeys(context.Background(), reader)
	runtime.ReadMemStats(&after)
	if apiErr != nil {
		t.Fatalf("large value rejected: %q", apiErr.message)
	}

	const ceiling = 1 << 20 // the 64 KiB read buffer plus slack
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > ceiling {
		t.Fatalf("scanning a %d-byte value allocated %d bytes; values are not streaming",
			huge, allocated)
	}

	// The same body with a repeat still rejects, and names the key.
	dup := `{"a":"` + strings.Repeat("x", huge) + `","a":1}`
	apiErr = scanDuplicateKeys(context.Background(), strings.NewReader(dup))
	if apiErr == nil || !strings.Contains(apiErr.message, `repeats property "a"`) {
		t.Fatalf("duplicate past a large value not caught: %v", apiErr)
	}
}

// TestScanDuplicateKeysHonoursCancellation pins that an abandoned scan
// stops early. It runs while holding the single large-body admission
// slot, so walking the rest of a gigabyte spool for a client that has
// gone would block the next large mutation for no reason.
func TestScanDuplicateKeysHonoursCancellation(t *testing.T) {
	body := `{"a":"` + strings.Repeat("x", 8<<20) + `"}`
	counter := &countingReader{r: strings.NewReader(body)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	apiErr := scanDuplicateKeys(ctx, counter)
	if apiErr == nil {
		t.Fatal("cancelled scan reported success")
	}
	if counter.n >= len(body) {
		t.Fatalf("cancelled scan consumed the whole body (%d of %d bytes)", counter.n, len(body))
	}
}

// TestByteAtChecksCancellationOnTheInterval pins WHERE the disconnect
// test falls. The earlier cancellation test only proves a cancelled
// scan stops somewhere; a check that fired one byte late — or never,
// because the counter ran the wrong way — would still pass it while
// costing a gigabyte spool's worth of scanning.
func TestByteAtChecksCancellationOnTheInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name  string
		start int
		stops bool
	}{
		{name: "one byte short of the interval", start: cancelCheckBytes - 2},
		{name: "exactly on the interval", start: cancelCheckBytes - 1, stops: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &jsonLexer{br: bufio.NewReader(strings.NewReader("xx")), ctx: ctx, read: tc.start}
			_, err := l.byteAt()
			switch {
			case tc.stops && err == nil:
				t.Fatalf("byte %d did not test for a disconnect", tc.start+1)
			case !tc.stops && err != nil:
				t.Fatalf("byte %d tested for a disconnect early: %v", tc.start+1, err)
			}
		})
	}
}

// TestScanDuplicateKeysBoundsDepth straddles maxScanDepth from both
// sides. The cap exists to match encoding/json's own limit, so it has
// to reject deeper AND accept everything the decoder would take — a cap
// that fires one level early silently rejects valid payloads.
func TestScanDuplicateKeysBoundsDepth(t *testing.T) {
	nest := map[string]func(int) string{
		"arrays":  func(d int) string { return strings.Repeat("[", d) + strings.Repeat("]", d) },
		"objects": func(d int) string { return strings.Repeat(`{"a":`, d) + `1` + strings.Repeat("}", d) },
	}
	for kind, build := range nest {
		t.Run(kind+" at the limit", func(t *testing.T) {
			if apiErr := scanDuplicateKeys(context.Background(),
				strings.NewReader(build(maxScanDepth))); apiErr != nil {
				t.Fatalf("%d levels rejected: %q", maxScanDepth, apiErr.message)
			}
		})
		t.Run(kind+" one past the limit", func(t *testing.T) {
			apiErr := scanDuplicateKeys(context.Background(),
				strings.NewReader(build(maxScanDepth+1)))
			if apiErr == nil {
				t.Fatalf("%d levels accepted", maxScanDepth+1)
			}
			if !strings.Contains(apiErr.message, "nests deeper") {
				t.Fatalf("expected a nesting rejection, got %q", apiErr.message)
			}
		})
	}
}

// TestScanDuplicateKeysReleasesDepth pins that depth is given back as
// each container closes. Without that, siblings accumulate and a
// shallow-but-wide payload — a list of records, the commonest shape
// this API receives — trips a cap meant for runaway NESTING.
func TestScanDuplicateKeysReleasesDepth(t *testing.T) {
	// Enough siblings that an unreleased level would breach the cap:
	// each unreleased container costs two levels instead of none.
	const siblings = maxScanDepth/2 + 1
	bodies := map[string]string{
		"sibling arrays":  "[" + strings.TrimSuffix(strings.Repeat("[],", siblings), ",") + "]",
		"sibling objects": "[" + strings.TrimSuffix(strings.Repeat("{},", siblings), ",") + "]",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if apiErr := scanDuplicateKeys(context.Background(), strings.NewReader(body)); apiErr != nil {
				t.Fatalf("%d shallow siblings rejected: %q", siblings, apiErr.message)
			}
		})
	}
}

// TestScanDuplicateKeysBudgetStraddlesTheLimit pins the budget at its
// edge, in raw bytes, and pins that names already held count toward it.
// The fat-object test overshoots the budget several times over, which
// proves a breach is caught but says nothing about where the line is.
func TestScanDuplicateKeysBudgetStraddlesTheLimit(t *testing.T) {
	// A name's raw form carries its two quotes, so an n-byte name costs
	// n+2 against the budget.
	cases := []struct {
		name     string
		body     string
		rejected bool
	}{
		{
			name: "one name whose raw form exactly fills the budget",
			body: `{"` + strings.Repeat("x", maxKeyMemory-2) + `":1}`,
		},
		{
			name:     "one name whose raw form is a byte over",
			body:     `{"` + strings.Repeat("x", maxKeyMemory-1) + `":1}`,
			rejected: true,
		},
		{
			// The outer name is still live inside the nested object, so
			// it is the SUM that breaches — the inner name alone fits.
			name: "a held name pushes a fitting name over",
			body: `{"` + strings.Repeat("x", 1000) + `":{"` +
				strings.Repeat("y", maxKeyMemory-1001) + `":1}}`,
			rejected: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := scanDuplicateKeys(context.Background(), strings.NewReader(tc.body))
			switch {
			case tc.rejected && apiErr == nil:
				t.Fatal("a body over the budget was accepted")
			case tc.rejected && !strings.Contains(apiErr.message, "scan budget"):
				t.Fatalf("expected a budget rejection, got %q", apiErr.message)
			case !tc.rejected && apiErr != nil:
				t.Fatalf("a body exactly at the budget was rejected: %q", apiErr.message)
			}
		})
	}
}

// TestScanDuplicateKeysNamesTrailingData pins that data after the value
// is reported as trailing data rather than as a read failure. The two
// arms of that check are a byte apart in the source and produce
// indistinguishable rejections unless the message is asserted.
func TestScanDuplicateKeysNamesTrailingData(t *testing.T) {
	apiErr := scanDuplicateKeys(context.Background(), strings.NewReader(`{"a":1} {"b":2}`))
	if apiErr == nil {
		t.Fatal("trailing data accepted")
	}
	if !strings.Contains(apiErr.message, "carries trailing data") {
		t.Fatalf("expected a trailing-data rejection, got %q", apiErr.message)
	}
}

type countingReader struct {
	r *strings.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
