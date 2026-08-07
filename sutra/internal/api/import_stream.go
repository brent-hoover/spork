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
		if required[key] {
			// A repeated property would let one pass validate the last
			// value while the other pass processes BOTH.
			return malformedImport("export payload repeats field %q", key)
		}
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
				// The pointer shadow distinguishes an OMITTED
				// subtree_revision from a valid zero — omitting the
				// required field would silently reset the concurrency
				// fence.
				var v struct {
					issues.Issue
					SubtreeRevision *int64          `json:"subtree_revision"`
					Labels          json.RawMessage `json:"labels"`
				}
				if err := dec.Decode(&v); err != nil {
					return err
				}
				if v.SubtreeRevision == nil {
					return fmt.Errorf("issue %s omits subtree_revision", v.ID)
				}
				v.Issue.SubtreeRevision = *v.SubtreeRevision
				if string(v.Labels) == "null" {
					// The closed Issue schema permits only an array
					// when labels is present; a null would vanish on
					// re-export.
					return fmt.Errorf("issue %s labels must not be null", v.ID)
				}
				if len(v.Labels) > 0 {
					if err := json.Unmarshal(v.Labels, &v.Issue.Labels); err != nil {
						return err
					}
				}
				return cb.issue(v.Issue)
			})
		case "comments":
			apiErr = walkArray(dec, key, func() error {
				// Optional anchor fields are non-nullable under the
				// omit-when-absent convention; raw shadows distinguish
				// explicit null (reject) from absence (nil).
				var v struct {
					comments.Comment
					Issue          json.RawMessage `json:"issue"`
					DocVersion     json.RawMessage `json:"doc_version"`
					Review         json.RawMessage `json:"review"`
					ReviewRevision json.RawMessage `json:"review_revision"`
					Parent         json.RawMessage `json:"parent"`
					Anchor         json.RawMessage `json:"anchor"`
				}
				if err := dec.Decode(&v); err != nil {
					return err
				}
				fields := map[string]json.RawMessage{"issue": v.Issue, "doc_version": v.DocVersion,
					"review": v.Review, "review_revision": v.ReviewRevision, "parent": v.Parent, "anchor": v.Anchor}
				for name, raw := range fields {
					if string(raw) == "null" {
						return fmt.Errorf("comment field %q must not be null", name)
					}
				}
				assign := func(dst **string, raw json.RawMessage) error {
					if len(raw) == 0 {
						return nil
					}
					var s string
					if err := json.Unmarshal(raw, &s); err != nil {
						return err
					}
					*dst = &s
					return nil
				}
				if err := assign(&v.Comment.Issue, v.Issue); err != nil {
					return err
				}
				if err := assign(&v.Comment.DocVersion, v.DocVersion); err != nil {
					return err
				}
				if err := assign(&v.Comment.Review, v.Review); err != nil {
					return err
				}
				if err := assign(&v.Comment.Parent, v.Parent); err != nil {
					return err
				}
				if err := assign(&v.Comment.Anchor, v.Anchor); err != nil {
					return err
				}
				if len(v.ReviewRevision) > 0 {
					var n int64
					if err := json.Unmarshal(v.ReviewRevision, &n); err != nil {
						return err
					}
					v.Comment.ReviewRevision = &n
				}
				return cb.comment(v.Comment)
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
				var v struct {
					threads.Thread
					Project json.RawMessage `json:"project"`
					Issue   json.RawMessage `json:"issue"`
				}
				if err := dec.Decode(&v); err != nil {
					return err
				}
				for name, raw := range map[string]json.RawMessage{"project": v.Project, "issue": v.Issue} {
					if string(raw) == "null" {
						return fmt.Errorf("thread field %q must not be null", name)
					}
				}
				assign := func(dst **string, raw json.RawMessage) error {
					if len(raw) == 0 {
						return nil
					}
					var s string
					if err := json.Unmarshal(raw, &s); err != nil {
						return err
					}
					*dst = &s
					return nil
				}
				if err := assign(&v.Thread.Project, v.Project); err != nil {
					return err
				}
				if err := assign(&v.Thread.Issue, v.Issue); err != nil {
					return err
				}
				return cb.thread(v.Thread)
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
			if sawDocument {
				return fmt.Errorf("documents element repeats the document field")
			}
			var d docs.Document
			if err := dec.Decode(&d); err != nil {
				return err
			}
			sawDocument = true
			if err := cb.document(d); err != nil {
				return err
			}
		case "versions":
			if sawVersions {
				return fmt.Errorf("documents element repeats the versions field")
			}
			sawVersions = true
			vt, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := vt.(json.Delim); !ok || d != '[' {
				return fmt.Errorf("versions must be an array")
			}
			for dec.More() {
				// The pointer shadow distinguishes OMITTED content from
				// a legitimately empty document version.
				var v struct {
					docs.Version
					Content *string `json:"content"`
				}
				if err := dec.Decode(&v); err != nil {
					return err
				}
				if v.Content == nil {
					return fmt.Errorf("doc version %s omits content", v.ID)
				}
				v.Version.Content = *v.Content
				if err := cb.docVersion(v.Version); err != nil {
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
			if sawSubmissions {
				return fmt.Errorf("reviews element repeats the submissions field")
			}
			sawSubmissions = true
			st, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := st.(json.Delim); !ok || d != '[' {
				return fmt.Errorf("submissions must be an array")
			}
			for dec.More() {
				var sub struct {
					review.Submission
					Branch     json.RawMessage `json:"branch"`
					Commit     json.RawMessage `json:"commit"`
					BaseCommit json.RawMessage `json:"base_commit"`
					DocVersion json.RawMessage `json:"doc_version"`
					Session    json.RawMessage `json:"session"`
					Content    json.RawMessage `json:"content"`
				}
				if err := dec.Decode(&sub); err != nil {
					return err
				}
				fields := map[string]json.RawMessage{"branch": sub.Branch, "commit": sub.Commit,
					"base_commit": sub.BaseCommit, "doc_version": sub.DocVersion, "session": sub.Session, "content": sub.Content}
				for name, raw := range fields {
					if string(raw) == "null" {
						return fmt.Errorf("submission field %q must not be null", name)
					}
				}
				assign := func(dst **string, raw json.RawMessage) error {
					if len(raw) == 0 {
						return nil
					}
					var s string
					if err := json.Unmarshal(raw, &s); err != nil {
						return err
					}
					*dst = &s
					return nil
				}
				for _, pair := range []struct {
					dst **string
					raw json.RawMessage
				}{{&sub.Submission.Branch, sub.Branch}, {&sub.Submission.Commit, sub.Commit},
					{&sub.Submission.BaseCommit, sub.BaseCommit}, {&sub.Submission.DocVersion, sub.DocVersion},
					{&sub.Submission.Session, sub.Session}, {&sub.Submission.Content, sub.Content}} {
					if err := assign(pair.dst, pair.raw); err != nil {
						return err
					}
				}
				if err := cb.submission(sub.Submission); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			continue
		}
		if _, dup := scalars[key]; dup {
			return fmt.Errorf("reviews element repeats field %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if string(raw) == "null" {
			// The omit-when-absent convention: no Review property is
			// nullable — a null would vanish on re-export and break
			// the verbatim round trip.
			return fmt.Errorf("review field %q must not be null", key)
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
