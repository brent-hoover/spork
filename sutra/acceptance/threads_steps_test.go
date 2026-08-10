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
	// The second project the both-ends guard needs: somewhere live for a
	// thread in an archived project to try to move to.
	otherProject string
	// The search scenario's fixtures: every thread that must come back,
	// keyed by id so each result is checked against its OWN transcript,
	// and the one that must not come back at all.
	matching  map[string]json.RawMessage
	unrelated string
}

func (tw *threadsWorld) reset() {
	tw.transcript = nil
	tw.session = ""
	tw.threadID = ""
	tw.title = ""
	tw.otherProject = ""
	tw.matching = map[string]json.RawMessage{}
	tw.unrelated = ""
}

// postTranscript posts one transcript verbatim and asserts nothing —
// the rejection scenarios need the response, not a thread.
func (tw *threadsWorld) postTranscript(title string, transcript []byte) error {
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
	body = append(body, transcript...)
	body = append(body, '}')
	return iw.s.call(http.MethodPost, "/threads", json.RawMessage(body))
}

// importThread posts the held transcript anchored to the world's
// project and records the minted thread id.
func (tw *threadsWorld) importThread(title string) error {
	iw := tw.iw
	if err := tw.postTranscript(title, tw.transcript); err != nil {
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

	// --- ambiguity is rejected at the door (AC-no-ambiguous-bodies)
	// A transcript is the sharpest case for the guarantee: it is stored
	// verbatim and re-served, so a repeat accepted here would outlive
	// the request and re-export differently than it arrived.
	ambiguous := map[string]string{
		"at the transcript's top level": `{"%[1]s":"human","%[1]s":"agent"}`,
		"inside a nested object":        `{"turn":{"%[1]s":"human","%[1]s":"agent"}}`,
		"inside an object in an array":  `[{"ok":1},{"%[1]s":"human","%[1]s":"agent"}]`,
	}
	sc.Step(`^a thread transcript repeating "([^"]*)" (.+) is imported$`, func(property, where string) error {
		shape, ok := ambiguous[where]
		if !ok {
			return fmt.Errorf("no transcript shape for %q", where)
		}
		return tw.postTranscript("ambiguous transcript", []byte(fmt.Sprintf(shape, property)))
	})
	sc.Step(`^the import is rejected as malformed, naming "([^"]*)"$`, func(property string) error {
		if err := iw.s.expectStatus(http.StatusBadRequest); err != nil {
			return err
		}
		if err := iw.s.expectErrorCode("bad-request"); err != nil {
			return err
		}
		// Naming the property is what makes the rejection actionable —
		// an ambiguous body gives the client no other way to find it.
		if !strings.Contains(string(iw.s.lastBody), property) {
			return fmt.Errorf("rejection does not name %q — body %s", property, iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^no thread was created$`, func() error {
		if err := iw.s.call(http.MethodGet, "/threads/search?project="+iw.project, nil); err != nil {
			return err
		}
		var threads []json.RawMessage
		if err := json.Unmarshal(iw.s.lastBody, &threads); err != nil {
			return fmt.Errorf("decode thread list: %w — body %s", err, iw.s.lastBody)
		}
		if len(threads) != 0 {
			return fmt.Errorf("expected no threads, got %d — body %s", len(threads), iw.s.lastBody)
		}
		return nil
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
	importMentioning := func(phrase string) error {
		raw, err := json.Marshal([]map[string]string{
			{"speaker": "claude", "text": "I found a " + phrase + " while tracing the claim path."},
		})
		if err != nil {
			return err
		}
		tw.transcript = raw
		if err := tw.importThread("thread mentioning " + phrase); err != nil {
			return err
		}
		tw.matching[tw.threadID] = raw
		return nil
	}
	sc.Step(`^a thread containing the phrase "([^"]*)"$`, importMentioning)
	sc.Step(`^another thread containing the phrase "([^"]*)"$`, importMentioning)
	sc.Step(`^a thread that mentions neither$`, func() error {
		raw, err := json.Marshal([]map[string]string{
			{"speaker": "claude", "text": "unrelated notes about the export format"},
		})
		if err != nil {
			return err
		}
		tw.transcript = raw
		if err := tw.importThread("unrelated thread"); err != nil {
			return err
		}
		tw.unrelated = tw.threadID
		return nil
	})
	sc.Step(`^threads are searched for "([^"]*)"$`, func(q string) error {
		if err := iw.s.call(http.MethodGet, "/threads/search?q="+url.QueryEscape(q), nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^each matching thread is returned with surrounding context$`, func() error {
		var found []struct {
			ID         string          `json:"id"`
			Transcript json.RawMessage `json:"transcript"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &found); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, t := range found {
			if t.ID == tw.unrelated {
				return fmt.Errorf("thread without the phrase came back: %s", iw.s.lastBody)
			}
			want, ok := tw.matching[t.ID]
			if !ok {
				return fmt.Errorf("unexpected thread %s in results: %s", t.ID, iw.s.lastBody)
			}
			// The full transcript IS the surrounding context: the
			// match comes back inside its conversation, not alone.
			if string(t.Transcript) != string(want) {
				return fmt.Errorf("search result lost its context: %s", t.Transcript)
			}
			seen[t.ID] = true
		}
		for id := range tw.matching {
			if !seen[id] {
				return fmt.Errorf("thread %s missing from search results: %s", id, iw.s.lastBody)
			}
		}
		return nil
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

	// --- an anchor is guarded at both ends
	sc.Step(`^it is anchored to project "SUT" and an issue in another project$`, func() error {
		other, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		tw.otherProject = other
		foreign, err := iw.createIssueIn(other, "someone else's work", "not SUT's")
		if err != nil {
			return err
		}
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		return iw.s.call(http.MethodPost, "/threads/"+tw.threadID+"/anchor",
			map[string]string{"project": iw.project, "issue": foreign, "actor": actor})
	})
	sc.Step(`^the anchor is refused as a bad request$`, func() error {
		return iw.s.expectStatus(http.StatusBadRequest)
	})
	sc.Step(`^"SUT" is archived and the thread is retargeted to the other project$`, func() error {
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/archive",
			map[string]string{"actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return iw.s.call(http.MethodPost, "/threads/"+tw.threadID+"/anchor",
			map[string]string{"project": tw.otherProject, "actor": actor})
	})
	sc.Step(`^the other project is archived and the thread is retargeted into it$`, func() error {
		other, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		tw.otherProject = other
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		// Only the DESTINATION freezes. The thread's own project stays
		// live, so the request is routed under a writable project and a
		// guard that asked only about that one would let it through.
		if err := iw.s.call(http.MethodPost, "/projects/"+other+"/archive",
			map[string]string{"actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return iw.s.call(http.MethodPost, "/threads/"+tw.threadID+"/anchor",
			map[string]string{"project": other, "actor": actor})
	})
	sc.Step(`^the anchor is refused as a conflict and the thread still belongs to "SUT"$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodGet, "/threads/"+tw.threadID, nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		var thread struct {
			Project *string `json:"project"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &thread); err != nil {
			return err
		}
		if thread.Project == nil || *thread.Project != iw.project {
			return fmt.Errorf("the thread left the archived project: %s", iw.s.lastBody)
		}
		return nil
	})
}
