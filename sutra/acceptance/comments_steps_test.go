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

// commentView mirrors the contract's Comment for assertions.
type commentView struct {
	ID      string  `json:"id"`
	Issue   *string `json:"issue"`
	Parent  *string `json:"parent"`
	Author  string  `json:"author"`
	Body    string  `json:"body"`
	Created string  `json:"created"`
}

// commentsWorld drives the comment and label scenarios.
type commentsWorld struct {
	iw          *issueWorld
	labels      map[string]string // name -> id
	lastComment commentView
	deepest     string // deepest comment id in the nesting scenario
}

func (cw *commentsWorld) reset() {
	cw.labels = map[string]string{}
	cw.lastComment = commentView{}
	cw.deepest = ""
}

func (cw *commentsWorld) comment(issueName, handle, body string, parent *string) error {
	iw := cw.iw
	author, err := iw.identity(handle)
	if err != nil {
		return err
	}
	payload := map[string]any{"issue": iw.issues[issueName], "author": author, "body": body}
	if parent != nil {
		payload["parent"] = *parent
	}
	if err := iw.s.call(http.MethodPost, "/comments", payload); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	return json.Unmarshal(iw.s.lastBody, &cw.lastComment)
}

func (cw *commentsWorld) issueComments(issueName string) ([]commentView, error) {
	iw := cw.iw
	if err := iw.s.call(http.MethodGet, "/comments?issue="+url.QueryEscape(iw.issues[issueName]), nil); err != nil {
		return nil, err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return nil, err
	}
	var list []commentView
	if err := json.Unmarshal(iw.s.lastBody, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func registerCommentsSteps(sc *godog.ScenarioContext, iw *issueWorld) {
	cw := &commentsWorld{iw: iw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		cw.reset()
		return ctx, nil
	})

	// --- comment lands on the issue
	sc.Step(`^"([^"]*)" comments "([^"]*)" on (SUT-\d+)$`, func(handle, body, issueName string) error {
		return cw.comment(issueName, handle, body, nil)
	})
	sc.Step(`^the comment appears in (SUT-\d+)'s comments with author and timestamp$`, func(issueName string) error {
		list, err := cw.issueComments(issueName)
		if err != nil {
			return err
		}
		for _, c := range list {
			if c.ID == cw.lastComment.ID {
				if c.Author == "" || c.Created == "" {
					return fmt.Errorf("comment missing author/timestamp: %+v", c)
				}
				return nil
			}
		}
		return fmt.Errorf("comment %s missing from %s's comments", cw.lastComment.ID, issueName)
	})

	// --- replies nest without depth limit
	sc.Step(`^a comment thread five levels deep on (SUT-\d+)$`, func(issueName string) error {
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		var parent *string
		for level := 1; level <= 5; level++ {
			if err := cw.comment(issueName, "human-brent", fmt.Sprintf("level %d", level), parent); err != nil {
				return err
			}
			id := cw.lastComment.ID
			parent = &id
		}
		cw.deepest = cw.lastComment.ID
		return nil
	})
	sc.Step(`^a reply is added at the deepest level$`, func() error {
		parent := cw.deepest
		for issueName := range iw.issues {
			return cw.comment(issueName, "claude", "level 6", &parent)
		}
		return fmt.Errorf("no issue in scope")
	})
	sc.Step(`^it nests under its parent as level six$`, func() error {
		if cw.lastComment.Parent == nil || *cw.lastComment.Parent != cw.deepest {
			return fmt.Errorf("reply's parent is %v, want %s", cw.lastComment.Parent, cw.deepest)
		}
		// Walk the parent chain: exactly six comments deep.
		var list []commentView
		for issueName := range iw.issues {
			var err error
			if list, err = cw.issueComments(issueName); err != nil {
				return err
			}
			break
		}
		byID := map[string]commentView{}
		for _, c := range list {
			byID[c.ID] = c
		}
		depth := 0
		for cur := &cw.lastComment.ID; cur != nil; {
			c, ok := byID[*cur]
			if !ok {
				return fmt.Errorf("parent chain broken at %s", *cur)
			}
			depth++
			cur = c.Parent
		}
		if depth != 6 {
			return fmt.Errorf("depth %d, want 6", depth)
		}
		return nil
	})

	// --- no artificial caps
	sc.Step(`^issue (SUT-\d+) has one thousand comments$`, func(issueName string) error {
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		author, err := iw.identity("claude")
		if err != nil {
			return err
		}
		issueID := iw.issues[issueName]
		for n := 1; n <= 1000; n++ {
			status, body, err := iw.s.callKeyed(http.MethodPost, "/comments",
				map[string]any{"issue": issueID, "author": author, "body": fmt.Sprintf("comment %d", n)},
				fmt.Sprintf("bulk-comment-%d", n))
			if err != nil {
				return err
			}
			if status != http.StatusCreated {
				return fmt.Errorf("comment %d failed: %d %s", n, status, body)
			}
		}
		return nil
	})
	sc.Step(`^another comment is added$`, func() error {
		for issueName := range iw.issues {
			return cw.comment(issueName, "claude", "comment 1001", nil)
		}
		return fmt.Errorf("no issue in scope")
	})
	sc.Step(`^it succeeds like the first$`, func() error {
		list, err := cw.issueComments("SUT-1")
		if err != nil {
			return err
		}
		if len(list) != 1001 {
			return fmt.Errorf("expected 1001 comments, got %d", len(list))
		}
		return nil
	})

	// --- labels attach and detach
	sc.Step(`^issue (SUT-\d+) exists and label "([^"]*)" exists$`, func(issueName, name string) error {
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": name}); err != nil {
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
		cw.labels[name] = created.ID
		return nil
	})
	// --- the label catalog (AC-label-catalog)
	sc.Step(`^labels "([^"]*)", "([^"]*)", and "([^"]*)" exist$`, func(a, b, c string) error {
		// Created OUT of alphabetical order, so the ordering assertion
		// tests the catalog's sort rather than creation order.
		for _, name := range []string{a, b, c} {
			if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": name}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusCreated); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^the label catalog is listed$`, func() error {
		if err := iw.s.call(http.MethodGet, "/labels", nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^it holds ([a-z]+), ([a-z]+), and ([a-z]+) in name order$`, func(a, b, c string) error {
		var got []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		names := make([]string, 0, len(got))
		for _, l := range got {
			names = append(names, l.Name)
		}
		want := strings.Join([]string{a, b, c}, ",")
		if strings.Join(names, ",") != want {
			return fmt.Errorf("catalog = %q, want %q", strings.Join(names, ","), want)
		}
		return nil
	})

	sc.Step(`^"([^"]*)" is attached to (SUT-\d+) and then detached$`, func(name, issueName string) error {
		issueID := iw.issues[issueName]
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/labels",
			map[string]string{"label": cw.labels[name], "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		// Attached state is visible before the detach.
		if !strings.Contains(string(iw.s.lastBody), cw.labels[name]) {
			return fmt.Errorf("attached label missing from issue read: %s", iw.s.lastBody)
		}
		if err := iw.s.call(http.MethodDelete, "/issues/"+issueID+"/labels/"+cw.labels[name]+"?actor="+actor, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+)'s label list reflects each change$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName], nil); err != nil {
			return err
		}
		var got struct {
			Labels []struct {
				ID string `json:"id"`
			} `json:"labels"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if len(got.Labels) != 0 {
			return fmt.Errorf("labels remain after detach: %+v", got.Labels)
		}
		return nil
	})
	sc.Step(`^an event of kind "([^"]*)" with actor and timestamp is recorded for (SUT-\d+)$`, func(kind, issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/events", nil); err != nil {
			return err
		}
		var history []struct {
			Kind    string `json:"kind"`
			Actor   string `json:"actor"`
			Created string `json:"created"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &history); err != nil {
			return err
		}
		for _, e := range history {
			if e.Kind == kind && e.Actor != "" && e.Created != "" {
				return nil
			}
		}
		return fmt.Errorf("no %s event in %s's audit history", kind, issueName)
	})
}
