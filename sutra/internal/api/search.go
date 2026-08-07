package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"sutra/internal/docs"
	"sutra/internal/events"
	"sutra/internal/issues"
	"sutra/internal/review"
	"sutra/internal/threads"
)

// search is the one query surface over issues, docs, and transcripts
// (AC-search-cross). A session query joins an instance's work: reviews
// any of whose submissions carry the session, threads imported from
// it, and the issues those anchor to (AC-search-session). Group
// results are assembled in one transaction with the feed watermark.
func (s *server) search(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	optional := func(name string) *string {
		if !params.Has(name) {
			return nil
		}
		v := params.Get(name)
		return &v
	}
	q, project, session := optional("q"), optional("project"), optional("session")

	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Issues dedup as ids with a light sort key; bodies are unbounded,
	// so rows stream at write time — never accumulated.
	type issueKey struct {
		project string
		number  int64
	}
	issueSet := map[string]issueKey{}
	noteRef := func(r issues.Ref) { issueSet[r.ID] = issueKey{r.Project, r.Number} }
	reviewIDs := []string{}

	// Text matches over issues, project-scoped when given. With q
	// omitted, the contract requires ALL content in the remaining
	// scope — a project when set, otherwise EVERYTHING.
	bare := q == nil && project == nil && session == nil
	if (q != nil && session == nil) || (project != nil && session == nil) || bare {
		scope := []string{}
		if project != nil {
			scope = append(scope, *project)
		} else {
			all, err := listProjectIDs(tx)
			if err != nil {
				writeError(w, errorFrom(err))
				return
			}
			scope = all
		}
		filter := issues.Filters{}
		if q != nil {
			filter.Q = *q
		}
		for _, p := range scope {
			matched, err := issues.SearchIDs(tx, p, filter)
			if err != nil {
				writeError(w, issueErrorFrom(err))
				return
			}
			for _, r := range matched {
				noteRef(r)
			}
		}
	}

	// Reviews enumerate under a project-only or bare query — all
	// content in scope when q is omitted.
	if (project != nil || bare) && session == nil && q == nil {
		for id := range issueSet {
			err := review.EachRef(tx, id, "", "", func(rv review.Ref) error {
				reviewIDs = append(reviewIDs, rv.ID)
				return nil
			})
			if err != nil {
				writeError(w, reviewErrorFrom(err))
				return
			}
		}
	}

	// Deciding whether a session-reached issue belongs in the result
	// needs its project and number — never its body or labels — and
	// several reviews or threads commonly name the SAME issue, so each
	// verdict is computed once from a light reference and memoized
	// (review 1920). Full issues load only while streaming survivors.
	type issueVerdict struct {
		ref issues.Ref
		ok  bool
	}
	verdicts := map[string]issueVerdict{}
	verdictFor := func(issueID string) (issueVerdict, error) {
		if v, seen := verdicts[issueID]; seen {
			return v, nil
		}
		ref, err := issues.RefByID(tx, issueID)
		if err != nil {
			return issueVerdict{}, err
		}
		v := issueVerdict{ref: ref, ok: true}
		if project != nil && ref.Project != *project {
			v.ok = false
		}
		if v.ok && q != nil {
			// Filters intersect: a session issue failing the text term
			// drops, and so does the review that led to it.
			match, err := issueMatchesQ(tx, ref.ID, *q)
			if err != nil {
				return issueVerdict{}, err
			}
			v.ok = match
		}
		verdicts[issueID] = v
		return v, nil
	}

	// A session filter defines the scope; a text term then narrows
	// WITHIN it (filters intersect, never union) via issueMatchesQ.
	// Session joins: reviews by any submission, plus their issues —
	// both confined to the project scope when one is given.
	if session != nil {
		err := review.EachRef(tx, "", "", *session, func(rev review.Ref) error {
			v, err := verdictFor(rev.Issue)
			if err != nil {
				return err
			}
			if !v.ok {
				return nil
			}
			reviewIDs = append(reviewIDs, rev.ID)
			noteRef(v.ref)
			return nil
		})
		if err != nil {
			writeError(w, reviewErrorFrom(err))
			return
		}
	}

	// Threads: native q/session/project composition. This pass collects
	// only ids — transcripts stream to the wire later, never
	// accumulating in memory.
	threadIDs := []string{}
	if q != nil || session != nil || project != nil || bare {
		err := threads.SearchRefsEach(tx, q, session, project, func(t threads.Ref) error {
			threadIDs = append(threadIDs, t.ID)
			return nil
		})
		if err != nil {
			writeError(w, issueErrorFrom(err))
			return
		}
	}

	// Issues reached through session threads are a SEPARATE question,
	// so they get a separate enumeration. q narrows the ISSUE here, not
	// the thread that led to it: a session thread whose linked issue
	// matches the term must contribute that issue even when the thread's
	// own title and transcript do not match, and filtering the threads
	// by q first would drop it before the issue was ever examined
	// (review 1916).
	if session != nil {
		err := threads.SearchRefsEach(tx, nil, session, project, func(t threads.Ref) error {
			if t.Issue == nil {
				return nil
			}
			v, err := verdictFor(*t.Issue)
			if err != nil {
				return err
			}
			if v.ok {
				noteRef(v.ref)
			}
			return nil
		})
		if err != nil {
			writeError(w, issueErrorFrom(err))
			return
		}
	}

	// Docs carry no session; they join on text, or enumerate wholly
	// under a project-only query. IDS only: titles are unbounded, so the
	// records load one at a time at write time like every other group
	// (review 1918).
	docIDs := []string{}
	switch {
	case q != nil && session == nil:
		matched, err := docs.SearchIDs(tx, project, *q)
		if err != nil {
			writeError(w, docErrorFrom(err))
			return
		}
		docIDs = matched
	case project != nil && session == nil:
		matched, err := docs.IDsByProject(tx, *project)
		if err != nil {
			writeError(w, docErrorFrom(err))
			return
		}
		docIDs = matched
	case bare:
		// Nothing is written yet, so a scope failure is reportable: an
		// empty scope here would serve 200 with a silently incomplete
		// documents group (review 1904).
		projectIDs, err := listProjectIDs(tx)
		if err != nil {
			writeError(w, errorFrom(err))
			return
		}
		for _, p := range projectIDs {
			matched, err := docs.IDsByProject(tx, p)
			if err != nil {
				writeError(w, docErrorFrom(err))
				return
			}
			docIDs = append(docIDs, matched...)
		}
	}

	watermark, err := events.Watermark(tx)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}

	issueIDs := make([]string, 0, len(issueSet))
	for id := range issueSet {
		issueIDs = append(issueIDs, id)
	}
	slices.SortFunc(issueIDs, func(a, b string) int {
		ka, kb := issueSet[a], issueSet[b]
		if ka.project != kb.project {
			return strings.Compare(ka.project, kb.project)
		}
		return int(ka.number - kb.number)
	})

	// Only the watermark marshals up front; EVERY group streams row by
	// row inside the envelope — document titles, issue bodies, review
	// summaries and transcripts are all unbounded, so one record is
	// resident at a time.
	envelope := struct {
		FeedWatermark string `json:"feed_watermark"`
	}{watermark}
	raw, err := json.Marshal(envelope)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw[:len(raw)-1])
	_, _ = w.Write([]byte(`,"documents":[`))
	for i, id := range docIDs {
		doc, err := docs.Get(tx, id)
		if err != nil {
			return // truncation is the only signal after the first byte
		}
		one, err := json.Marshal(doc)
		if err != nil {
			return
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write(one)
	}
	// Reviews stream one row at a time — summaries are unbounded.
	_, _ = w.Write([]byte(`],"reviews":[`))
	slices.Sort(reviewIDs)
	for i, id := range reviewIDs {
		rv, err := review.Get(tx, id)
		if err != nil {
			return // truncation is the only signal after the first byte
		}
		one, err := json.Marshal(rv)
		if err != nil {
			return
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write(one)
	}
	_, _ = w.Write([]byte(`],"issues":[`))
	for i, id := range issueIDs {
		issue, err := issues.Get(tx, id)
		if err != nil {
			return // truncation is the only signal after the first byte
		}
		one, err := json.Marshal(issue)
		if err != nil {
			return
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write(one)
	}
	_, _ = w.Write([]byte(`],"threads":[`))
	for i, id := range threadIDs {
		t, err := threads.Get(tx, id)
		if err != nil {
			return
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		if apiErr := writeThreadJSON(w, t); apiErr != nil {
			return
		}
	}
	_, _ = w.Write([]byte(`]}`))
}

// issueMatchesQ reports whether one issue text-matches the term —
// the intersection primitive for q composed with session scope.
func issueMatchesQ(tx *sql.Tx, issueID, q string) (bool, error) {
	term := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	var one int
	err := tx.QueryRow(`
		SELECT 1 FROM issues WHERE id = ? AND (title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM comments c WHERE c.issue = issues.id AND c.body LIKE ? ESCAPE '\'))`,
		issueID, term, term, term).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func listProjectIDs(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM projects`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
