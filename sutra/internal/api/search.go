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
	noteIssue := func(i issues.Issue) { issueSet[i.ID] = issueKey{i.Project, i.Number} }
	reviewList := []review.Review{}

	// Text matches over issues, project-scoped when given. With q
	// omitted but a project set, the contract requires ALL content in
	// scope — the empty filter enumerates it.
	if q != nil || (project != nil && session == nil) {
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
			matched, err := issues.List(tx, p, filter)
			if err != nil {
				writeError(w, issueErrorFrom(err))
				return
			}
			for _, i := range matched {
				noteIssue(i)
			}
		}
	}

	// A project-only query enumerates the project's reviews too — all
	// content in scope when q is omitted.
	if project != nil && session == nil && q == nil {
		for id := range issueSet {
			matched, err := review.List(tx, id, "", "")
			if err != nil {
				writeError(w, reviewErrorFrom(err))
				return
			}
			reviewList = append(reviewList, matched...)
		}
	}

	// Session joins: reviews by any submission, plus their issues —
	// both confined to the project scope when one is given.
	if session != nil {
		matched, err := review.List(tx, "", "", *session)
		if err != nil {
			writeError(w, reviewErrorFrom(err))
			return
		}
		for _, rev := range matched {
			iss, err := issues.Get(tx, rev.Issue)
			if err != nil {
				writeError(w, issueErrorFrom(err))
				return
			}
			if project != nil && iss.Project != *project {
				continue
			}
			reviewList = append(reviewList, rev)
			noteIssue(iss)
		}
	}

	// Threads: native q/session/project composition. This first pass
	// collects only ids and anchored issues — transcripts stream to
	// the wire later, never accumulating in memory.
	threadIDs := []string{}
	if q != nil || session != nil || project != nil {
		err := threads.SearchEach(tx, q, session, project, func(t threads.Thread) error {
			threadIDs = append(threadIDs, t.ID)
			if session != nil && t.Issue != nil {
				iss, err := issues.Get(tx, *t.Issue)
				if err != nil {
					return err
				}
				noteIssue(iss)
			}
			return nil
		})
		if err != nil {
			writeError(w, issueErrorFrom(err))
			return
		}
	}

	// Docs carry no session; they join on text, or enumerate wholly
	// under a project-only query.
	docList := []docs.Document{}
	switch {
	case q != nil:
		matched, err := docs.Search(tx, project, *q)
		if err != nil {
			writeError(w, docErrorFrom(err))
			return
		}
		docList = matched
	case project != nil && session == nil:
		matched, err := docs.ListByProject(tx, *project)
		if err != nil {
			writeError(w, docErrorFrom(err))
			return
		}
		docList = matched
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

	// The envelope's bounded groups marshal normally; issues and
	// threads stream row by row inside it — bodies and transcripts are
	// never accumulated, one record re-reads at a time.
	envelope := struct {
		FeedWatermark string          `json:"feed_watermark"`
		Documents     []docs.Document `json:"documents"`
		Reviews       []review.Review `json:"reviews"`
	}{watermark, docList, reviewList}
	raw, err := json.Marshal(envelope)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw[:len(raw)-1])
	_, _ = w.Write([]byte(`,"issues":[`))
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
		one, apiErr := threadJSON(t)
		if apiErr != nil {
			return
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write(one)
	}
	_, _ = w.Write([]byte(`]}`))
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
