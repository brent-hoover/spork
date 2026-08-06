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

	issueSet := map[string]issues.Issue{}
	reviewList := []review.Review{}

	// Text matches over issues, project-scoped when given.
	if q != nil {
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
		for _, p := range scope {
			matched, err := issues.List(tx, p, issues.Filters{Q: *q})
			if err != nil {
				writeError(w, issueErrorFrom(err))
				return
			}
			for _, i := range matched {
				issueSet[i.ID] = i
			}
		}
	}

	// Session joins: reviews by any submission, plus their issues.
	if session != nil {
		matched, err := review.List(tx, "", "", *session)
		if err != nil {
			writeError(w, reviewErrorFrom(err))
			return
		}
		reviewList = matched
		for _, rev := range matched {
			iss, err := issues.Get(tx, rev.Issue)
			if err != nil {
				writeError(w, issueErrorFrom(err))
				return
			}
			if project == nil || iss.Project == *project {
				issueSet[iss.ID] = iss
			}
		}
	}

	// Threads: native q/session/project composition; issue-anchored
	// session threads pull their issues in too.
	threadList := []threads.Thread{}
	if q != nil || session != nil {
		matched, err := threads.Search(tx, q, session, project)
		if err != nil {
			writeError(w, errorFrom(err))
			return
		}
		threadList = matched
		if session != nil {
			for _, t := range matched {
				if t.Issue == nil {
					continue
				}
				iss, err := issues.Get(tx, *t.Issue)
				if err != nil {
					writeError(w, issueErrorFrom(err))
					return
				}
				issueSet[iss.ID] = iss
			}
		}
	}

	// Docs carry no session; they join on text alone.
	docList := []docs.Document{}
	if q != nil {
		matched, err := docs.Search(tx, project, *q)
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

	issueList := make([]issues.Issue, 0, len(issueSet))
	for _, i := range issueSet {
		issueList = append(issueList, i)
	}
	sortIssues(issueList)

	threadsRaw, apiErr := threadListJSON(threadList)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	// Threads splice verbatim transcripts, so the envelope assembles
	// around their pre-built bytes.
	envelope := struct {
		FeedWatermark string          `json:"feed_watermark"`
		Issues        []issues.Issue  `json:"issues"`
		Documents     []docs.Document `json:"documents"`
		Reviews       []review.Review `json:"reviews"`
	}{watermark, issueList, docList, reviewList}
	raw, err := json.Marshal(envelope)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	body := append(raw[:len(raw)-1], []byte(`,"threads":`)...)
	body = append(body, threadsRaw...)
	body = append(body, '}')
	writeJSON(w, http.StatusOK, json.RawMessage(body))
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

func sortIssues(list []issues.Issue) {
	slices.SortFunc(list, func(a, b issues.Issue) int {
		if a.Project != b.Project {
			return strings.Compare(a.Project, b.Project)
		}
		return int(a.Number - b.Number)
	})
}
