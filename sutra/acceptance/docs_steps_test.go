package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cucumber/godog"
)

// docsWorld drives the document/template scenarios.
type docsWorld struct {
	iw        *issueWorld
	cw        *closeWorld
	documents map[string]string // title -> id
	templates map[string]string // name -> id
	lastView  struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Issue   *string
		Version struct {
			ID      string `json:"id"`
			Number  int64  `json:"number"`
			Content string `json:"content"`
			Author  string `json:"author"`
			Created string `json:"created"`
		} `json:"version"`
	}
}

func (dw *docsWorld) reset() {
	dw.documents = map[string]string{}
	dw.templates = map[string]string{}
}

// createDoc files a document with explicit content tied optionally to
// an issue name.
func (dw *docsWorld) createDoc(title, content string, issueName, authorHandle string) error {
	iw := dw.iw
	if err := dw.cw.ensureGitProject(); err != nil {
		return err
	}
	author, err := iw.identity(authorHandle)
	if err != nil {
		return err
	}
	body := map[string]any{"title": title, "content": content, "author": author}
	if issueName != "" {
		issueID, err := iw.ensureIssue(issueName)
		if err != nil {
			return err
		}
		body["issue"] = issueID
	}
	if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents", body); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	if err := json.Unmarshal(iw.s.lastBody, &dw.lastView); err != nil {
		return err
	}
	dw.documents[title] = dw.lastView.ID
	return nil
}

func (dw *docsWorld) saveVersion(title, content, authorHandle string) error {
	iw := dw.iw
	author, err := iw.identity(authorHandle)
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/documents/"+dw.documents[title]+"/versions",
		map[string]string{"content": content, "author": author}); err != nil {
		return err
	}
	return iw.s.expectStatus(http.StatusCreated)
}

func (dw *docsWorld) readDoc(title string, version int64) error {
	iw := dw.iw
	path := "/documents/" + dw.documents[title]
	if version != 0 {
		path += fmt.Sprintf("?version=%d", version)
	}
	if err := iw.s.call(http.MethodGet, path, nil); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	return json.Unmarshal(iw.s.lastBody, &dw.lastView)
}

func registerDocsSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	dw := &docsWorld{iw: iw, cw: cw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		dw.reset()
		return ctx, nil
	})

	// --- doc files under its project
	sc.Step(`^project "SUT" and issue (SUT-\d+) exist$`, func(name string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		_, err := iw.ensureIssue(name)
		return err
	})
	sc.Step(`^document "([^"]*)" is created in "SUT" tied to (SUT-\d+) with author "([^"]*)"$`, func(title, issueName, author string) error {
		return dw.createDoc(title, "initial content of "+title, issueName, author)
	})
	sc.Step(`^it is listed under "SUT" and under (SUT-\d+)$`, func(issueName string) error {
		for _, path := range []string{"/projects/" + iw.project + "/documents", "/issues/" + iw.issues[issueName] + "/documents"} {
			if err := iw.s.call(http.MethodGet, path, nil); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
			if !strings.Contains(string(iw.s.lastBody), dw.lastView.ID) {
				return fmt.Errorf("document missing from %s: %s", path, iw.s.lastBody)
			}
		}
		return nil
	})
	sc.Step(`^reading it returns title and content$`, func() error {
		if err := dw.readDoc(dw.lastView.Title, 0); err != nil {
			return err
		}
		if dw.lastView.Title == "" || dw.lastView.Version.Content == "" {
			return fmt.Errorf("view missing title/content: %+v", dw.lastView)
		}
		return nil
	})
	sc.Step(`^a "([^"]*)" event with actor "([^"]*)", a timestamp, and subject (SUT-\d+) is recorded$`, func(kind, handle, issueName string) error {
		return iw.expectEvent(kind, issueName, handle)
	})
	sc.Step(`^a "([^"]*)" event with actor "([^"]*)" and a timestamp is recorded for (SUT-\d+)$`, func(kind, handle, issueName string) error {
		return iw.expectEvent(kind, issueName, handle)
	})

	// --- saves append immutable versions
	sc.Step(`^document "([^"]*)" has one version$`, func(title string) error {
		return dw.createDoc(title, "v1 content", "", "human-brent")
	})
	sc.Step(`^new content is saved by "([^"]*)"$`, func(handle string) error {
		return dw.saveVersion(dw.lastView.Title, "v2 content", handle)
	})
	sc.Step(`^version 2 exists with author "([^"]*)" and a timestamp$`, func(handle string) error {
		if err := dw.readDoc(dw.lastView.Title, 2); err != nil {
			return err
		}
		if dw.lastView.Version.Author != iw.identities[handle] || dw.lastView.Version.Created == "" {
			return fmt.Errorf("version 2 wrong author/timestamp: %+v", dw.lastView.Version)
		}
		return nil
	})
	sc.Step(`^version 1 is still readable and unchanged$`, func() error {
		if err := dw.readDoc(dw.lastView.Title, 1); err != nil {
			return err
		}
		if dw.lastView.Version.Content != "v1 content" {
			return fmt.Errorf("version 1 mutated: %q", dw.lastView.Version.Content)
		}
		return nil
	})

	// --- latest by default / history and diffs
	sc.Step(`^document "([^"]*)" has three versions$`, func(title string) error {
		if err := dw.createDoc(title, "v1 content", "", "human-brent"); err != nil {
			return err
		}
		for _, content := range []string{"v2 content", "v3 content"} {
			if err := dw.saveVersion(title, content, "claude"); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^the document is requested without a version$`, func() error {
		return dw.readDoc(dw.lastView.Title, 0)
	})
	sc.Step(`^version 3's content is returned$`, func() error {
		if dw.lastView.Version.Number != 3 || dw.lastView.Version.Content != "v3 content" {
			return fmt.Errorf("expected v3, got %+v", dw.lastView.Version)
		}
		return nil
	})
	sc.Step(`^its history is requested$`, func() error {
		if err := iw.s.call(http.MethodGet, "/documents/"+dw.documents[dw.lastView.Title]+"/versions", nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^all versions are listed in order with author and time$`, func() error {
		var versions []struct {
			Number  int64  `json:"number"`
			Author  string `json:"author"`
			Created string `json:"created"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &versions); err != nil {
			return err
		}
		if len(versions) != 3 {
			return fmt.Errorf("expected 3 versions, got %d", len(versions))
		}
		for i, v := range versions {
			if v.Number != int64(i+1) || v.Author == "" || v.Created == "" {
				return fmt.Errorf("version %d malformed: %+v", i+1, v)
			}
		}
		return nil
	})
	sc.Step(`^versions 1 and 3 are compared$`, func() error {
		if err := iw.s.call(http.MethodGet, "/documents/"+dw.documents[dw.lastView.Title]+"/diff?from=1&to=3", nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^a diff of their content is returned$`, func() error {
		var diff struct {
			From int64  `json:"from"`
			To   int64  `json:"to"`
			Diff string `json:"diff"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &diff); err != nil {
			return err
		}
		if diff.From != 1 || diff.To != 3 || !strings.Contains(diff.Diff, "-v1 content") || !strings.Contains(diff.Diff, "+v3 content") {
			return fmt.Errorf("diff malformed: %+v", diff)
		}
		return nil
	})

	// --- templates are managed by name
	sc.Step(`^no template named "([^"]*)" exists$`, func(name string) error {
		if err := iw.s.call(http.MethodGet, "/templates", nil); err != nil {
			return err
		}
		if strings.Contains(string(iw.s.lastBody), name) {
			return fmt.Errorf("template %q pre-exists", name)
		}
		return nil
	})
	sc.Step(`^template "([^"]*)" is created with content$`, func(name string) error {
		if err := iw.s.call(http.MethodPost, "/templates", map[string]string{"name": name, "content": "template body of " + name}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
			return err
		}
		dw.templates[name] = created.ID
		return nil
	})
	sc.Step(`^it appears in the template list$`, func() error {
		if err := iw.s.call(http.MethodGet, "/templates", nil); err != nil {
			return err
		}
		for name, id := range dw.templates {
			if !strings.Contains(string(iw.s.lastBody), id) {
				return fmt.Errorf("template %q missing from list", name)
			}
		}
		return nil
	})
	sc.Step(`^it is updated and then removed$`, func() error {
		var name, id string
		for n, i := range dw.templates {
			name, id = n, i
		}
		if err := iw.s.call(http.MethodPut, "/templates/"+id, map[string]string{"content": "updated body"}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodGet, "/templates/"+id, nil); err != nil {
			return err
		}
		var got struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.Content != "updated body" {
			return fmt.Errorf("update not reflected: %q", got.Content)
		}
		if err := iw.s.call(http.MethodDelete, "/templates/"+id, nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusNoContent); err != nil {
			return err
		}
		delete(dw.templates, name)
		return nil
	})
	sc.Step(`^the list reflects each change$`, func() error {
		if err := iw.s.call(http.MethodGet, "/templates", nil); err != nil {
			return err
		}
		if string(iw.s.lastBody) != "[]" {
			return fmt.Errorf("removed template still listed: %s", iw.s.lastBody)
		}
		return nil
	})

	// --- template seeds the first version
	sc.Step(`^template "([^"]*)" exists$`, func(name string) error {
		if err := iw.s.call(http.MethodPost, "/templates", map[string]string{"name": name, "content": "template body of " + name}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
			return err
		}
		dw.templates[name] = created.ID
		return nil
	})
	sc.Step(`^document "([^"]*)" is created in project "SUT" from "([^"]*)"$`, func(title, template string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		author, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
			map[string]string{"title": title, "template_id": dw.templates[template], "author": author}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		if err := json.Unmarshal(iw.s.lastBody, &dw.lastView); err != nil {
			return err
		}
		dw.documents[title] = dw.lastView.ID
		return nil
	})
	sc.Step(`^version 1 of "([^"]*)" has the template's content$`, func(title string) error {
		if err := dw.readDoc(title, 1); err != nil {
			return err
		}
		if !strings.HasPrefix(dw.lastView.Version.Content, "template body of ") {
			return fmt.Errorf("v1 content is not the template's: %q", dw.lastView.Version.Content)
		}
		return nil
	})
	sc.Step(`^saving new content appends version 2 as usual$`, func() error {
		title := dw.lastView.Title
		if err := dw.saveVersion(title, "post-template content", "claude"); err != nil {
			return err
		}
		if err := dw.readDoc(title, 0); err != nil {
			return err
		}
		if dw.lastView.Version.Number != 2 {
			return fmt.Errorf("expected version 2, got %d", dw.lastView.Version.Number)
		}
		return nil
	})

	// --- link and unlink after creation
	sc.Step(`^document "([^"]*)" exists in project "SUT" with no issue$`, func(title string) error {
		return dw.createDoc(title, "content of "+title, "", "human-brent")
	})
	sc.Step(`^"([^"]*)" is tied to issue (SUT-\d+) by "([^"]*)"$`, func(title, issueName, handle string) error {
		issueID, err := iw.ensureIssue(issueName)
		if err != nil {
			return err
		}
		actor, err := iw.identity(handle)
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/documents/"+dw.documents[title]+"/issue",
			map[string]string{"issue": issueID, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) lists "([^"]*)"$`, func(issueName, title string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/documents", nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), dw.documents[title]) {
			return fmt.Errorf("%s not listed under %s: %s", title, issueName, iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^"([^"]*)" is untied from (SUT-\d+) by "([^"]*)"$`, func(title, _, handle string) error {
		actor, err := iw.identity(handle)
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodDelete, "/documents/"+dw.documents[title]+"/issue?actor="+actor, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) lists no documents$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/documents", nil); err != nil {
			return err
		}
		if string(iw.s.lastBody) != "[]" {
			return fmt.Errorf("expected no documents, got %s", iw.s.lastBody)
		}
		return nil
	})

	// --- both sides see the link
	sc.Step(`^document "([^"]*)" is tied to issue (SUT-\d+)$`, func(title, issueName string) error {
		return dw.createDoc(title, "content of "+title, issueName, "human-brent")
	})
	sc.Step(`^(SUT-\d+)'s documents are requested$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/documents", nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^"([^"]*)" is listed$`, func(title string) error {
		if !strings.Contains(string(iw.s.lastBody), dw.documents[title]) {
			return fmt.Errorf("%s not listed: %s", title, iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^"([^"]*)" shows (SUT-\d+) as its issue$`, func(title, issueName string) error {
		if err := dw.readDoc(title, 0); err != nil {
			return err
		}
		var doc struct {
			Issue *string `json:"issue"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
			return err
		}
		if doc.Issue == nil || *doc.Issue != iw.issues[issueName] {
			return fmt.Errorf("%s does not show %s: %v", title, issueName, doc.Issue)
		}
		return nil
	})

	// --- doc deliverables gate like code (REQ-close-requires-review)
	sc.Step(`^issue (SUT-\d+) has a review whose deliverable is a document version$`, func(issueName string) error {
		if err := dw.createDoc("deliverable-doc", "the doc under review", "", "human-brent"); err != nil {
			return err
		}
		issueID, err := iw.ensureIssue(issueName)
		if err != nil {
			return err
		}
		author := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/reviews", map[string]string{
			"issue": issueID, "author": author, "doc_version": dw.lastView.Version.ID,
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
		cw.reviews[issueName] = reviewRef{id: created.ID, revision: created.Revision}
		return nil
	})
	sc.Step(`^that review is in state "approved"$`, func() error {
		for issueName := range cw.reviews {
			return cw.verdict(issueName, "approved")
		}
		return fmt.Errorf("no review recorded")
	})

	// --- a doc deliverable resolves from its version
	sc.Step(`^the deliverable of that review is read$`, func() error {
		ref, ok := cw.reviews["SUT-1"]
		if !ok {
			return fmt.Errorf("no review recorded for SUT-1")
		}
		if err := iw.s.call(http.MethodGet, "/reviews/"+ref.id+"/deliverable", nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^it is served as a doc, carrying the version's own text$`, func() error {
		var resolved struct {
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &resolved); err != nil {
			return err
		}
		if resolved.Kind != "doc" {
			return fmt.Errorf("deliverable kind %q, want doc", resolved.Kind)
		}
		if resolved.Content != "the doc under review" {
			return fmt.Errorf("deliverable content %q, want the document version's own text", resolved.Content)
		}
		return nil
	})
}
