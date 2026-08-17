package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cucumber/godog"
)

// archiveWorld drives AC-project-archive's read-only half across EVERY
// door rather than one. The AC names a set — "archived projects are
// read-only" — and a scenario that posts one issue discharges it without
// exercising the other twenty-two enforcement points. Species 1 in
// spec-gaps.md, and species 8 is why it matters here in particular: a
// door whose check is missing fails the same way as one whose check is
// present, because the request is refused either way for some other
// reason, so nobody notices which.
type archiveWorld struct {
	iw *issueWorld
	cw *closeWorld

	project    string // the archived project
	liveIssue  string // an issue in a DIFFERENT, still-writable project
	issueA     string // all four are in the archived project
	issueB     string
	issueC     string
	relationAB string
	// A relation with ONE end archived, so removing it from the live end
	// reaches the destination guard instead of being turned away by the
	// source guard first.
	crossRelation string
	label         string
	document      string
	docVersion    string
	thread        string
	liveThread    string // anchored outside, so re-anchoring aims INTO the freeze
	actor         string
	reviewer      string
	commit        string

	// Three reviews, because the verdict, consume, and resubmit doors
	// each demand a DIFFERENT state and no single review can be pending,
	// approved, and changes-requested at once. Reaching a door in the
	// wrong state gets a 409 for the wrong reason, which is precisely
	// the confusion this scenario exists to remove.
	open, approved, changesRequested reviewFixture
}

type reviewFixture struct {
	id           string
	revision     int64
	verdictEvent string
}

func (aw *archiveWorld) reset() { *aw = archiveWorld{iw: aw.iw, cw: aw.cw} }

// door is one write into the archived project. Each returns a request
// the server must refuse; none may 404, because a door that cannot find
// its target has not been shown to refuse anything.
type door struct {
	method string
	path   func(aw *archiveWorld) string
	body   func(aw *archiveWorld) any
}

func noBody(*archiveWorld) any { return nil }

// doors is keyed by the name the feature file uses, so the Gherkin table
// IS the enumeration and the two cannot drift apart unnoticed — the step
// below fails if either side holds a name the other does not.
var doors = map[string]door{
	"create an issue": {http.MethodPost,
		func(aw *archiveWorld) string { return "/projects/" + aw.project + "/issues" },
		func(aw *archiveWorld) any { return map[string]string{"title": "after the freeze", "actor": aw.actor} }},
	"update an issue": {http.MethodPatch,
		func(aw *archiveWorld) string { return "/issues/" + aw.issueA },
		func(aw *archiveWorld) any { return map[string]string{"title": "renamed", "actor": aw.actor} }},
	"change an issue's status": {http.MethodPost,
		func(aw *archiveWorld) string { return "/issues/" + aw.issueA + "/status" },
		func(aw *archiveWorld) any { return map[string]string{"status": "in-progress", "actor": aw.actor} }},
	"assign an issue": {http.MethodPost,
		func(aw *archiveWorld) string { return "/issues/" + aw.issueA + "/assign" },
		func(aw *archiveWorld) any { return map[string]string{"assignee": aw.actor, "actor": aw.actor} }},

	// Relations are guarded at BOTH ends: the archived project can be the
	// side the relation is written from, or the side it points at. One
	// scenario cannot see the difference, and a missing check on either
	// end is a lever for writing into a frozen project from a live one.
	"add a relation from an archived issue": {http.MethodPost,
		func(aw *archiveWorld) string { return "/issues/" + aw.issueA + "/relations" },
		func(aw *archiveWorld) any {
			return map[string]string{"kind": "blocks", "to": aw.liveIssue, "actor": aw.actor}
		}},
	"add a relation to an archived issue": {http.MethodPost,
		func(aw *archiveWorld) string { return "/issues/" + aw.liveIssue + "/relations" },
		func(aw *archiveWorld) any {
			return map[string]string{"kind": "blocks", "to": aw.issueA, "actor": aw.actor}
		}},
	"remove a relation from an archived issue": {http.MethodDelete,
		func(aw *archiveWorld) string {
			return "/issues/" + aw.issueA + "/relations/" + aw.relationAB + "?actor=" + aw.actor
		}, noBody},
	// Deleted from the LIVE side of a relation that crosses the boundary.
	// Aiming this at a relation with both ends archived would be refused
	// by the source guard before the destination guard was reached — the
	// same masking the whole scenario exists to end (review 2012).
	"remove a relation to an archived issue": {http.MethodDelete,
		func(aw *archiveWorld) string {
			return "/issues/" + aw.liveIssue + "/relations/" + aw.crossRelation + "?actor=" + aw.actor
		}, noBody},

	"attach a label": {http.MethodPost,
		func(aw *archiveWorld) string { return "/issues/" + aw.issueA + "/labels" },
		func(aw *archiveWorld) any { return map[string]string{"label": aw.label, "actor": aw.actor} }},
	"detach a label": {http.MethodDelete,
		func(aw *archiveWorld) string {
			return "/issues/" + aw.issueA + "/labels/" + aw.label + "?actor=" + aw.actor
		}, noBody},

	"create a document": {http.MethodPost,
		func(aw *archiveWorld) string { return "/projects/" + aw.project + "/documents" },
		func(aw *archiveWorld) any {
			return map[string]string{"title": "after the freeze", "content": "x", "author": aw.actor}
		}},
	"save a document version": {http.MethodPost,
		func(aw *archiveWorld) string { return "/documents/" + aw.document + "/versions" },
		func(aw *archiveWorld) any { return map[string]string{"content": "revised", "author": aw.actor} }},
	"link a document to an issue": {http.MethodPost,
		func(aw *archiveWorld) string { return "/documents/" + aw.document + "/issue" },
		func(aw *archiveWorld) any { return map[string]string{"issue": aw.issueB, "actor": aw.actor} }},
	"unlink a document from an issue": {http.MethodDelete,
		func(aw *archiveWorld) string { return "/documents/" + aw.document + "/issue?actor=" + aw.actor },
		noBody},

	// A comment carries exactly one of three anchors, and each resolves
	// its project by a different route.
	"comment on an issue": {http.MethodPost,
		func(*archiveWorld) string { return "/comments" },
		func(aw *archiveWorld) any {
			return map[string]any{"issue": aw.issueA, "author": aw.actor, "body": "after the freeze"}
		}},
	"comment on a document version": {http.MethodPost,
		func(*archiveWorld) string { return "/comments" },
		func(aw *archiveWorld) any {
			return map[string]any{"doc_version": aw.docVersion, "author": aw.actor, "body": "after the freeze"}
		}},
	"comment on a review": {http.MethodPost,
		func(*archiveWorld) string { return "/comments" },
		func(aw *archiveWorld) any {
			return map[string]any{"review": aw.open.id, "review_revision": aw.open.revision,
				"author": aw.actor, "body": "after the freeze"}
		}},

	"open a review": {http.MethodPost,
		func(*archiveWorld) string { return "/reviews" },
		func(aw *archiveWorld) any {
			return map[string]string{"issue": aw.issueB, "author": aw.actor,
				"branch": "post-freeze", "commit": aw.commit}
		}},
	"record a review verdict": {http.MethodPost,
		func(aw *archiveWorld) string { return "/reviews/" + aw.open.id + "/verdict" },
		func(aw *archiveWorld) any {
			return map[string]any{"verdict": "approved", "actor": aw.reviewer, "revision": aw.open.revision}
		}},
	"consume a review approval": {http.MethodPost,
		func(aw *archiveWorld) string { return "/reviews/" + aw.approved.id + "/consume" },
		func(aw *archiveWorld) any {
			return map[string]any{"actor": aw.actor, "expected_revision": aw.approved.revision,
				"expected_verdict_event": aw.approved.verdictEvent}
		}},
	"resubmit a review": {http.MethodPost,
		func(aw *archiveWorld) string { return "/reviews/" + aw.changesRequested.id + "/resubmit" },
		func(aw *archiveWorld) any {
			return map[string]any{"author": aw.actor, "commit": aw.commit, "branch": "post-freeze",
				"expected_revision":      aw.changesRequested.revision,
				"expected_verdict_event": aw.changesRequested.verdictEvent}
		}},

	// A thread anchors to a project OR an issue, and re-anchoring checks
	// the anchor it is leaving as well as the one it is joining.
	"import a project-anchored thread": {http.MethodPost,
		func(*archiveWorld) string { return "/threads" },
		func(aw *archiveWorld) any {
			return map[string]any{"title": "after the freeze", "project": aw.project, "actor": aw.actor,
				"transcript": json.RawMessage(`[{"speaker":"claude","text":"x"}]`)}
		}},
	"import an issue-anchored thread": {http.MethodPost,
		func(*archiveWorld) string { return "/threads" },
		func(aw *archiveWorld) any {
			return map[string]any{"title": "after the freeze", "issue": aw.issueA, "actor": aw.actor,
				"transcript": json.RawMessage(`[{"speaker":"claude","text":"x"}]`)}
		}},
	"re-anchor a thread into the archived project": {http.MethodPost,
		func(aw *archiveWorld) string { return "/threads/" + aw.liveThread + "/anchor" },
		func(aw *archiveWorld) any { return map[string]any{"issue": aw.issueA, "actor": aw.actor} }},
	// The other direction, which is a different guard: moving a thread
	// OUT checks the anchor being left, not the one being joined. Only
	// this door reaches guardCurrentAnchor (review 2012).
	"re-anchor a thread out of the archived project": {http.MethodPost,
		func(aw *archiveWorld) string { return "/threads/" + aw.thread + "/anchor" },
		func(aw *archiveWorld) any { return map[string]any{"issue": aw.liveIssue, "actor": aw.actor} }},
}

func registerArchiveSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	aw := &archiveWorld{iw: cw.iw, cw: cw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		aw.reset()
		return ctx, nil
	})

	sc.Step(`^archived project "OTH" holds an issue, a relation, a label, a document, a review, and a thread$`,
		func() error { return aw.populateThenFreeze() })

	sc.Step(`^every write into "OTH" is refused$`, func(table *godog.Table) error {
		named, err := doorNames(table)
		if err != nil {
			return err
		}
		if err := sameDoors(named); err != nil {
			return err
		}
		for _, name := range named {
			d := doors[name]
			if err := aw.iw.s.call(d.method, d.path(aw), d.body(aw)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if aw.iw.s.lastResp.StatusCode != http.StatusConflict {
				return fmt.Errorf("%s: expected 409 into an archived project, got %d — body %s",
					name, aw.iw.s.lastResp.StatusCode, aw.iw.s.lastBody)
			}
			if err := aw.iw.s.expectErrorCode("project-archived"); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	})

	sc.Step(`^every read of "OTH" still works$`, func() error {
		for _, path := range []string{
			"/projects/" + aw.project,
			"/projects/" + aw.project + "/issues",
			"/projects/" + aw.project + "/documents",
			"/issues/" + aw.issueA,
			"/issues/" + aw.issueA + "/relations",
			"/documents/" + aw.document,
			"/documents/" + aw.document + "/versions",
			"/reviews/" + aw.open.id,
			"/reviews/" + aw.approved.id,
			"/reviews/" + aw.changesRequested.id,
			"/threads/" + aw.thread,
		} {
			if err := aw.iw.s.call(http.MethodGet, path, nil); err != nil {
				return err
			}
			if err := aw.iw.s.expectStatus(http.StatusOK); err != nil {
				return fmt.Errorf("reading %s after archive: %w", path, err)
			}
		}
		return nil
	})
}

func doorNames(table *godog.Table) ([]string, error) {
	if len(table.Rows) < 2 {
		return nil, fmt.Errorf("the door table names nothing")
	}
	out := make([]string, 0, len(table.Rows)-1)
	for _, row := range table.Rows[1:] {
		out = append(out, row.Cells[0].Value)
	}
	return out, nil
}

// sameDoors keeps the feature file and this registry honest about each
// other. Without it, deleting a table row would silently stop asserting
// a door, and adding a door here without a row would assert nothing —
// which is the very failure this scenario exists to end.
func sameDoors(named []string) error {
	seen := map[string]bool{}
	for _, name := range named {
		if _, ok := doors[name]; !ok {
			return fmt.Errorf("the feature names door %q, which the step registry does not implement", name)
		}
		if seen[name] {
			return fmt.Errorf("the feature names door %q twice", name)
		}
		seen[name] = true
	}
	for name := range doors {
		if !seen[name] {
			return fmt.Errorf("door %q is implemented but the feature never names it", name)
		}
	}
	return nil
}

// populateThenFreeze fills the project with a target for every door and
// only then archives it. Order matters: each fixture below is created
// while the project is still writable, because a door needs something
// real to aim at — a 404 proves nothing about the freeze.
func (aw *archiveWorld) populateThenFreeze() error {
	iw, cw := aw.iw, aw.cw
	// A git-backed live project first: reviews need a repo and a real
	// commit, and the relation and re-anchor doors need a live issue on
	// the other side of the boundary.
	if err := cw.ensureGitProject(); err != nil {
		return err
	}
	actor, err := iw.identity("operator")
	if err != nil {
		return err
	}
	aw.actor = actor
	live, err := iw.ensureIssue("SUT-1")
	if err != nil {
		return err
	}
	aw.liveIssue = live

	// The archived project is git-backed too, so opening a review inside
	// it reaches the freeze check rather than failing for want of a repo.
	if err := iw.s.call(http.MethodPost, "/projects", map[string]string{
		"key": "OTH", "name": "Other", "actor": actor, "repo_path": cw.repoPath}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	if aw.project, err = idOf(iw.s.lastBody); err != nil {
		return err
	}

	for _, target := range []*string{&aw.issueA, &aw.issueB, &aw.issueC} {
		if err := iw.s.call(http.MethodPost, "/projects/"+aw.project+"/issues",
			map[string]string{"title": "pre-freeze", "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		if *target, err = idOf(iw.s.lastBody); err != nil {
			return err
		}
	}

	if err := iw.s.call(http.MethodPost, "/issues/"+aw.issueA+"/relations",
		map[string]string{"kind": "blocks", "to": aw.issueB, "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	// The relation response nests its subject under "relation" beside the
	// operation id, unlike the flat creates above.
	var rel struct {
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &rel); err != nil {
		return fmt.Errorf("decode relation: %w", err)
	}
	if aw.relationAB = rel.Relation.ID; aw.relationAB == "" {
		return fmt.Errorf("relation response carries no id: %s", iw.s.lastBody)
	}

	// A second relation crossing the boundary, minted from the live side
	// while both ends are still writable.
	if err := iw.s.call(http.MethodPost, "/issues/"+aw.liveIssue+"/relations",
		map[string]string{"kind": "blocks", "to": aw.issueC, "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	if err := json.Unmarshal(iw.s.lastBody, &rel); err != nil {
		return fmt.Errorf("decode cross relation: %w", err)
	}
	if aw.crossRelation = rel.Relation.ID; aw.crossRelation == "" {
		return fmt.Errorf("cross relation response carries no id: %s", iw.s.lastBody)
	}

	if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": "frozen"}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	if aw.label, err = idOf(iw.s.lastBody); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/issues/"+aw.issueA+"/labels",
		map[string]string{"label": aw.label, "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}

	if err := iw.s.call(http.MethodPost, "/projects/"+aw.project+"/documents",
		map[string]any{"title": "frozen doc", "content": "before", "author": actor, "issue": aw.issueA}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var doc struct {
		ID      string `json:"id"`
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	aw.document, aw.docVersion = doc.ID, doc.Version.ID

	reviewer, err := iw.identity("human-brent")
	if err != nil {
		return err
	}
	aw.reviewer = reviewer
	sha, err := cw.mintCommit()
	if err != nil {
		return err
	}
	aw.commit = sha
	// One review per state the doors need. Each door must be well formed
	// and correctly staged, because the guard runs AFTER the request's
	// own validation: a 400 for a missing field, or a 409 for the wrong
	// review state, would look like a refusal while proving nothing
	// about the freeze.
	if aw.open, err = aw.review(aw.issueA, "open", ""); err != nil {
		return err
	}
	if aw.approved, err = aw.review(aw.issueB, "approved", "approved"); err != nil {
		return err
	}
	if aw.changesRequested, err = aw.review(aw.issueC, "cr", "changes-requested"); err != nil {
		return err
	}

	if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
		"title": "frozen thread", "project": aw.project, "actor": actor,
		"transcript": json.RawMessage(`[{"speaker":"claude","text":"before"}]`)}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	if aw.thread, err = idOf(iw.s.lastBody); err != nil {
		return err
	}
	// Anchored OUTSIDE, so re-anchoring it is a write aimed INTO the
	// freeze rather than one already inside it.
	if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
		"title": "live thread", "issue": aw.liveIssue, "actor": actor,
		"transcript": json.RawMessage(`[{"speaker":"claude","text":"outside"}]`)}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	if aw.liveThread, err = idOf(iw.s.lastBody); err != nil {
		return err
	}

	if err := iw.s.call(http.MethodPost, "/projects/"+aw.project+"/archive",
		map[string]string{"actor": actor}); err != nil {
		return err
	}
	return iw.s.expectStatus(http.StatusOK)
}

// review opens a review on an issue of the archived project and, when a
// verdict is named, drives it to that state — all before the freeze.
func (aw *archiveWorld) review(issue, branch, verdict string) (reviewFixture, error) {
	var f reviewFixture
	s := aw.iw.s
	sha, err := aw.cw.mintCommit()
	if err != nil {
		return f, err
	}
	if err := s.call(http.MethodPost, "/reviews", map[string]string{
		"issue": issue, "author": aw.actor, "branch": branch, "commit": sha}); err != nil {
		return f, err
	}
	if err := s.expectStatus(http.StatusCreated); err != nil {
		return f, err
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(s.lastBody, &created); err != nil {
		return f, fmt.Errorf("decode review: %w", err)
	}
	f.id, f.revision = created.ID, created.Revision
	if verdict == "" {
		return f, nil
	}
	if err := s.call(http.MethodPost, "/reviews/"+f.id+"/verdict", map[string]any{
		"verdict": verdict, "revision": f.revision, "actor": aw.reviewer}); err != nil {
		return f, err
	}
	if err := s.expectStatus(http.StatusOK); err != nil {
		return f, err
	}
	var judged struct {
		Revision           int64  `json:"revision"`
		LatestVerdictEvent string `json:"latest_verdict_event"`
	}
	if err := json.Unmarshal(s.lastBody, &judged); err != nil {
		return f, fmt.Errorf("decode verdict: %w", err)
	}
	f.revision, f.verdictEvent = judged.Revision, judged.LatestVerdictEvent
	return f, nil
}

func idOf(body []byte) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("decode id: %w — body %s", err, body)
	}
	if created.ID == "" {
		return "", fmt.Errorf("response carries no id: %s", body)
	}
	return created.ID, nil
}
