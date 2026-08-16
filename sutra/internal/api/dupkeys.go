package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Duplicate JSON properties are ambiguous: encoding/json keeps the last
// value, so the server could store one value while a different consumer
// of the same bytes reads another. Thread transcripts and event payloads
// make that worse — they are stored VERBATIM and re-served, so the
// ambiguity outlives the request (review 1900).
//
// EVERY mutating body is scanned, not just imports: a transcript the
// API accepts must survive export and re-import unchanged, so both
// doors have to apply the same rule (review 1902).
//
// The scan is a lexer, not a decode: it retains only the property names
// of the objects currently OPEN, never a value. A transcript with a
// million turns costs one turn's key set, because each closes before
// the next opens.
const (
	// maxScanDepth matches encoding/json's own nesting limit, so the
	// scan never rejects a payload the decoder would have accepted.
	maxScanDepth = 10000
	// maxKeyMemory bounds the TOTAL property-name bytes retained across
	// every open object — the only figure that actually bounds the
	// scan. Per-object caps do not: 65,536 keys of 1 MiB each is 64 GiB
	// (review 1902). Real records carry a handful of short names, and
	// arbitrary transcript JSON would need a megabyte of names live on
	// ONE nesting path to reach this.
	maxKeyMemory = 1 << 20
)

// cancelCheckBytes is how often the scan tests for a disconnect. A
// gigabyte spool divides into ~16k checks — frequent enough that an
// abandoned scan stops promptly, rare enough to cost nothing.
const cancelCheckBytes = 64 << 10

// scanDuplicateKeys walks one JSON value, rejecting an object that
// repeats a property name at ANY depth. It reads the body once and
// holds no values. An EMPTY body passes: whether a body is required is
// the handler's business, not this scan's.
//
// The scan runs while holding the single large-body admission slot, so
// it honours cancellation: a client that hangs up mid-scan must not
// keep a gigabyte spool's worth of scanning — and the slot — from the
// next large mutation (review 1906).
func scanDuplicateKeys(ctx context.Context, body io.Reader) *apiError {
	l := &jsonLexer{br: bufio.NewReaderSize(body, 64<<10), ctx: ctx}
	if _, err := l.peek(); errors.Is(err, io.EOF) {
		return nil
	}
	if err := l.value(); err != nil {
		return dupScanError(err)
	}
	// Trailing data after the value is malformed; the decoders reject
	// it too, and agreeing here keeps the two in step.
	if _, err := l.next(); !errors.Is(err, io.EOF) {
		if err != nil {
			return dupScanError(err)
		}
		return badBody("carries trailing data")
	}
	return nil
}

func badBody(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "bad-request",
		message: "malformed request body: " + fmt.Sprintf(format, args...)}
}

func dupScanError(err error) *apiError {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return badBody("ended mid-value")
	}
	var dup *duplicateKeyError
	if errors.As(err, &dup) {
		return badBody("object repeats property %q", dup.key)
	}
	return badBody("%v", err)
}

type duplicateKeyError struct{ key string }

func (e *duplicateKeyError) Error() string { return "duplicate property " + e.key }

// jsonLexer walks JSON structure byte by byte, keeping no values.
type jsonLexer struct {
	br       *bufio.Reader
	ctx      context.Context
	depth    int
	keyBytes int // property-name bytes live across all open objects
	read     int // bytes since the last cancellation check
}

// byteAt reads one byte, testing for a disconnect every
// cancelCheckBytes so an abandoned scan stops instead of walking the
// rest of the spool.
func (l *jsonLexer) byteAt() (byte, error) {
	if l.read++; l.read >= cancelCheckBytes {
		l.read = 0
		if l.ctx != nil && l.ctx.Err() != nil {
			return 0, l.ctx.Err()
		}
	}
	return l.br.ReadByte()
}

// next returns the next significant byte.
func (l *jsonLexer) next() (byte, error) {
	for {
		c, err := l.byteAt()
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
	held := 0
	defer func() { l.keyBytes -= held }() // this object's names go out of scope
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
		// The post-decode check is NOT redundant with str()'s, which
		// counts RAW bytes. Decoding usually shrinks a name — two quote
		// bytes at minimum, five more per \uXXXX — and that reasoning is
		// what made this look unkillable. It is wrong for invalid UTF-8:
		// encoding/json replaces every bad byte with U+FFFD, three bytes
		// each, so a name comfortably inside the raw cap can decode to
		// nearly three times it. Measured: a 1044480-byte raw name
		// decoded to 3133440 bytes and was admitted while this check was
		// absent (reviews 2011/2012). str() bounds what is READ; this
		// bounds what is RETAINED, and only the second is the promise
		// maxKeyMemory makes.
		if l.keyBytes += len(key); l.keyBytes > maxKeyMemory {
			return fmt.Errorf("property names exceed the %d-byte scan budget", maxKeyMemory)
		}
		held += len(key)
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
		c, err := l.byteAt()
		if err != nil {
			return "", err
		}
		if keep {
			raw = append(raw, c)
			// One name cannot outgrow the whole budget.
			if l.keyBytes+len(raw) > maxKeyMemory {
				return "", fmt.Errorf("property names exceed the %d-byte scan budget", maxKeyMemory)
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
		c, err := l.byteAt()
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
