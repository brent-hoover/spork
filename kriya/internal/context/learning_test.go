package context_test

import (
	stdctx "context"
	"testing"
	"time"

	// Imported under its OWN name, with the standard library aliased instead —
	// see the note in context_test.go.
	"kriya/internal/context"
	"kriya/internal/fakes"
)

func learnings(t *testing.T) context.SQLLearnings {
	t.Helper()
	return context.SQLLearnings{DB: sqlDB(t, true), Now: fakes.NewClock(time.Unix(0, 0))}
}

func codeCapture() context.Capture {
	return context.Capture{
		Scope: context.ScopeProject, ProjectKey: "/target",
		Lesson: "pagination here is one-based", Module: "MOD-api", Pattern: "pagination",
		SourceKind: context.SourceReviewFinding, SourceRun: "run-1",
		SourceCommit: "C2", SourceRef: "round-1", SourceSession: "sess-42",
	}
}

func TestACapturedLearningKnowsItsOrigin(t *testing.T) {
	// The run, the TRIGGERING commit, and the finding that produced it.
	s := learnings(t)
	if err := s.Record(stdctx.Background(), codeCapture()); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, found, err := s.Provenance(stdctx.Background(), "pagination here is one-based")
	if err != nil || !found {
		t.Fatalf("provenance: %v found=%v", err, found)
	}
	if got.SourceRun != "run-1" || got.SourceCommit != "C2" || got.SourceRef != "round-1" {
		t.Errorf("recorded %+v", got)
	}
	if got.SourceKind != context.SourceReviewFinding {
		t.Errorf("recorded kind %q", got.SourceKind)
	}
}

func TestAResearchBackedLearningAnchorsOnItsDocument(t *testing.T) {
	s := learnings(t)
	c := codeCapture()
	c.SourceCommit, c.SourceDoc = "", "doc-v3"
	c.Lesson = "the spike's finding"
	if err := s.Record(stdctx.Background(), c); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, _, err := s.Provenance(stdctx.Background(), "the spike's finding")
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if got.SourceDoc != "doc-v3" || got.SourceCommit != "" {
		t.Errorf("recorded %+v", got)
	}
}

func TestAManualLearningRecordsTheOperatorAsItsOrigin(t *testing.T) {
	s := learnings(t)
	c := context.Capture{
		Scope: context.ScopeGlobal, Lesson: "never swallow an exception",
		Module: "MOD-api", Pattern: "error-handling", SourceKind: context.SourceOperator,
	}
	if err := s.Record(stdctx.Background(), c); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, _, err := s.Provenance(stdctx.Background(), "never swallow an exception")
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if got.SourceKind != context.SourceOperator {
		t.Errorf("recorded kind %q", got.SourceKind)
	}
	if got.SourceRun != "" || got.SourceCommit != "" {
		t.Errorf("a manual learning claims a run or commit: %+v", got)
	}
}

func TestAManualLearningFeedsForwardLikeACapturedOne(t *testing.T) {
	// Indistinguishable to the feed-forward path, which is what "first-class"
	// means: it reads the tags and nothing else.
	s := learnings(t)
	if err := s.Record(stdctx.Background(), context.Capture{
		Scope: context.ScopeGlobal, Lesson: "by hand", Module: "MOD-api",
		Pattern: "error-handling", SourceKind: context.SourceOperator,
	}); err != nil {
		t.Fatalf("record manual: %v", err)
	}
	if err := s.Record(stdctx.Background(), codeCapture()); err != nil {
		t.Fatalf("record captured: %v", err)
	}
	got, err := s.Matching(stdctx.Background(), []string{"MOD-api"}, nil)
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matched %+v", got)
	}
	for _, l := range got {
		if l.Module != "MOD-api" {
			t.Errorf("matched %+v", l)
		}
	}
}

func TestACrossProjectLearningIsVisibleOutsideItsOrigin(t *testing.T) {
	// Its whole purpose: a global lesson matches by tag, not by project.
	s := learnings(t)
	global := codeCapture()
	global.Scope, global.ProjectKey = context.ScopeGlobal, ""
	global.Lesson = "a lesson for every project"
	if err := s.Record(stdctx.Background(), global); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.Matching(stdctx.Background(), []string{"MOD-api"}, nil)
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(got) != 1 || got[0].Lesson != "a lesson for every project" {
		t.Errorf("matched %+v", got)
	}
}

func TestAProjectLearningWithoutItsKeyIsRefused(t *testing.T) {
	// The format cannot say "present exactly when", so the write enforces it —
	// and the write is the last moment anything knows enough to reject an
	// entry that would be unmatched forever after.
	s := learnings(t)
	c := codeCapture()
	c.ProjectKey = ""
	if err := s.Record(stdctx.Background(), c); err == nil {
		t.Fatal("a project learning with no key was recorded")
	}
}

func TestAGlobalLearningCarryingAProjectKeyIsRefused(t *testing.T) {
	s := learnings(t)
	c := codeCapture()
	c.Scope = context.ScopeGlobal
	if err := s.Record(stdctx.Background(), c); err == nil {
		t.Fatal("a global learning with a project key was recorded")
	}
}

func TestACapturedLearningNeedsExactlyOneAnchor(t *testing.T) {
	// A code correction points at the triggering commit, a research one at the
	// finding document version. Both or neither is a provenance nobody can
	// follow.
	s := learnings(t)
	neither := codeCapture()
	neither.SourceCommit = ""
	if err := s.Record(stdctx.Background(), neither); err == nil {
		t.Error("a captured learning with no anchor was recorded")
	}
	both := codeCapture()
	both.SourceDoc = "doc-v3"
	if err := s.Record(stdctx.Background(), both); err == nil {
		t.Error("a captured learning with two anchors was recorded")
	}
}

func TestACapturedLearningNeedsItsRunAndFinding(t *testing.T) {
	s := learnings(t)
	noRun := codeCapture()
	noRun.SourceRun = ""
	if err := s.Record(stdctx.Background(), noRun); err == nil {
		t.Error("a captured learning with no run was recorded")
	}
	noRef := codeCapture()
	noRef.SourceRef = ""
	if err := s.Record(stdctx.Background(), noRef); err == nil {
		t.Error("a captured learning with no finding was recorded")
	}
}

func TestAnOperatorLearningClaimsNoRunOrCommit(t *testing.T) {
	s := learnings(t)
	c := context.Capture{
		Scope: context.ScopeGlobal, Lesson: "by hand", Module: "MOD-api",
		Pattern: "p", SourceKind: context.SourceOperator, SourceRun: "run-1",
	}
	if err := s.Record(stdctx.Background(), c); err == nil {
		t.Fatal("an operator learning claiming a run was recorded")
	}
}

func TestALearningNeedsItsTags(t *testing.T) {
	// Matching runs on module and pattern. An entry with neither is one no run
	// will ever see.
	s := learnings(t)
	for _, blank := range []func(*context.Capture){
		func(c *context.Capture) { c.Lesson = "" },
		func(c *context.Capture) { c.Module = "" },
		func(c *context.Capture) { c.Pattern = "" },
	} {
		c := codeCapture()
		blank(&c)
		if err := s.Record(stdctx.Background(), c); err == nil {
			t.Errorf("a learning missing a required field was recorded: %+v", c)
		}
	}
}

func TestAnUnknownScopeOrSourceIsRefused(t *testing.T) {
	s := learnings(t)
	badScope := codeCapture()
	badScope.Scope = "team"
	if err := s.Record(stdctx.Background(), badScope); err == nil {
		t.Error("an unknown scope was recorded")
	}
	badSource := codeCapture()
	badSource.SourceKind = "a hunch"
	if err := s.Record(stdctx.Background(), badSource); err == nil {
		t.Error("an unknown source kind was recorded")
	}
}

func TestALearningOutlivesItsBuild(t *testing.T) {
	// Nothing about a finished run removes it: the store is keyed by nothing
	// the run owns.
	s := learnings(t)
	project := codeCapture()
	global := codeCapture()
	global.Scope, global.ProjectKey, global.Lesson = context.ScopeGlobal, "", "the global one"
	for _, c := range []context.Capture{project, global} {
		if err := s.Record(stdctx.Background(), c); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	got, err := s.Matching(stdctx.Background(), []string{"MOD-api"}, nil)
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("matched %+v after the build that produced them finished", got)
	}
}

func TestAnUnwritableLearningStoreFails(t *testing.T) {
	s := context.SQLLearnings{DB: sqlDB(t, false), Now: fakes.NewClock(time.Unix(0, 0))}
	if err := s.Record(stdctx.Background(), codeCapture()); err == nil {
		t.Error("a write to a missing table reported success")
	}
	if _, _, err := s.Provenance(stdctx.Background(), "anything"); err == nil {
		t.Error("a missing table read as a learning with no provenance")
	}
}

func TestAnUnrecordedLessonHasNoProvenance(t *testing.T) {
	s := learnings(t)
	_, found, err := s.Provenance(stdctx.Background(), "never said")
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if found {
		t.Error("a lesson nobody recorded had provenance")
	}
}
