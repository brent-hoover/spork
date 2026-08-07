package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Duplicate JSON properties are ambiguous: encoding/json keeps the last
// value, so an import could store one value while a different consumer
// of the same bytes reads another. walkImport rejects repeats among the
// envelope's own keys, but records decode straight into structs, where
// a repeat silently wins. Event payloads and thread transcripts make
// that worse — they are stored VERBATIM and re-served, so an ambiguous
// object outlives the import (review 1900).
//
// The scan is a lexer over the spooled body, not a decode: it retains
// only the key names of the objects currently OPEN, never a value. A
// transcript with a million turns costs one turn's key set, because
// each closes before the next opens.
const (
	// maxScanDepth matches encoding/json's own nesting limit, so the
	// scan never rejects a payload the decoder would have accepted.
	maxScanDepth = 10000
	// maxObjectKeys bounds one object's key set. Every schema in the
	// contract is closed (additionalProperties: false) and the importer
	// runs DisallowUnknownFields, so a real record has a handful of
	// properties; transcripts nest arbitrary JSON, but an object with
	// more keys than this is a memory attack, not a payload.
	maxObjectKeys = 1 << 16
	// maxKeyBytes bounds one property name. Longer names appear only
	// inside verbatim transcripts and payloads, where they are still
	// JSON objects whose keys this scan must compare in full — a
	// truncated comparison would collide two distinct long names.
	maxKeyBytes = 1 << 20
)

// scanDuplicateKeys walks the whole JSON value, rejecting an object
// that repeats a property name at ANY depth. It reads the body once
// and holds no values.
func scanDuplicateKeys(body io.Reader) *apiError {
	l := &jsonLexer{br: bufio.NewReaderSize(body, 64<<10)}
	if err := l.value(); err != nil {
		return dupScanError(err)
	}
	// A trailing value after the payload is malformed; walkImport
	// rejects it too, and agreeing here keeps the two in step.
	if _, err := l.next(); !errors.Is(err, io.EOF) {
		if err != nil {
			return dupScanError(err)
		}
		return malformedImport("export payload carries trailing data")
	}
	return nil
}

func dupScanError(err error) *apiError {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformedImport("export payload ended mid-value")
	}
	var dup *duplicateKeyError
	if errors.As(err, &dup) {
		return &apiError{status: http.StatusBadRequest, code: "bad-request",
			message: fmt.Sprintf("malformed request body: object repeats property %q", dup.key)}
	}
	return malformedImport("scan export payload: %v", err)
}

type duplicateKeyError struct{ key string }

func (e *duplicateKeyError) Error() string { return "duplicate property " + e.key }

// jsonLexer walks JSON structure byte by byte, keeping no values.
type jsonLexer struct {
	br    *bufio.Reader
	depth int
}

// next returns the next significant byte.
func (l *jsonLexer) next() (byte, error) {
	for {
		c, err := l.br.ReadByte()
		if err != nil {
			return 0, err
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return c, nil
	}
}

func (l *jsonLexer) peek() (byte, error) {
	c, err := l.next()
	if err != nil {
		return 0, err
	}
	return c, l.br.UnreadByte()
}

// value consumes exactly one JSON value.
func (l *jsonLexer) value() error {
	c, err := l.next()
	if err != nil {
		return err
	}
	switch c {
	case '{':
		return l.object()
	case '[':
		return l.array()
	case '"':
		_, err := l.str(false)
		return err
	default:
		// Numbers, true, false, null: consume the literal's bytes. Their
		// syntax is the decoder's business — this scan only needs to
		// know where the value ends.
		return l.literal()
	}
}

// object consumes an object, rejecting any repeated property name.
func (l *jsonLexer) object() error {
	if l.depth++; l.depth > maxScanDepth {
		return fmt.Errorf("payload nests deeper than %d levels", maxScanDepth)
	}
	defer func() { l.depth-- }()

	seen := map[string]bool{}
	c, err := l.peek()
	if err != nil {
		return err
	}
	if c == '}' {
		_, err := l.next()
		return err
	}
	for {
		c, err := l.next()
		if err != nil {
			return err
		}
		if c != '"' {
			return fmt.Errorf("object property name expected, found %q", string(c))
		}
		key, err := l.str(true)
		if err != nil {
			return err
		}
		if seen[key] {
			return &duplicateKeyError{key: key}
		}
		if len(seen) >= maxObjectKeys {
			return fmt.Errorf("object carries more than %d properties", maxObjectKeys)
		}
		seen[key] = true
		if c, err = l.next(); err != nil {
			return err
		} else if c != ':' {
			return fmt.Errorf("expected ':' after property %q", key)
		}
		if err := l.value(); err != nil {
			return err
		}
		c, err = l.next()
		if err != nil {
			return err
		}
		switch c {
		case ',':
			continue
		case '}':
			return nil
		default:
			return fmt.Errorf("expected ',' or '}' in object, found %q", string(c))
		}
	}
}

func (l *jsonLexer) array() error {
	if l.depth++; l.depth > maxScanDepth {
		return fmt.Errorf("payload nests deeper than %d levels", maxScanDepth)
	}
	defer func() { l.depth-- }()

	c, err := l.peek()
	if err != nil {
		return err
	}
	if c == ']' {
		_, err := l.next()
		return err
	}
	for {
		if err := l.value(); err != nil {
			return err
		}
		c, err := l.next()
		if err != nil {
			return err
		}
		switch c {
		case ',':
			continue
		case ']':
			return nil
		default:
			return fmt.Errorf("expected ',' or ']' in array, found %q", string(c))
		}
	}
}

// str consumes a string after its opening quote. VALUES pass keep
// false and stream past without retaining a byte — that is what keeps
// an unbounded issue body or transcript out of memory. Property names
// pass keep true: their raw bytes are retained escapes-and-all and
// decoded by encoding/json, so a \u-escaped name compares exactly as
// the second-pass decoder will read it (matching scanExplicitNulls).
func (l *jsonLexer) str(keep bool) (string, error) {
	raw := []byte{'"'}
	escaped := false
	for {
		c, err := l.br.ReadByte()
		if err != nil {
			return "", err
		}
		if keep {
			raw = append(raw, c)
			if len(raw) > maxKeyBytes {
				return "", fmt.Errorf("property name longer than %d bytes", maxKeyBytes)
			}
		}
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			if !keep {
				return "", nil
			}
			var key string
			if err := json.Unmarshal(raw, &key); err != nil {
				return "", fmt.Errorf("undecodable property name: %v", err)
			}
			return key, nil
		}
	}
}

// literal consumes a number, true, false, or null — anything up to the
// next structural byte.
func (l *jsonLexer) literal() error {
	for {
		c, err := l.br.ReadByte()
		if err != nil {
			if err == io.EOF {
				return nil // a bare literal may end the payload
			}
			return err
		}
		switch c {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return l.br.UnreadByte()
		}
	}
}
