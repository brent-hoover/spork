//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/agent"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
)

// catalog stands in for sutra's thread catalog.
//
// It behaves as sutra does on the one point these scenarios turn on: a
// replayed Idempotency-Key returns the ORIGINAL thread rather than creating a
// second one. A fake that ignored the key would make the crash-recovery
// scenarios pass for the wrong reason.
type catalog struct {
	byKey   map[string]thread
	threads []thread
	err     error
	next    int
}

type thread struct {
	id         string
	title      string
	session    string
	issue      string
	transcript string
}

func newCatalog() *catalog { return &catalog{byKey: map[string]thread{}} }

func (c *catalog) Import(
	_ context.Context, title string, transcript json.RawMessage,
	session, issue, actor, key string,
) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if existing, ok := c.byKey[key]; ok {
		return existing.id, nil
	}
	c.next++
	t := thread{
		id: fmt.Sprintf("thread-%d", c.next), title: title, session: session,
		issue: issue, transcript: string(transcript),
	}
	c.byKey[key] = t
	c.threads = append(c.threads, t)
	return t.id, nil
}

// forSession returns every thread carrying a session id.
func (c *catalog) forSession(session string) []thread {
	var out []thread
	for _, t := range c.threads {
		if t.session == session {
			out = append(out, t)
		}
	}
	return out
}

// sessionStore is the dev-loop store for these scenarios.
type sessionStore struct{ rows map[string]devloop.Session }

func (s *sessionStore) Upsert(_ context.Context, sess devloop.Session) error {
	s.rows[sess.Run] = sess
	return nil
}

func (s *sessionStore) Find(_ context.Context, run string) (devloop.Session, bool, error) {
	sess, ok := s.rows[run]
	return sess, ok, nil
}

func (s *sessionStore) Importing(context.Context) ([]devloop.Session, error) {
	var out []devloop.Session
	for _, sess := range s.rows {
		if sess.ImportState == devloop.ImportImporting {
			out = append(out, sess)
		}
	}
	return out, nil
}

// threadWorld is one thread-capture scenario's state.
type threadWorld struct {
	catalog  *catalog
	store    *sessionStore
	loop     devloop.Loop
	work     string
	sessions map[string]devloop.Session
	err      error
}

func (w *world) newThreads(sessions ...string) (*threadWorld, error) {
	dir, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	cat := newCatalog()
	store := &sessionStore{rows: map[string]devloop.Session{}}
	tw := &threadWorld{
		catalog: cat, store: store, work: dir,
		sessions: map[string]devloop.Session{},
		loop: devloop.Loop{
			// One reply per session, in order: two runs are two conversations,
			// and a fake handing both the same session id would key their
			// imports identically and hide a lost transcript behind sutra's
			// correct refusal to duplicate.
			Agent: &fakes.Agent{Replies: replies(sessions...), Repeat: true},
			Store: store, Threads: cat, Now: fakes.NewClock(time.Unix(0, 0)),
		},
	}
	w.threads = tw
	return tw, nil
}

func (t *threadWorld) run(run, ticket string) (devloop.Session, error) {
	return t.loop.Work(context.Background(), devloop.Request{
		Run: run, Ticket: ticket, Title: ticket,
		Workspace: t.work, Issue: ticket, Actor: "actor-1",
	})
}

// replies builds one agent result per session id.
func replies(sessions ...string) []agent.Result {
	out := make([]agent.Result, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, agent.Result{
			SessionID: session, Model: "test-model", Text: "done",
		})
	}
	return out
}

func registerThreadCapture(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a run with session id "([^"]*)" working ticket "([^"]*)"$`,
		func(session, ticket string) error {
			tw, err := w.newThreads(session)
			if err != nil {
				return err
			}
			tw.sessions["ticket"] = devloop.Session{Ticket: ticket, SessionID: session}
			return nil
		})

	sc.Step(`^the run ends$`, func() error {
		tw := w.threads
		got, err := tw.run("run-1", tw.sessions["ticket"].Ticket)
		if err != nil {
			return err
		}
		tw.sessions["ended"] = got
		return nil
	})

	sc.Step(`^its dev-agent transcript is imported into sutra's thread catalog$`, func() error {
		tw := w.threads
		if len(tw.catalog.threads) != 1 {
			return fmt.Errorf("%d threads in the catalog", len(tw.catalog.threads))
		}
		if !strings.Contains(tw.catalog.threads[0].transcript, "Implement this ticket") {
			return fmt.Errorf("the thread holds %q", tw.catalog.threads[0].transcript)
		}
		return nil
	})

	sc.Step(`^the thread is stamped with "([^"]*)" and tied to "([^"]*)"$`,
		func(session, ticket string) error {
			got := w.threads.catalog.threads[0]
			if got.session != session {
				return fmt.Errorf("the thread is stamped %q", got.session)
			}
			if got.issue != ticket {
				return fmt.Errorf("the thread is tied to %q", got.issue)
			}
			return nil
		})

	sc.Step(`^runs that ended completed, retired, and crashed$`, func() error {
		tw, err := w.newThreads("sess-completed", "sess-retired")
		if err != nil {
			return err
		}
		// Completed and retired both END, so both capture on the way out.
		for i, ticket := range []string{"KRI-completed", "KRI-retired"} {
			got, err := tw.run(fmt.Sprintf("run-%d", i+1), ticket)
			if err != nil {
				return err
			}
			tw.sessions[ticket] = got
		}
		// The crashed one never reached capture: its row exists, its import
		// does not.
		tw.store.rows["run-crashed"] = devloop.Session{
			Run: "run-crashed", Ticket: "KRI-crashed", SessionID: "sess-crashed",
			ImportState: devloop.ImportNone,
		}
		return nil
	})

	sc.Step(`^each run's transcript is imported$`, func() error {
		tw := w.threads
		for _, ticket := range []string{"KRI-completed", "KRI-retired"} {
			if _, ok := tw.sessions[ticket]; !ok {
				return fmt.Errorf("%s never ran", ticket)
			}
		}
		if len(tw.catalog.threads) != 2 {
			return fmt.Errorf("%d threads for two ended runs", len(tw.catalog.threads))
		}
		return nil
	})

	sc.Step(`^recovery imports whatever transcript exists for the crashed run$`, func() error {
		tw := w.threads
		crashed := tw.store.rows["run-crashed"]
		// The crash left it before the write-ahead row, so there is nothing to
		// replay: recovery must not invent an import, and the session stays
		// visible as one that never captured.
		before := len(tw.catalog.threads)
		if _, err := tw.loop.RecoverImports(context.Background(), "", "actor-1"); err != nil {
			return err
		}
		if len(tw.catalog.threads) != before {
			return errors.New("recovery imported a transcript that was never persisted")
		}
		if crashed.ImportState != devloop.ImportNone {
			return fmt.Errorf("the crashed session is in state %q", crashed.ImportState)
		}
		return nil
	})

	sc.Step(`^the import key and transcript reference were persisted with state "([^"]*)" and the crash hit before sutra accepted the import$`,
		func(state string) error {
			tw, err := w.newThreads("sess-42")
			if err != nil {
				return err
			}
			tw.catalog.err = errors.New("sutra unreachable")
			if _, err := tw.run("run-1", "KRI-7"); err == nil {
				return errors.New("an import that never landed read as success")
			}
			row := tw.store.rows["run-1"]
			if row.ImportState != state {
				return fmt.Errorf("the row is in state %q", row.ImportState)
			}
			if row.ImportKey == "" || row.TranscriptRef == "" {
				return fmt.Errorf("the row cannot rebuild its own call: %+v", row)
			}
			tw.catalog.err = nil
			return nil
		})

	// Two features say this — one means a transcript import, the other a
	// review resubmission. Whichever world the scenario set up is the one
	// that means it.
	sc.Step(`^recovery replays the persisted request under the same key$`, func() error {
		if w.submit != nil {
			return w.submit.replayResubmission()
		}
		tw := w.threads
		_, tw.err = tw.loop.RecoverImports(context.Background(), "KRI-7", "actor-1")
		return tw.err
	})

	sc.Step(`^sutra creates the thread and exactly one thread exists for the session$`, func() error {
		return exactlyOneThread(w.threads, "sess-42")
	})

	sc.Step(`^sutra accepted the import but the crash hit before the thread id was recorded$`,
		func() error {
			tw := w.threads
			row := tw.store.rows["run-1"]
			row.ThreadRef, row.ImportState = "", devloop.ImportImporting
			return tw.store.Upsert(context.Background(), row)
		})

	// Shared with the review-resubmission feature, same as the sentence
	// above: whichever world the scenario set up is the one that means it.
	sc.Step(`^recovery replays under the same key$`, func() error {
		if w.submit != nil {
			return w.submit.replayResubmission()
		}
		tw := w.threads
		_, tw.err = tw.loop.RecoverImports(context.Background(), "KRI-7", "actor-1")
		return tw.err
	})

	sc.Step(`^sutra returns the original thread and the recorded id matches it$`, func() error {
		tw := w.threads
		row := tw.store.rows["run-1"]
		if row.ThreadRef == "" {
			return errors.New("no thread id was recorded")
		}
		if row.ThreadRef != tw.catalog.threads[0].id {
			return fmt.Errorf("recorded %q, want the original %q",
				row.ThreadRef, tw.catalog.threads[0].id)
		}
		return nil
	})

	sc.Step(`^exactly one thread exists for the session$`, func() error {
		return exactlyOneThread(w.threads, "sess-42")
	})
}

func exactlyOneThread(tw *threadWorld, session string) error {
	got := tw.catalog.forSession(session)
	if len(got) != 1 {
		return fmt.Errorf("%d threads exist for session %s", len(got), session)
	}
	return nil
}
