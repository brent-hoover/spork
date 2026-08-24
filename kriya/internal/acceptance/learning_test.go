//go:build acceptance

package acceptance

import (
	stdctx "context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"

	kctx "kriya/internal/context"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/reviewbridge"
)

// learningWorld is one learning-loop scenario's state.
type learningWorld struct {
	store   kctx.SQLLearnings
	db      *sql.DB
	err     error
	lessons []string
}

func (w *world) newLearning() (*learningWorld, error) {
	dir, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "l.db"))
	if err != nil {
		return nil, err
	}
	w.cleanup = append(w.cleanup, func() { _ = db.Close() })
	for _, schema := range []string{kctx.Migration, kctx.ProvenanceMigration} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				return nil, err
			}
		}
	}
	l := &learningWorld{
		db: db, store: kctx.SQLLearnings{DB: db, Now: fakes.NewClock(time.Unix(0, 0))},
	}
	w.learn = l
	return l, nil
}

func registerLearningLoop(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a review finding is fixed in the pair loop$`, func() error {
		l, err := w.newLearning()
		if err != nil {
			return err
		}
		// The real loop, so the capture happens where the correction does.
		store := &sessionStore{rows: map[string]devloop.Session{}}
		rev := newFakeRoborev()
		rev.reports = []string{"- **Severity**: Low\n- **Problem**: pagination is zero-based"}
		loop := devloop.Loop{
			Agent: &fakes.Agent{Replies: replies("sess-42"), Repeat: true},
			Store: store, Commit: &seqCommitter{},
			Review: reviewbridge.Bridge{
				Repo: "/repo", Store: &memRounds{rev: rev}, Rev: rev,
				Now: fakes.NewClock(time.Unix(0, 0)),
			},
			Learnings: l.store, MaxRounds: 5,
			Now: fakes.NewClock(time.Unix(0, 0)),
		}
		work, err := w.tempDir()
		if err != nil {
			return err
		}
		_, l.err = loop.Work(stdctx.Background(), devloop.Request{
			Run: "run-1", Ticket: "KRI-1", Title: "Create a short link",
			Workspace: work, ProjectKey: "/target", Modules: []string{"MOD-api"},
		})
		return l.err
	})

	sc.Step(`^a learning is recorded then and there, stating what went wrong and how to avoid it$`,
		func() error {
			got, err := w.learn.store.Matching(stdctx.Background(), "/target", []string{"MOD-api"}, nil)
			if err != nil {
				return err
			}
			if len(got) != 1 {
				return fmt.Errorf("recorded %d learnings for one finding", len(got))
			}
			if got[0].Lesson == "" {
				return errors.New("the learning says nothing")
			}
			w.learn.lessons = []string{got[0].Lesson}
			return nil
		})

	sc.Step(`^it is tagged with the module and the failure pattern$`, func() error {
		got, err := w.learn.store.Matching(stdctx.Background(), "/target", []string{"MOD-api"}, nil)
		if err != nil {
			return err
		}
		if got[0].Module != "MOD-api" || got[0].Pattern == "" {
			return fmt.Errorf("tagged %+v — an untagged learning is one no run will see", got[0])
		}
		return nil
	})

	sc.Step(`^a gate failure is diagnosed, an SA direction lands, or the PO rejects a run$`,
		func() error {
			l, err := w.newLearning()
			if err != nil {
				return err
			}
			// Each kind records the same way, through the same write.
			for i, kind := range []string{
				kctx.SourceGateFailure, kctx.SourceSADirection, kctx.SourcePORejection,
			} {
				lesson := fmt.Sprintf("lesson from %s", kind)
				if err := l.store.Record(stdctx.Background(), kctx.Capture{
					Scope: kctx.ScopeProject, ProjectKey: "/target", Lesson: lesson,
					Module: "MOD-api", Pattern: fmt.Sprintf("pattern-%d", i),
					SourceKind: kind, SourceRun: "run-1",
					SourceCommit: "C2", SourceRef: "ref-1",
				}); err != nil {
					return err
				}
				l.lessons = append(l.lessons, lesson)
			}
			return nil
		})

	sc.Step(`^each produces its learning the same way, not in a post-mortem$`, func() error {
		l := w.learn
		for _, lesson := range l.lessons {
			got, found, err := l.store.Provenance(stdctx.Background(), lesson)
			if err != nil || !found {
				return fmt.Errorf("%q has no provenance: %v", lesson, err)
			}
			// Anchored on the TRIGGERING commit, which is what makes it a
			// capture at the moment of correction rather than a reconstruction.
			if got.SourceRun == "" || got.SourceCommit == "" || got.SourceRef == "" {
				return fmt.Errorf("%q is untraceable: %+v", lesson, got)
			}
		}
		return nil
	})

	sc.Step(`^the operator writes a learning from their own experience with the project$`,
		func() error {
			_, err := w.newLearning()
			return err
		})

	sc.Step(`^they add it manually$`, func() error {
		l := w.learn
		l.err = l.store.Record(stdctx.Background(), kctx.Capture{
			Scope: kctx.ScopeGlobal, Lesson: "never swallow an exception",
			Module: "MOD-api", Pattern: "error-handling", SourceKind: kctx.SourceOperator,
		})
		return l.err
	})

	sc.Step(`^it carries the same scope and tags as a captured learning$`, func() error {
		got, found, err := w.learn.store.Provenance(stdctx.Background(), "never swallow an exception")
		if err != nil || !found {
			return fmt.Errorf("the manual learning was not recorded: %v", err)
		}
		if got.Scope != kctx.ScopeGlobal || got.Module != "MOD-api" || got.Pattern == "" {
			return fmt.Errorf("recorded %+v", got)
		}
		return nil
	})

	sc.Step(`^the feed-forward path treats it exactly like a captured one$`, func() error {
		l := w.learn
		if err := l.store.Record(stdctx.Background(), kctx.Capture{
			Scope: kctx.ScopeProject, ProjectKey: "/target", Lesson: "a captured one",
			Module: "MOD-api", Pattern: "error-handling",
			SourceKind: kctx.SourceReviewFinding, SourceRun: "run-1",
			SourceCommit: "C2", SourceRef: "round-1",
		}); err != nil {
			return err
		}
		got, err := l.store.Matching(stdctx.Background(), "/target", []string{"MOD-api"}, nil)
		if err != nil {
			return err
		}
		if len(got) != 2 {
			return fmt.Errorf("matched %+v", got)
		}
		// Indistinguishable here: the path reads the tags and nothing else.
		for _, learning := range got {
			if learning.Module != "MOD-api" {
				return fmt.Errorf("matched %+v", learning)
			}
		}
		return nil
	})

	sc.Step(`^a learning scoped project-specific and another scoped cross-project$`, func() error {
		l, err := w.newLearning()
		if err != nil {
			return err
		}
		for _, c := range []kctx.Capture{
			{
				Scope: kctx.ScopeProject, ProjectKey: "/target", Lesson: "the project one",
				Module: "MOD-api", Pattern: "p", SourceKind: kctx.SourceReviewFinding,
				SourceRun: "run-1", SourceCommit: "C2", SourceRef: "round-1",
			},
			{
				Scope: kctx.ScopeGlobal, Lesson: "the cross-project one",
				Module: "MOD-api", Pattern: "p", SourceKind: kctx.SourceReviewFinding,
				SourceRun: "run-1", SourceCommit: "C2", SourceRef: "round-1",
			},
		} {
			if err := l.store.Record(stdctx.Background(), c); err != nil {
				return err
			}
		}
		return nil
	})

	sc.Step(`^the run and the build that produced them complete$`, func() error {
		// Nothing about a finished run touches the store: it is keyed by
		// nothing the run owns, which is what "outlive" means mechanically.
		return nil
	})

	sc.Step(`^both learnings persist durably$`, func() error {
		got, err := w.learn.store.Matching(stdctx.Background(), "/target", []string{"MOD-api"}, nil)
		if err != nil {
			return err
		}
		if len(got) != 2 {
			return fmt.Errorf("matched %+v after the build completed", got)
		}
		return nil
	})

	sc.Step(`^the cross-project learning is visible outside its project of origin$`, func() error {
		// A reader working on a DIFFERENT project still matches it, because a
		// global learning is scoped to no project — which is precisely what
		// makes it cross-project.
		fresh := kctx.SQLLearnings{DB: w.learn.db, Now: fakes.NewClock(time.Unix(0, 0))}
		got, err := fresh.Matching(stdctx.Background(), "/elsewhere", []string{"MOD-api"}, nil)
		if err != nil {
			return err
		}
		for _, l := range got {
			if l.Lesson == "the cross-project one" {
				return nil
			}
		}
		return fmt.Errorf("the cross-project learning is not visible: %+v", got)
	})

	sc.Step(`^a learning tagged with module "([^"]*)" was captured in an earlier run$`,
		func(module string) error {
			l, err := w.newLearning()
			if err != nil {
				return err
			}
			return l.store.Record(stdctx.Background(), kctx.Capture{
				Scope: kctx.ScopeProject, ProjectKey: "/target",
				Lesson: "pagination here is one-based", Module: moduleID(module),
				Pattern: "pagination", SourceKind: kctx.SourceReviewFinding,
				SourceRun: "run-earlier", SourceCommit: "C1", SourceRef: "round-1",
			})
		})

	sc.Step(`^a new run starts whose ticket touches "([^"]*)"$`, func(module string) error {
		c := w.newContext()
		c.learnings = nil
		c.ticket.Modules = []string{moduleID(module)}
		c.ticket.ProjectKey = "/target"
		c.spec.Modules[0].ID = moduleID(module)
		return nil
	})

	sc.Step(`^the learning is injected into that run's context$`, func() error {
		// Through the REAL store, so what the assembler receives is what a
		// later run would actually be given.
		c := w.ctxw
		a := kctx.Assembler{
			Store: c.bundles, Learnings: w.learn.store, Now: fakes.NewClock(time.Unix(0, 0)),
		}
		bundle, err := a.Assemble(stdctx.Background(), "run-2", c.spec, c.ticket, "")
		if err != nil {
			return err
		}
		if !strings.Contains(string(bundle.Content), "pagination here is one-based") {
			return fmt.Errorf("the learning was not injected: %s", bundle.Content)
		}
		return nil
	})

	sc.Step(`^a cross-project learning matching a later build's ticket$`, func() error {
		return w.learn.store.Record(stdctx.Background(), kctx.Capture{
			Scope: kctx.ScopeGlobal, Lesson: "a cross-project lesson",
			Module: "MOD-api", Pattern: "pagination",
			SourceKind: kctx.SourceReviewFinding, SourceRun: "run-elsewhere",
			SourceCommit: "C9", SourceRef: "round-9",
		})
	})

	sc.Step(`^it is injected there too$`, func() error {
		c := w.ctxw
		a := kctx.Assembler{
			Store: c.bundles, Learnings: w.learn.store, Now: fakes.NewClock(time.Unix(0, 0)),
		}
		bundle, err := a.Assemble(stdctx.Background(), "run-3", c.spec, c.ticket, "")
		if err != nil {
			return err
		}
		if !strings.Contains(string(bundle.Content), "a cross-project lesson") {
			return fmt.Errorf("the cross-project learning was not injected: %s", bundle.Content)
		}
		return nil
	})

	sc.Step(`^a captured learning from a code-backed correction$`, func() error {
		l, err := w.newLearning()
		if err != nil {
			return err
		}
		return l.store.Record(stdctx.Background(), kctx.Capture{
			Scope: kctx.ScopeProject, ProjectKey: "/target", Lesson: "code-backed",
			Module: "MOD-api", Pattern: "p", SourceKind: kctx.SourceReviewFinding,
			SourceRun: "run-1", SourceCommit: "C2", SourceRef: "round-1",
		})
	})

	sc.Step(`^it records the run, the triggering commit, and the finding or event that produced it$`,
		func() error {
			got, found, err := w.learn.store.Provenance(stdctx.Background(), "code-backed")
			if err != nil || !found {
				return fmt.Errorf("no provenance: %v found=%v", err, found)
			}
			if got.SourceRun != "run-1" || got.SourceCommit != "C2" || got.SourceRef != "round-1" {
				return fmt.Errorf("recorded %+v", got)
			}
			return nil
		})

	sc.Step(`^a captured learning from a research-backed correction$`, func() error {
		return w.learn.store.Record(stdctx.Background(), kctx.Capture{
			Scope: kctx.ScopeProject, ProjectKey: "/target", Lesson: "research-backed",
			Module: "MOD-api", Pattern: "p", SourceKind: kctx.SourceSADirection,
			SourceRun: "run-2", SourceDoc: "doc-v3", SourceRef: "event-9",
		})
	})

	sc.Step(`^it records the run, the finding document version, and the event that produced it$`,
		func() error {
			got, found, err := w.learn.store.Provenance(stdctx.Background(), "research-backed")
			if err != nil || !found {
				return fmt.Errorf("no provenance: %v found=%v", err, found)
			}
			if got.SourceRun != "run-2" || got.SourceDoc != "doc-v3" || got.SourceRef != "event-9" {
				return fmt.Errorf("recorded %+v", got)
			}
			// Exactly one anchor, matching the run kind.
			if got.SourceCommit != "" {
				return fmt.Errorf("a research-backed learning also names a commit: %+v", got)
			}
			return nil
		})

	sc.Step(`^a manual learning$`, func() error {
		return w.learn.store.Record(stdctx.Background(), kctx.Capture{
			Scope: kctx.ScopeGlobal, Lesson: "by hand", Module: "MOD-api",
			Pattern: "p", SourceKind: kctx.SourceOperator,
		})
	})

	sc.Step(`^it records the operator as its origin$`, func() error {
		got, found, err := w.learn.store.Provenance(stdctx.Background(), "by hand")
		if err != nil || !found {
			return fmt.Errorf("no provenance: %v found=%v", err, found)
		}
		if got.SourceKind != kctx.SourceOperator {
			return fmt.Errorf("recorded kind %q", got.SourceKind)
		}
		if got.SourceRun != "" || got.SourceCommit != "" || got.SourceDoc != "" {
			return fmt.Errorf("a manual learning claims a run or an anchor: %+v", got)
		}
		return nil
	})
}
