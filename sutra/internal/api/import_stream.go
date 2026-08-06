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
	submission func(sub review.Submission) error
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
	required := map[string]bool{"project": false, "identities": false, "issues": false,
		"comments": false, "labels": false, "issue_relations": false, "documents": false,
		"threads": false, "reviews": false, "events": false}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return malformedImport("decode export: %v", err)
		}
		key, _ := keyTok.(string)
		required[key] = true
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
	for key, seen := range required {
		if !seen {
			return malformedImport("export payload omits required field %q", key)
		}
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

// walkDocumentExport streams one {document, versions:[…]} element in
// ANY property order — JSON object order is insignificant. Versions
// fire individually as encountered (each carries its document id), so
// a many-gigabyte history never materializes together; both element
// keys are required per DocumentExport.
func walkDocumentExport(dec *json.Decoder, cb importCallbacks) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("documents element must be an object")
	}
	sawDocument, sawVersions := false, false
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
			sawVersions = true
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
	if !sawDocument || !sawVersions {
		return fmt.Errorf("documents element requires document and versions")
	}
	return nil
}

// walkReviewExport streams one ReviewExport element in ANY property
// order: submissions fire individually as encountered (each carries
// its review id), the bounded metadata collects into a small map and
// fires when the element closes.
func walkReviewExport(dec *json.Decoder, cb importCallbacks) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("reviews element must be an object")
	}
	scalars := map[string]json.RawMessage{}
	sawSubmissions := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		if key == "submissions" {
			sawSubmissions = true
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
				if err := cb.submission(sub); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			continue
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
	if !sawSubmissions {
		return fmt.Errorf("reviews element requires submissions")
	}
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
	return cb.review(r)
}
