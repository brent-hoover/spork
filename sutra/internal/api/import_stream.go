package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"sutra/internal/comments"
	"sutra/internal/docs"
	"sutra/internal/events"
	"sutra/internal/identity"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
	"sutra/internal/threads"
)

// importCallbacks receives records as the walker streams them, one at
// a time — content-bearing records never accumulate. document fires
// before its versions; review (metadata, submissions nil) fires before
// its submissions.
type importCallbacks struct {
	project    func(projects.Project) error
	identity   func(identity.Identity) error
	issue      func(issues.Issue) error
	comment    func(comments.Comment) error
	label      func(issues.Label) error
	relation   func(issues.Relation) error
	document   func(docs.Document) error
	docVersion func(docs.Version) error
	thread     func(threads.Thread) error
	review     func(review.Review) error
	submission func(sub review.Submission, reviewID string) error
	event      func(events.Event) error
}

// walkImport streams a ProjectExport payload element by element. It is
// the single decode path for both import passes: the validation pass
// strips content into sentinels, the insert pass writes rows. Unknown
// fields reject as malformed at every level, and a trailing JSON value
// after the payload rejects too.
func walkImport(body io.Reader, cb importCallbacks) *apiError {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	tok, err := dec.Token()
	if err != nil {
		return malformedImport("decode export: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return malformedImport("export payload must be a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return malformedImport("decode export: %v", err)
		}
		key, _ := keyTok.(string)
		var apiErr *apiError
		switch key {
		case "project":
			var p projects.Project
			if err := dec.Decode(&p); err != nil {
				return malformedImport("decode project: %v", err)
			}
			if err := cb.project(p); err != nil {
				return errorFrom(err)
			}
		case "identities":
			apiErr = walkArray(dec, key, func() error {
				var v identity.Identity
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.identity(v)
			})
		case "issues":
			apiErr = walkArray(dec, key, func() error {
				var v issues.Issue
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.issue(v)
			})
		case "comments":
			apiErr = walkArray(dec, key, func() error {
				var v comments.Comment
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.comment(v)
			})
		case "labels":
			apiErr = walkArray(dec, key, func() error {
				var v issues.Label
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.label(v)
			})
		case "issue_relations":
			apiErr = walkArray(dec, key, func() error {
				var v issues.Relation
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.relation(v)
			})
		case "documents":
			apiErr = walkArray(dec, key, func() error {
				return walkDocumentExport(dec, cb)
			})
		case "threads":
			apiErr = walkArray(dec, key, func() error {
				var v threads.Thread
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.thread(v)
			})
		case "reviews":
			apiErr = walkArray(dec, key, func() error {
				return walkReviewExport(dec, cb)
			})
		case "events":
			apiErr = walkArray(dec, key, func() error {
				var v events.Event
				if err := dec.Decode(&v); err != nil {
					return err
				}
				return cb.event(v)
			})
		default:
			return malformedImport("unknown field %q in export payload", key)
		}
		if apiErr != nil {
			return apiErr
		}
	}
	if _, err := dec.Token(); err != nil { // consume the closing '}'
		return malformedImport("decode export: %v", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return malformedImport("trailing data after the export payload")
	}
	return nil
}

func walkArray(dec *json.Decoder, what string, elem func() error) *apiError {
	tok, err := dec.Token()
	if err != nil {
		return malformedImport("decode %s: %v", what, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return malformedImport("%s must be an array", what)
	}
	for dec.More() {
		if err := elem(); err != nil {
			return malformedImport("decode %s: %v", what, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return malformedImport("decode %s: %v", what, err)
	}
	return nil
}

// walkDocumentExport streams one {document, versions:[…]} element —
// the document fires first, then each version individually, so a
// many-gigabyte version history never materializes together. The
// document key must precede versions (the order our own exports
// produce); buffering versions to tolerate the reverse would defeat
// streaming.
func walkDocumentExport(dec *json.Decoder, cb importCallbacks) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("documents element must be an object")
	}
	sawDocument := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		switch keyTok {
		case "document":
			var d docs.Document
			if err := dec.Decode(&d); err != nil {
				return err
			}
			sawDocument = true
			if err := cb.document(d); err != nil {
				return err
			}
		case "versions":
			if !sawDocument {
				return fmt.Errorf("document key must precede its versions")
			}
			vt, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := vt.(json.Delim); !ok || d != '[' {
				return fmt.Errorf("versions must be an array")
			}
			for dec.More() {
				var v docs.Version
				if err := dec.Decode(&v); err != nil {
					return err
				}
				if err := cb.docVersion(v); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown field %v in documents element", keyTok)
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if !sawDocument {
		return fmt.Errorf("documents element carries no document")
	}
	return nil
}

// walkReviewExport streams one ReviewExport element: metadata fields
// collect into a small map and MUST precede submissions (the order our
// exports produce — buffering submissions would defeat streaming). The
// review callback fires with submissions nil, then each submission
// streams through individually.
func walkReviewExport(dec *json.Decoder, cb importCallbacks) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("reviews element must be an object")
	}
	scalars := map[string]json.RawMessage{}
	reviewFired := false
	var reviewID string
	fireReview := func() error {
		remarshaled, err := json.Marshal(scalars)
		if err != nil {
			return err
		}
		strict := json.NewDecoder(bytes.NewReader(remarshaled))
		strict.DisallowUnknownFields()
		var r review.Review
		if err := strict.Decode(&r); err != nil {
			return fmt.Errorf("review metadata: %w", err)
		}
		reviewFired = true
		reviewID = r.ID
		return cb.review(r)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		if key == "submissions" {
			if !reviewFired {
				if err := fireReview(); err != nil {
					return err
				}
			}
			st, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := st.(json.Delim); !ok || d != '[' {
				return fmt.Errorf("submissions must be an array")
			}
			for dec.More() {
				var sub review.Submission
				if err := dec.Decode(&sub); err != nil {
					return err
				}
				if err := cb.submission(sub, reviewID); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			continue
		}
		if reviewFired {
			return fmt.Errorf("review metadata must precede submissions")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		scalars[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if !reviewFired {
		if err := fireReview(); err != nil {
			return err
		}
	}
	return nil
}
