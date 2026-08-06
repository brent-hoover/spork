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

// threadsWorld drives the transcript import/anchor scenarios
// (REQ-thread-catalog, REQ-thread-links).
type threadsWorld struct {
	iw         *issueWorld
	cw         *closeWorld
	transcript json.RawMessage // the "file" content, compared verbatim after import
	session    string
	threadID   string
	title      string
}

func (tw *threadsWorld) reset() {
	tw.transcript = nil
	tw.session = ""
	tw.threadID = ""
	tw.title = ""
}

// importThread posts the held transcript anchored to the world's
// project and records the minted thread id.
func (tw *threadsWorld) importThread(title string) error {
	iw := tw.iw
	if err := tw.cw.ensureGitProject(); err != nil {
		return err
	}
	actor, err := iw.identity("operator")
	if err != nil {
		return err
	}
	// The request body is assembled by splicing the transcript bytes
	// in raw — marshaling a map would compact the fixture's deliberate
	// whitespace before the server ever saw it.
	rest := map[string]any{"title": title, "project": iw.project, "actor": actor}
	if tw.session != "" {
		rest["session"] = tw.session
	}
	encoded, err := json.Marshal(rest)
	if err != nil {
		return err
	}
	body := append(encoded[:len(encoded)-1], []byte(`,"transcript":`)...)
	body = append(body, tw.transcript...)
	body = append(body, '}')
	if err := iw.s.call(http.MethodPost, "/threads", json.RawMessage(body)); err != nil {
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
	tw.threadID = created.ID
	tw.title = title
	return nil
}

// anchorEvent finds the latest thread.anchor-changed event for a
// subject and returns its decoded payload anchors.
func (tw *threadsWorld) anchorEvent(subject string) (old, new map[string]*string, err error) {
	iw := tw.iw
	if err := iw.s.call(http.MethodGet, "/events?kind="+url.QueryEscape("thread.anchor-changed")+"&subject="+subject, nil); err != nil {
		return nil, nil, err
	}
	var page struct {
		Events []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
		return nil, nil, err
	}
	if len(page.Events) == 0 {
		return nil, nil, fmt.Errorf("no thread.anchor-changed event for subject %s", subject)
	}
	last := page.Events[len(page.Events)-1]
	if len(last.Payload) == 0 {
		return nil, nil, fmt.Errorf("anchor-changed event carries no payload")
	}
	var payload struct {
		Thread string             `json:"thread"`
		Old    map[string]*string `json:"old"`
		New    map[string]*string `json:"new"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		return nil, nil, fmt.Errorf("decode anchor payload: %w", err)
	}
	if payload.Thread != tw.threadID {
		return nil, nil, fmt.Errorf("payload names thread %s, want %s", payload.Thread, tw.threadID)
	}
	return payload.Old, payload.New, nil
}

func registerThreadsSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	tw := &threadsWorld{iw: iw, cw: cw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		tw.reset()
		return ctx, nil
	})

	// --- import preserves the transcript
	sc.Step(`^a session transcript file from agent "([^"]*)" with session "([^"]*)"$`, func(agent, session string) error {
		// Deliberately formatted: insignificant whitespace and
		// newlines must survive import and serving byte-for-byte.
		tw.transcript = json.RawMessage("[\n" +
			"  { \"speaker\": \"human-brent\",  \"text\": \"why does pop hand out the parent first?\" },\n" +
			"  { \"speaker\": \"" + agent + "\",  \"text\": \"the deepest unblocked descendant wins; the parent waits.\" }\n" +
			"]")
		tw.session = session
		return nil
	})
	sc.Step(`^it is imported as "([^"]*)"$`, func(title string) error {
		return tw.importThread(title)
	})
	sc.Step(`^a thread exists with that title, session "([^"]*)", and an import time$`, func(session string) error {
		if err := iw.s.call(http.MethodGet, "/threads/"+tw.threadID, nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		var got struct {
			Title      string  `json:"title"`
			Session    *string `json:"session"`
			ImportedAt string  `json:"imported_at"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.Title != tw.title {
			return fmt.Errorf("title %q, want %q", got.Title, tw.title)
		}
		if got.Session == nil || *got.Session != session {
			return fmt.Errorf("session %v, want %q", got.Session, session)
		}
		if got.ImportedAt == "" {
			return fmt.Errorf("imported_at missing")
		}
		return nil
	})
	sc.Step(`^its content matches the file verbatim$`, func() error {
		var got struct {
			Transcript json.RawMessage `json:"transcript"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if string(got.Transcript) != string(tw.transcript) {
			return fmt.Errorf("transcript altered:\n got: %s\nwant: %s", got.Transcript, tw.transcript)
		}
		return nil
	})

	// --- thread content is searchable
	sc.Step(`^a thread containing the phrase "([^"]*)"$`, func(phrase string) error {
		raw, err := json.Marshal([]map[string]string{
			{"speaker": "claude", "text": "I found a " + phrase + " while tracing the claim path."},
		})
		if err != nil {
			return err
		}
		tw.transcript = raw
		return tw.importThread("thread mentioning " + phrase)
	})
	sc.Step(`^threads are searched for "([^"]*)"$`, func(q string) error {
		if err := iw.s.call(http.MethodGet, "/threads/search?q="+url.QueryEscape(q), nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^that thread is returned with surrounding context$`, func() error {
		var found []struct {
			ID         string          `json:"id"`
			Transcript json.RawMessage `json:"transcript"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &found); err != nil {
			return err
		}
		for _, t := range found {
			if t.ID == tw.threadID {
				// The full transcript IS the surrounding context: the
				// match comes back inside its conversation, not alone.
				if string(t.Transcript) != string(tw.transcript) {
					return fmt.Errorf("search result lost its context: %s", t.Transcript)
				}
				return nil
			}
		}
		return fmt.Errorf("thread %s missing from search results: %s", tw.threadID, iw.s.lastBody)
	})

	// --- threads anchor to their work
	sc.Step(`^a thread imported anchored only to project "SUT"$`, func() error {
		raw, err := json.Marshal([]map[string]string{
			{"speaker": "claude", "text": "session notes for the anchor scenario"},
		})
		if err != nil {
			return err
		}
		tw.transcript = raw
		return tw.importThread("anchor-scenario thread")
	})
	sc.Step(`^it is tied to issue (SUT-\d+) by "([^"]*)"$`, func(issueName, handle string) error {
		issueID, err := iw.ensureIssue(issueName)
		if err != nil {
			return err
		}
		actor, err := iw.identity(handle)
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/threads/"+tw.threadID+"/anchor",
			map[string]string{"issue": issueID, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) lists the thread$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/threads", nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), tw.threadID) {
			return fmt.Errorf("thread missing from %s's listing: %s", issueName, iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^that event's payload carries the old and new anchors$`, func() error {
		// The retarget-to-issue event: old anchor was project-only,
		// new anchor is the issue.
		old, newAnchor, err := tw.anchorEvent(iw.issues["SUT-1"])
		if err != nil {
			return err
		}
		if old["project"] == nil || *old["project"] != iw.project || old["issue"] != nil {
			return fmt.Errorf("old anchor wrong: %v", old)
		}
		if newAnchor["issue"] == nil || *newAnchor["issue"] != iw.issues["SUT-1"] {
			return fmt.Errorf("new anchor wrong: %v", newAnchor)
		}
		return nil
	})
	sc.Step(`^it is retargeted to project "SUT" by "([^"]*)"$`, func(handle string) error {
		actor, err := iw.identity(handle)
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/threads/"+tw.threadID+"/anchor",
			map[string]string{"project": iw.project, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^"SUT" lists the thread and (SUT-\d+) no longer does$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/threads/search?project="+iw.project, nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), tw.threadID) {
			return fmt.Errorf("thread missing from project listing: %s", iw.s.lastBody)
		}
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/threads", nil); err != nil {
			return err
		}
		if strings.Contains(string(iw.s.lastBody), tw.threadID) {
			return fmt.Errorf("thread still listed under %s after retarget: %s", issueName, iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^a "thread\.anchor-changed" event with actor "([^"]*)", a timestamp, subject "SUT", and the old and new anchors in its payload is recorded$`, func(handle string) error {
		if err := iw.expectEventBySubject("thread.anchor-changed", iw.project, handle); err != nil {
			return err
		}
		old, newAnchor, err := tw.anchorEvent(iw.project)
		if err != nil {
			return err
		}
		if old["issue"] == nil || *old["issue"] != iw.issues["SUT-1"] {
			return fmt.Errorf("old anchor wrong on retarget event: %v", old)
		}
		if newAnchor["project"] == nil || *newAnchor["project"] != iw.project || newAnchor["issue"] != nil {
			return fmt.Errorf("new anchor wrong on retarget event: %v", newAnchor)
		}
		return nil
	})
}
