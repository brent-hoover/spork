package api

import "testing"

// canonicalID is a well-formed lowercase uuidv7: version nibble 7 at
// index 14, variant 8 at index 19, and one of each boundary hex digit
// ('0', '9', 'a', 'f') somewhere in it.
const canonicalID = "0123456f-9abc-7def-8012-3456789abcde"

// TestIsUUID pins the character rule identifiers are held to. Every
// door that takes an id in a request body reaches the store through
// this one check, and the acceptance suite only ever sends ids the
// server itself minted — all of which are canonical, so no scenario
// can reach the far side of any of these comparisons. Widen one and
// two spellings of a uuid become two rows naming one entity; narrow
// one and ordinary ids stop resolving.
func TestIsUUID(t *testing.T) {
	sub := func(index int, c byte) string {
		out := []byte(canonicalID)
		out[index] = c
		return string(out)
	}
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"the canonical form", canonicalID, true},
		{"lowest hex digit", sub(0, '0'), true},
		{"highest hex digit", sub(0, '9'), true},
		{"lowest hex letter", sub(0, 'a'), true},
		{"highest hex letter", sub(0, 'f'), true},

		{"just below the digits", sub(0, '/'), false},
		{"just above the digits", sub(0, ':'), false},
		{"just below the hex letters", sub(0, '`'), false},
		{"just above the hex letters", sub(0, 'g'), false},

		// The contract fixes identifiers at the canonical LOWERCASE
		// form: uppercase is a different text value for the same uuid,
		// and every layer below compares ids as case-sensitive text.
		{"uppercase hex is not the canonical form", sub(0, 'A'), false},
		{"uppercase at the top of the hex letters", sub(0, 'F'), false},

		{"a hyphen where a hex digit belongs", sub(0, '-'), false},
		{"a hex digit where a hyphen belongs", sub(8, '0'), false},
		{"the second group's hyphen", sub(13, '0'), false},
		{"the third group's hyphen", sub(18, '0'), false},
		{"the fourth group's hyphen", sub(23, '0'), false},

		{"one character short", canonicalID[:35], false},
		{"one character long", canonicalID + "0", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUUID(tc.in); got != tc.want {
				t.Fatalf("isUUID(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalUUIDv7 pins the extra two nibbles import demands on top
// of the shape: ids arrive from a payload rather than from this
// server's minting, and a record id is written verbatim and re-exported
// forever, so a v4 or a reserved-variant id would outlive the import.
func TestCanonicalUUIDv7(t *testing.T) {
	sub := func(index int, c byte) string {
		out := []byte(canonicalID)
		out[index] = c
		return string(out)
	}
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"a canonical uuidv7", canonicalID, true},
		{"variant 9", sub(19, '9'), true},
		{"variant a", sub(19, 'a'), true},
		{"variant b", sub(19, 'b'), true},

		{"version 4 is not v7", sub(14, '4'), false},
		{"version 8 is not v7", sub(14, '8'), false},
		{"variant 7 is below the RFC 9562 range", sub(19, '7'), false},
		{"variant c is above the RFC 9562 range", sub(19, 'c'), false},

		{"a shape isUUID already refuses", "not-a-uuid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalUUIDv7(tc.in); got != tc.want {
				t.Fatalf("canonicalUUIDv7(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
