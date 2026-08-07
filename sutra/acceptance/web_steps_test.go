package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/cucumber/godog"

	"sutra/internal/web"
)

// webWorld drives the handler-level web scenarios: pages are plain
// HTML asserted on markup — the project decision is handler/HTML-level
// coverage, no browser driver.
type webWorld struct {
	iw *issueWorld
	cw *closeWorld

	ui       *httptest.Server
	lastHTML string
	lastCode int

	docID        string
	docVersionID string
	seenVersion  int64
	threadID     string
}

func (ww *webWorld) reset() {
	if ww.ui != nil {
		ww.ui.Close()
	}
	*ww = webWorld{iw: ww.iw, cw: ww.cw}
}

// open boots the UI against the scenario's API server on first use
// and fetches a page.
func (ww *webWorld) open(path string) error {
	if ww.ui == nil {
		actor, err := ww.iw.identity("human-brent")
		if err != nil {
			return err
		}
		ww.ui = httptest.NewServer(web.New(ww.iw.s.server.URL, actor).Handler())
	}
	resp, err := ww.ui.Client().Get(ww.ui.URL + path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	ww.lastCode = resp.StatusCode
	ww.lastHTML = string(body)
	return nil
}

func (ww *webWorld) postForm(path string, form url.Values) error {
	if ww.ui == nil {
		if err := ww.open("/"); err != nil {
			return err
		}
	}
	resp, err := ww.ui.Client().PostForm(ww.ui.URL+path, form)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	ww.lastCode = resp.StatusCode
	ww.lastHTML = string(body)
	return nil
}

func (ww *webWorld) expectInHTML(wants ...string) error {
	for _, want := range wants {
		if !strings.Contains(ww.lastHTML, want) {
			return fmt.Errorf("page missing %q:\n%.2000s", want, ww.lastHTML)
		}
	}
	return nil
}

func registerWebSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	ww := &webWorld{iw: iw, cw: cw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		ww.reset()
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if ww.ui != nil {
			ww.ui.Close()
			ww.ui = nil
		}
		return ctx, nil
	})

	// --- columns mirror statuses
	sc.Step(`^project "SUT" has issues in several statuses$`, func() error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		for i, status := range []string{"open", "in-progress", "blocked", "deferred"} {
			name := fmt.Sprintf("SUT-%d", i+1)
			if _, err := iw.ensureIssue(name); err != nil {
				return err
			}
			if status == "open" {
				continue
			}
			if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[name]+"/status",
				map[string]any{"status": status, "actor": actor}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^the board for "SUT" is opened$`, func() error {
		return ww.open("/p/SUT")
	})
	sc.Step(`^there is a column for each of open, in-progress, blocked, deferred, complete$`, func() error {
		for _, status := range []string{"open", "in-progress", "blocked", "deferred", "complete"} {
			if err := ww.expectInHTML(`data-status="` + status + `"`); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^each issue's card sits in its status's column$`, func() error {
		// Every card renders inside the section carrying its status:
		// split the page by column and check membership.
		sections := strings.Split(ww.lastHTML, `<section class="column"`)
		place := map[string]string{}
		for _, sec := range sections[1:] {
			status := strings.SplitN(strings.SplitN(sec, `data-status="`, 2)[1], `"`, 2)[0]
			place[status] = sec
		}
		for i, status := range []string{"open", "in-progress", "blocked", "deferred"} {
			id := iw.issues[fmt.Sprintf("SUT-%d", i+1)]
			if !strings.Contains(place[status], id) {
				return fmt.Errorf("issue %d not in %q column", i+1, status)
			}
		}
		return nil
	})

	// --- drag is a real transition
	sc.Step(`^issue (SUT-\d+) has no approved review$`, func(issueName string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		_, err := iw.ensureIssue(issueName)
		return err
	})
	sc.Step(`^its card is dragged to the "complete" column$`, func() error {
		return ww.postForm("/p/SUT/i/1/move", url.Values{"status": {"complete"}})
	})
	sc.Step(`^the card snaps back$`, func() error {
		// The re-rendered board still shows the card in its original
		// column, and the issue is unchanged server-side.
		sections := strings.Split(ww.lastHTML, `<section class="column"`)
		for _, sec := range sections[1:] {
			status := strings.SplitN(strings.SplitN(sec, `data-status="`, 2)[1], `"`, 2)[0]
			if strings.Contains(sec, iw.issues["SUT-1"]) && status != "open" {
				return fmt.Errorf("card landed in %q", status)
			}
		}
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), `"open"`) {
			return fmt.Errorf("issue mutated despite the snap-back")
		}
		return nil
	})
	sc.Step(`^the close-requires-review error is shown$`, func() error {
		return ww.expectInHTML(`class="error"`, "missing-approval")
	})

	// --- cards carry the essentials
	sc.Step(`^issue (SUT-\d+) has an assignee and labels$`, func(issueName string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		assignee, err := iw.identity("claude")
		if err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/assign",
			map[string]any{"assignee": assignee, "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": "ui-bug"}); err != nil {
			return err
		}
		var label struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &label); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/labels",
			map[string]string{"label": label.ID, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the board is viewed$`, func() error {
		return ww.open("/p/SUT")
	})
	sc.Step(`^(SUT-\d+)'s card shows number, title, assignee, and labels$`, func(issueName string) error {
		return ww.expectInHTML("SUT-1", "issue "+issueName, `class="assignee"`, `class="label">ui-bug`)
	})
	sc.Step(`^clicking it opens the issue$`, func() error {
		if err := ww.expectInHTML(`href="/p/SUT/i/1"`); err != nil {
			return err
		}
		if err := ww.open("/p/SUT/i/1"); err != nil {
			return err
		}
		return ww.expectInHTML("<h1>SUT-1")
	})

	// --- doc renders in the browser
	sc.Step(`^document "([^"]*)" has markdown content$`, func(title string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		author, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents", map[string]string{
			"title": title, "author": author,
			"content": "# Design\n\nA paragraph of prose.\n\n- first point\n- second point\n"}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var doc struct {
			ID      string `json:"id"`
			Version struct {
				ID     string `json:"id"`
				Number int64  `json:"number"`
			} `json:"version"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
			return err
		}
		ww.docID = doc.ID
		ww.docVersionID = doc.Version.ID
		ww.seenVersion = doc.Version.Number
		return nil
	})
	sc.Step(`^it is opened in the web UI$`, func() error {
		if ww.docID != "" {
			return ww.open("/p/SUT/d/" + ww.docID)
		}
		return ww.open("/p/SUT/t/" + ww.threadID)
	})
	sc.Step(`^the content renders formatted$`, func() error {
		return ww.expectInHTML("<h1>Design</h1>", "<p>A paragraph of prose.</p>", "<li>first point</li>")
	})
	sc.Step(`^the shown version is the latest unless one is chosen$`, func() error {
		if err := ww.expectInHTML(`data-version="1"`); err != nil {
			return err
		}
		author := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/documents/"+ww.docID+"/versions",
			map[string]string{"content": "# Design v2", "author": author}); err != nil {
			return err
		}
		if err := ww.open("/p/SUT/d/" + ww.docID); err != nil {
			return err
		}
		if err := ww.expectInHTML(`data-version="2"`); err != nil {
			return err
		}
		if err := ww.open("/p/SUT/d/" + ww.docID + "?version=1"); err != nil {
			return err
		}
		return ww.expectInHTML(`data-version="1"`)
	})

	// --- comments pin to a spot in a version
	sc.Step(`^document "([^"]*)" is at version 2$`, func(title string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		author, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
			map[string]string{"title": title, "content": "v1 text", "author": author}); err != nil {
			return err
		}
		var doc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
			return err
		}
		ww.docID = doc.ID
		if err := iw.s.call(http.MethodPost, "/documents/"+doc.ID+"/versions",
			map[string]string{"content": "one\n\ntwo\n\nthree", "author": author}); err != nil {
			return err
		}
		var v2 struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &v2); err != nil {
			return err
		}
		ww.docVersionID = v2.ID
		return nil
	})
	sc.Step(`^a reader comments on its third block$`, func() error {
		return ww.postForm("/p/SUT/d/"+ww.docID+"/comment", url.Values{
			"doc_version": {ww.docVersionID}, "anchor": {"block-3"}, "body": {"tighten this"}})
	})
	sc.Step(`^the comment anchors to version 2 at that block$`, func() error {
		if err := iw.s.call(http.MethodGet, "/comments?doc_version="+ww.docVersionID, nil); err != nil {
			return err
		}
		var list []struct {
			Anchor *string `json:"anchor"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &list); err != nil {
			return err
		}
		if len(list) != 1 || list[0].Anchor == nil || *list[0].Anchor != "block-3" {
			return fmt.Errorf("comment not pinned: %s", iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^viewing version 3 does not silently orphan the comment$`, func() error {
		author := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/documents/"+ww.docID+"/versions",
			map[string]string{"content": "v3 text", "author": author}); err != nil {
			return err
		}
		if err := ww.open("/p/SUT/d/" + ww.docID); err != nil {
			return err
		}
		// The v2-pinned comment stays visible, labeled with its version.
		return ww.expectInHTML(`data-anchor="block-3"`, "on v2", "tighten this")
	})

	// --- discussion sits beside the doc
	sc.Step(`^comments and replies exist on document "([^"]*)"$`, func(title string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		author, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
			map[string]string{"title": title, "content": "discussed text", "author": author}); err != nil {
			return err
		}
		var doc struct {
			ID      string `json:"id"`
			Version struct {
				ID string `json:"id"`
			} `json:"version"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
			return err
		}
		ww.docID = doc.ID
		ww.docVersionID = doc.Version.ID
		if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
			"doc_version": doc.Version.ID, "author": author, "body": "first thought"}); err != nil {
			return err
		}
		var first struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &first); err != nil {
			return err
		}
		second, err := iw.identity("claude")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
			"doc_version": doc.Version.ID, "author": second, "body": "a reply", "parent": first.ID}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^the doc is open in the web UI$`, func() error {
		return ww.open("/p/SUT/d/" + ww.docID)
	})
	sc.Step(`^the threaded discussion is visible alongside the content$`, func() error {
		return ww.expectInHTML(`class="discussion"`, "first thought", `class="comment reply"`, "a reply", "discussed text")
	})

	// --- viewer learns of new versions
	sc.Step(`^"([^"]*)" is open in a browser$`, func(title string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		author, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
			map[string]string{"title": title, "content": "watched text", "author": author}); err != nil {
			return err
		}
		var doc struct {
			ID      string `json:"id"`
			Version struct {
				Number int64 `json:"number"`
			} `json:"version"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
			return err
		}
		ww.docID = doc.ID
		ww.seenVersion = doc.Version.Number
		return ww.open("/p/SUT/d/" + ww.docID)
	})
	sc.Step(`^a new version is saved$`, func() error {
		author := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/documents/"+ww.docID+"/versions",
			map[string]string{"content": "updated text", "author": author}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^the viewer is notified or refreshed to the new version$`, func() error {
		if err := ww.open(fmt.Sprintf("/p/SUT/d/%s/poll?since=%d", ww.docID, ww.seenVersion)); err != nil {
			return err
		}
		var poll struct {
			Latest  int64 `json:"latest"`
			Refresh bool  `json:"refresh"`
		}
		if err := json.Unmarshal([]byte(ww.lastHTML), &poll); err != nil {
			return err
		}
		if !poll.Refresh || poll.Latest != ww.seenVersion+1 {
			return fmt.Errorf("viewer not signaled: %+v", poll)
		}
		// And re-opening shows the new version.
		if err := ww.open("/p/SUT/d/" + ww.docID); err != nil {
			return err
		}
		return ww.expectInHTML("updated text")
	})

	// --- threads read like conversations
	sc.Step(`^an imported thread$`, func() error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		transcript, err := json.Marshal([]map[string]string{
			{"speaker": "human-brent", "text": "what broke?"},
			{"speaker": "claude", "text": "the tokenizer table."},
		})
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
			"title": "debug session", "transcript": json.RawMessage(transcript),
			"project": iw.project, "actor": actor}); err != nil {
			return err
		}
		var thread struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &thread); err != nil {
			return err
		}
		ww.threadID = thread.ID
		return nil
	})
	sc.Step(`^it renders turn by turn with speakers distinguished$`, func() error {
		return ww.expectInHTML(`class="conversation"`,
			`<span class="speaker">human-brent</span>`, "what broke?",
			`<span class="speaker">claude</span>`, "the tokenizer table.")
	})

	// --- parent rolls up child progress
	sc.Step(`^(SUT-\d+) has five children of which three are complete$`, func(parent string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue(parent); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		for i := 2; i <= 6; i++ {
			child := fmt.Sprintf("SUT-%d", i)
			if _, err := iw.ensureIssue(child); err != nil {
				return err
			}
			if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[parent]+"/relations",
				map[string]string{"to": iw.issues[child], "kind": "parent_of", "actor": actor}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusCreated); err != nil {
				return err
			}
		}
		for i := 2; i <= 4; i++ {
			child := fmt.Sprintf("SUT-%d", i)
			if err := cw.approvedReview(child, fmt.Sprintf("child-%d", i)); err != nil {
				return err
			}
			if err := cw.close(child, 0, ""); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) is viewed$`, func(string) error {
		return ww.open("/p/SUT/i/1")
	})
	sc.Step(`^it shows progress "3 of 5 complete"$`, func() error {
		return ww.expectInHTML(`class="progress"`, "3 of 5 complete")
	})
}
