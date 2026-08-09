package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cucumber/godog"
)

// searchWorld drives the REQ-search scenarios.
type searchWorld struct {
	iw *issueWorld
	cw *closeWorld

	matching    map[string]string // rank kind (title/body/comment) -> issue id
	nonMatching string
	filterHits  []string // ids expected from the three-way filter
	textHit     string   // the one also matching the text term

	crossDoc    string
	crossThread string
	crossIssue  string

	// Every review the session reaches. A slice because "the session's
	// work" is a set: with one member, a search that streams all of it
	// and one that stops after the first return the same bytes.
	sessionReviews  []string
	unrelatedReview string
	sessionThread   string
	sessionIssues   []string

	// Scope rows: the same three kinds in two projects, plus a review in
	// the first, so a widening scope has something to widen onto.
	scopeIssue, scopeDoc, scopeThread, scopeReview string
	otherIssue, otherDoc, otherThread              string
	// Siblings of scopeIssue in the same project. The handler gathers
	// issues into a map before it answers, so the only thing standing
	// between the reader and an arbitrary order is the sort — and one
	// sibling is few enough that an arbitrary order comes out sorted by
	// accident often enough to hide a broken one.
	scopeSiblings []string
}

func (sw *searchWorld) reset() {
	*sw = searchWorld{iw: sw.iw, cw: sw.cw}
}

// createIssueWith mints an issue with explicit body/status/assignee/
// label plumbing on top of the shared world helpers.
func (sw *searchWorld) createIssueWith(title, body, status, assigneeHandle string, labelIDs []string) (string, error) {
	iw := sw.iw
	if err := sw.cw.ensureGitProject(); err != nil {
		return "", err
	}
	actor := iw.identities["operator"]
	payload := map[string]any{"title": title, "actor": actor}
	if body != "" {
		payload["body"] = body
	}
	if assigneeHandle != "" {
		assignee, err := iw.identity(assigneeHandle)
		if err != nil {
			return "", err
		}
		payload["assignee"] = assignee
	}
	if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/issues", payload); err != nil {
		return "", err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return "", err
	}
	for _, labelID := range labelIDs {
		if err := iw.s.call(http.MethodPost, "/issues/"+created.ID+"/labels",
			map[string]string{"label": labelID, "actor": actor}); err != nil {
			return "", err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return "", err
		}
	}
	if status != "" && status != "open" {
		if err := iw.s.call(http.MethodPost, "/issues/"+created.ID+"/status",
			map[string]any{"status": status, "actor": actor}); err != nil {
			return "", err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return "", err
		}
	}
	return created.ID, nil
}

// The scope scenarios need a doc and a thread in each of two projects
// (issues come from issueWorld.createIssueIn). Every other fixture in
// the suite mints its content in the one project the rest of the suite
// works in, so these take the project as an argument.
func (sw *searchWorld) createDocIn(projectID, title, content string) (string, error) {
	if err := sw.iw.s.call(http.MethodPost, "/projects/"+projectID+"/documents",
		map[string]string{"title": title, "content": content, "author": sw.iw.identities["operator"]}); err != nil {
		return "", err
	}
	return sw.iw.createdID()
}

func (sw *searchWorld) createThreadIn(projectID, title, text string) (string, error) {
	transcript, err := json.Marshal([]map[string]string{{"speaker": "claude", "text": text}})
	if err != nil {
		return "", err
	}
	if err := sw.iw.s.call(http.MethodPost, "/threads", map[string]any{
		"title": title, "transcript": json.RawMessage(transcript),
		"project": projectID, "actor": sw.iw.identities["operator"]}); err != nil {
		return "", err
	}
	return sw.iw.createdID()
}

func (sw *searchWorld) createLabel(name string) (string, error) {
	iw := sw.iw
	if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": name}); err != nil {
		return "", err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (sw *searchWorld) listIssueIDs(query string) ([]string, error) {
	iw := sw.iw
	if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/issues"+query, nil); err != nil {
		return nil, err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return nil, err
	}
	var page struct {
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(page.Issues))
	for _, i := range page.Issues {
		ids = append(ids, i.ID)
	}
	return ids, nil
}

func registerSearchSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	sw := &searchWorld{iw: iw, cw: cw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		sw.reset()
		return ctx, nil
	})

	// --- text search over issues
	sc.Step(`^issues exist mentioning "([^"]*)" in title, body, or comments$`, func(term string) error {
		sw.matching = map[string]string{}
		// Created in REVERSE rank order — comment hit first, title hit
		// last — so the issue numbers run opposite to the ranking. Built
		// in rank order, issue number and rank are the same sequence, and
		// a search that never ranked at all would satisfy the assertion
		// below just as well as one that did.
		commentHit, err := sw.createIssueWith("nightly failure", "no details yet", "", "", nil)
		if err != nil {
			return err
		}
		author, err := iw.identity("claude")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/comments",
			map[string]any{"issue": commentHit, "author": author, "body": "the " + term + " needs a rebuild"}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		sw.matching["comment"] = commentHit
		bodyHit, err := sw.createIssueWith("parser regression", "traced to the "+term+" table", "", "", nil)
		if err != nil {
			return err
		}
		sw.matching["body"] = bodyHit
		titleHit, err := sw.createIssueWith("the "+term+" crashes", "", "", "", nil)
		if err != nil {
			return err
		}
		sw.matching["title"] = titleHit
		other, err := sw.createIssueWith("unrelated work", "nothing to see", "", "", nil)
		if err != nil {
			return err
		}
		sw.nonMatching = other
		return nil
	})
	sc.Step(`^project "SUT" is searched for "([^"]*)"$`, func(term string) error {
		_, err := sw.listIssueIDs("?q=" + url.QueryEscape(term))
		return err
	})
	sc.Step(`^those issues are returned ranked$`, func() error {
		var page struct {
			Issues []struct {
				ID string `json:"id"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		pos := map[string]int{}
		for idx, i := range page.Issues {
			pos[i.ID] = idx
			if i.ID == sw.nonMatching {
				return fmt.Errorf("non-matching issue returned")
			}
		}
		for kind, id := range sw.matching {
			if _, ok := pos[id]; !ok {
				return fmt.Errorf("%s-matched issue missing from results", kind)
			}
		}
		if pos[sw.matching["title"]] >= pos[sw.matching["body"]] || pos[sw.matching["body"]] >= pos[sw.matching["comment"]] {
			return fmt.Errorf("ranking wrong: title=%d body=%d comment=%d",
				pos[sw.matching["title"]], pos[sw.matching["body"]], pos[sw.matching["comment"]])
		}
		return nil
	})

	// --- filters compose
	sc.Step(`^a mix of issues in project "SUT"$`, func() error {
		bug, err := sw.createLabel("bug")
		if err != nil {
			return err
		}
		full, err := sw.createIssueWith("all filters and text", "alpha tokenizer notes", "open", "claude", []string{bug})
		if err != nil {
			return err
		}
		sw.textHit = full
		plain, err := sw.createIssueWith("all filters no text", "plain body", "open", "claude", []string{bug})
		if err != nil {
			return err
		}
		sw.filterHits = []string{full, plain}
		if _, err := sw.createIssueWith("wrong label", "alpha tokenizer notes", "open", "claude", nil); err != nil {
			return err
		}
		if _, err := sw.createIssueWith("wrong assignee", "", "open", "human-brent", []string{bug}); err != nil {
			return err
		}
		if _, err := sw.createIssueWith("wrong status", "", "in-progress", "claude", []string{bug}); err != nil {
			return err
		}
		return nil
	})
	sc.Step(`^issues are listed with status "([^"]*)", assignee "([^"]*)", label "([^"]*)"$`, func(status, handle, label string) error {
		assignee := iw.identities[handle]
		ids, err := sw.listIssueIDs("?status=" + status + "&assignee=" + assignee + "&label=" + url.QueryEscape(label))
		if err != nil {
			return err
		}
		if len(ids) != len(sw.filterHits) {
			return fmt.Errorf("expected %d issues, got %d: %v", len(sw.filterHits), len(ids), ids)
		}
		for _, want := range sw.filterHits {
			if !strings.Contains(strings.Join(ids, ","), want) {
				return fmt.Errorf("issue %s missing from filtered list", want)
			}
		}
		return nil
	})
	sc.Step(`^only issues matching all three are returned$`, func() error {
		return nil // asserted in the listing step, where the filter values are in scope
	})
	sc.Step(`^adding a text term narrows the same result set$`, func() error {
		assignee := iw.identities["claude"]
		ids, err := sw.listIssueIDs("?status=open&assignee=" + assignee + "&label=bug&q=tokenizer")
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != sw.textHit {
			return fmt.Errorf("text term did not narrow to the matching issue: %v", ids)
		}
		return nil
	})

	// --- one surface over all content
	sc.Step(`^a doc and a thread in "SUT" mention "([^"]*)"$`, func(phrase string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		author := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
			map[string]string{"title": "ops runbook", "content": "our " + phrase + " is exponential backoff", "author": author}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var doc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
			return err
		}
		sw.crossDoc = doc.ID
		transcript, err := json.Marshal([]map[string]string{{"speaker": "claude", "text": "we should revisit the " + phrase}})
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
			"title": "retry discussion", "transcript": json.RawMessage(transcript),
			"project": iw.project, "actor": author}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var thread struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &thread); err != nil {
			return err
		}
		sw.crossThread = thread.ID
		issueID, err := sw.createIssueWith("flaky uploads", "the "+phrase+" masks the real bug", "", "", nil)
		if err != nil {
			return err
		}
		sw.crossIssue = issueID
		return nil
	})
	sc.Step(`^"SUT" is searched for "([^"]*)" across all content$`, func(phrase string) error {
		if err := iw.s.call(http.MethodGet, "/search?project="+iw.project+"&q="+url.QueryEscape(phrase), nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the doc and the thread appear alongside matching issues$`, func() error {
		var results struct {
			Issues    []struct{ ID string }
			Documents []struct{ ID string }
			Threads   []struct{ ID string }
		}
		if err := json.Unmarshal(iw.s.lastBody, &results); err != nil {
			return err
		}
		body := string(iw.s.lastBody)
		for name, id := range map[string]string{"doc": sw.crossDoc, "thread": sw.crossThread, "issue": sw.crossIssue} {
			if !strings.Contains(body, id) {
				return fmt.Errorf("%s %s missing from search results", name, id)
			}
		}
		return nil
	})

	// --- session id joins an instance's work
	sc.Step(`^a review whose revision 1 was submitted under session "([^"]*)" and revision 2 under session "([^"]*)"$`, func(s1, s2 string) error {
		if err := sw.reviewWithSession("SUT-1", "sessioned", s1); err != nil {
			return err
		}
		sw.sessionReviews = append(sw.sessionReviews, cw.reviews["SUT-1"].id)
		sw.sessionIssues = append(sw.sessionIssues, iw.issues["SUT-1"])
		if err := cw.verdict("SUT-1", "changes-requested"); err != nil {
			return err
		}
		ref := cw.reviews["SUT-1"]
		sha, err := cw.mintCommit()
		if err != nil {
			return err
		}
		author := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/resubmit", map[string]any{
			"author": author, "expected_revision": ref.revision, "expected_verdict_event": ref.verdictEvent,
			"branch": "sessioned", "commit": sha, "session": s2,
		}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^an unrelated review whose submissions never carried "([^"]*)"$`, func(string) error {
		if err := sw.reviewWithSession("SUT-2", "other", "sess-99"); err != nil {
			return err
		}
		sw.unrelatedReview = cw.reviews["SUT-2"].id
		return nil
	})
	sc.Step(`^a second review under session "([^"]*)"$`, func(session string) error {
		if err := sw.reviewWithSession("SUT-4", "also-sessioned", session); err != nil {
			return err
		}
		// Appended after the first, and review ids are UUIDv7, so this is
		// the row that sorts last under ORDER BY r.id — exactly the one a
		// listing that stops after its first row drops.
		sw.sessionReviews = append(sw.sessionReviews, cw.reviews["SUT-4"].id)
		sw.sessionIssues = append(sw.sessionIssues, iw.issues["SUT-4"])
		return nil
	})
	sc.Step(`^an imported thread carrying session "([^"]*)"$`, func(session string) error {
		transcript, err := json.Marshal([]map[string]string{{"speaker": "claude", "text": "instance notes"}})
		if err != nil {
			return err
		}
		issueID, err := iw.ensureIssue("SUT-3")
		if err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
			"title": "instance transcript", "transcript": json.RawMessage(transcript),
			"session": session, "issue": issueID, "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var thread struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &thread); err != nil {
			return err
		}
		sw.sessionThread = thread.ID
		sw.sessionIssues = append(sw.sessionIssues, issueID)
		return nil
	})
	sc.Step(`^"([^"]*)" is searched$`, func(session string) error {
		if err := iw.s.call(http.MethodGet, "/search?session="+url.QueryEscape(session), nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the reviews, the thread, and their linked issues are returned$`, func() error {
		body := string(iw.s.lastBody)
		for _, id := range sw.sessionReviews {
			if !strings.Contains(body, id) {
				return fmt.Errorf("session review %s missing", id)
			}
		}
		if !strings.Contains(body, sw.sessionThread) {
			return fmt.Errorf("session thread missing")
		}
		for _, id := range sw.sessionIssues {
			if !strings.Contains(body, id) {
				return fmt.Errorf("linked issue %s missing", id)
			}
		}
		return nil
	})
	sc.Step(`^the unrelated review is not returned$`, func() error {
		if strings.Contains(string(iw.s.lastBody), sw.unrelatedReview) {
			return fmt.Errorf("unrelated review leaked into session results")
		}
		return nil
	})
	sc.Step(`^listing reviews filtered by session "([^"]*)" also returns the reviews and excludes the unrelated one$`, func(session string) error {
		if err := iw.s.call(http.MethodGet, "/reviews?session="+url.QueryEscape(session), nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		body := string(iw.s.lastBody)
		for _, id := range sw.sessionReviews {
			if !strings.Contains(body, id) {
				return fmt.Errorf("session review %s missing from listing", id)
			}
		}
		if strings.Contains(body, sw.unrelatedReview) {
			return fmt.Errorf("unrelated review present in listing")
		}
		return nil
	})
	sc.Step(`^scoping that same session search to a project the work never touched returns nothing$`, func() error {
		elsewhere, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodGet, "/search?session=sess-42&project="+url.QueryEscape(elsewhere), nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		body := string(iw.s.lastBody)
		for _, id := range append(append([]string{}, sw.sessionReviews...), sw.sessionThread) {
			if strings.Contains(body, id) {
				return fmt.Errorf("session content %s survived a scope its work never touched", id)
			}
		}
		for _, id := range sw.sessionIssues {
			if strings.Contains(body, id) {
				return fmt.Errorf("issue %s survived a scope it does not belong to", id)
			}
		}
		return nil
	})

	// --- each filter omitted widens the scope
	sc.Step(`^content in "SUT" and a second project, both mentioning "([^"]*)"$`, func(phrase string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		var err error
		if sw.scopeIssue, err = iw.createIssueIn(iw.project, "flaky uploads", "the "+phrase+" masks the real bug"); err != nil {
			return err
		}
		// Siblings in the SAME project: ordering within a project has
		// nothing to compare until several of its issues are here.
		for _, title := range []string{"duplicate uploads", "the " + phrase + " loops", "uploads stall"} {
			sibling, err := iw.createIssueIn(iw.project, title, "another "+phrase+" symptom")
			if err != nil {
				return err
			}
			sw.scopeSiblings = append(sw.scopeSiblings, sibling)
		}
		if sw.scopeDoc, err = sw.createDocIn(iw.project, "ops runbook", "our "+phrase+" is exponential backoff"); err != nil {
			return err
		}
		if sw.scopeThread, err = sw.createThreadIn(iw.project, "retry discussion", "we should revisit the "+phrase); err != nil {
			return err
		}
		if err := sw.reviewWithSession("SUT-1", "scoped", "sess-scope"); err != nil {
			return err
		}
		sw.scopeReview = cw.reviews["SUT-1"].id

		other, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		if sw.otherIssue, err = iw.createIssueIn(other, "slow restores", "their "+phrase+" is too eager"); err != nil {
			return err
		}
		if sw.otherDoc, err = sw.createDocIn(other, "restore notes", "the "+phrase+" we inherited"); err != nil {
			return err
		}
		sw.otherThread, err = sw.createThreadIn(other, "restore chat", "about the "+phrase)
		return err
	})
	sc.Step(`^the search names (.+)$`, func(filters string) error {
		query := ""
		switch strings.TrimSpace(filters) {
		case "nothing at all":
		case "only the term":
			query = "?q=" + url.QueryEscape("retry policy")
		case "only the project":
			query = "?project=" + iw.project
		default:
			return fmt.Errorf("unknown filter set %q", filters)
		}
		if err := iw.s.call(http.MethodGet, "/search"+query, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^it returns (.+)$`, func(scope string) error {
		var want map[string]string
		unwanted := map[string]string{}
		switch strings.TrimSpace(scope) {
		case "every issue, review, doc, and thread from both projects":
			want = map[string]string{
				"the issue": sw.scopeIssue,
				"the doc":   sw.scopeDoc, "the thread": sw.scopeThread,
				"the review": sw.scopeReview, "the far issue": sw.otherIssue,
				"the far doc": sw.otherDoc, "the far thread": sw.otherThread,
			}
		case "the matching issues, docs, and threads from both projects, and no reviews":
			want = map[string]string{
				"the issue": sw.scopeIssue,
				"the doc":   sw.scopeDoc, "the thread": sw.scopeThread,
				"the far issue": sw.otherIssue, "the far doc": sw.otherDoc, "the far thread": sw.otherThread,
			}
			unwanted = map[string]string{"the review": sw.scopeReview}
		case `everything in "SUT" and nothing from the second project`:
			want = map[string]string{
				"the issue": sw.scopeIssue,
				"the doc":   sw.scopeDoc, "the thread": sw.scopeThread,
				"the review": sw.scopeReview,
			}
			unwanted = map[string]string{
				"the far issue": sw.otherIssue, "the far doc": sw.otherDoc, "the far thread": sw.otherThread,
			}
		default:
			return fmt.Errorf("unknown scope %q", scope)
		}
		// Every row keeps the siblings: they live in "SUT" and mention the
		// term, so no filter in the table excludes any of them.
		for idx, id := range sw.scopeSiblings {
			want[fmt.Sprintf("sibling %d", idx+1)] = id
		}
		body := string(iw.s.lastBody)
		for name, id := range want {
			if !strings.Contains(body, id) {
				return fmt.Errorf("%s is missing — the scope did not widen to it", name)
			}
		}
		for name, id := range unwanted {
			if strings.Contains(body, id) {
				return fmt.Errorf("%s came back — the scope was wider than the filters asked for", name)
			}
		}
		return nil
	})
	sc.Step(`^each project's issues arrive together and in ascending number order$`, func() error {
		var page struct {
			Issues []struct {
				Project string `json:"project"`
				Number  int64  `json:"number"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		if len(page.Issues) < 2 {
			return fmt.Errorf("ordering needs at least two issues, got %d", len(page.Issues))
		}
		seen := map[string]bool{}
		for idx, cur := range page.Issues {
			if idx == 0 {
				seen[cur.Project] = true
				continue
			}
			prev := page.Issues[idx-1]
			if cur.Project == prev.Project {
				if cur.Number <= prev.Number {
					return fmt.Errorf("issue %d of the same project came back at number %d after %d",
						idx, cur.Number, prev.Number)
				}
				continue
			}
			if seen[cur.Project] {
				return fmt.Errorf("project %s resumes at position %d after another project interrupted it",
					cur.Project, idx)
			}
			seen[cur.Project] = true
		}
		return nil
	})
}

// reviewWithSession mirrors closeWorld.createReview with a session
// stamp, registering the ref under the issue name for verdict reuse.
func (sw *searchWorld) reviewWithSession(issueName, branch, session string) error {
	cw := sw.cw
	iw := sw.iw
	if err := cw.ensureGitProject(); err != nil {
		return err
	}
	issueID, err := iw.ensureIssue(issueName)
	if err != nil {
		return err
	}
	sha, err := cw.mintCommit()
	if err != nil {
		return err
	}
	author := iw.identities["operator"]
	if err := iw.s.call(http.MethodPost, "/reviews", map[string]string{
		"issue": issueID, "author": author, "branch": branch, "commit": sha, "session": session,
	}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return err
	}
	cw.reviews[issueName] = reviewRef{id: created.ID, revision: created.Revision, commit: sha}
	return nil
}
