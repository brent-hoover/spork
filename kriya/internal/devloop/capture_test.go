package devloop_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kctx "kriya/internal/context"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/reviewbridge"
)

// fakeThreads records every import, so a second one for a session is visible.
type fakeThreads struct {
	// byKey is sutra's behaviour: a replayed key returns the original thread.
	byKey  map[string]string
	calls  []importCall
	err    error
	nextID int
}

type importCall struct {
	title      string
	transcript string
	session    string
	issue      string
	actor      string
	key        string
}

func newFakeThreads() *fakeThreads {
	return &fakeThreads{byKey: map[string]string{}}
}

func (f *fakeThreads) Import(
	_ context.Context, title string, transcript json.RawMessage,
	session, issue, actor, key string,
) (string, error) {
	f.calls = append(f.calls, importCall{
		title: title, transcript: string(transcript), session: session,
		issue: issue, actor: actor, key: key,
	})
	if f.err != nil {
		return "", f.err
	}
	if id, ok := f.byKey[key]; ok {
		return id, nil
	}
	f.nextID++
	id := "thread-" + string(rune('0'+f.nextID))
	f.byKey[key] = id
	return id, nil
}

func captureRequest(t *testing.T) devloop.Request {
	t.Helper()
	req := request()
	req.Workspace = t.TempDir()
	req.Issue = "issue-7"
	req.Actor = "actor-1"
	return req
}

func captureLoop(store *memStore, ag *fakes.Agent, threads *fakeThreads) devloop.Loop {
	return devloop.Loop{
		Agent: ag, Store: store, Threads: threads,
		Now: fakes.NewClock(time.Unix(0, 0)),
	}
}

func TestAFinishedSessionsTranscriptReachesTheCatalog(t *testing.T) {
	store, threads := &memStore{}, newFakeThreads()
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	req := captureRequest(t)
	got, err := captureLoop(store, ag, threads).Work(context.Background(), req)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(threads.calls) != 1 {
		t.Fatalf("imported %d times", len(threads.calls))
	}
	call := threads.calls[0]
	if call.session != got.SessionID || call.issue != "issue-7" || call.actor != "actor-1" {
		t.Errorf("imported %+v", call)
	}
	if got.ThreadRef == "" || got.ImportState != devloop.ImportImported {
		t.Errorf("session records %q in state %q", got.ThreadRef, got.ImportState)
	}
}

func TestTheTranscriptCarriesEveryTurn(t *testing.T) {
	// The transcript is the audit trail and the learning loop's fuel; a
	// transcript holding only the first prompt is neither.
	store, threads := &memStore{}, newFakeThreads()
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low\n- **Location**: `a.go:1`",
	}
	loop := pairLoop(store, ag, c, rev, 5)
	loop.Threads = threads
	if _, err := loop.Work(context.Background(), captureRequest(t)); err != nil {
		t.Fatalf("work: %v", err)
	}
	transcript := threads.calls[0].transcript
	for _, want := range []string{"Implement this ticket", "a.go:1"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the transcript omits %q: %s", want, transcript)
		}
	}
}

func TestTheReferenceAndKeyArePersistedBeforeTheCall(t *testing.T) {
	// A crash between the write and the call must leave a row that says an
	// import may be in flight, carrying exactly what the replay needs.
	store, threads := &memStore{}, newFakeThreads()
	threads.err = errors.New("sutra unreachable")
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	if _, err := captureLoop(store, ag, threads).Work(context.Background(), captureRequest(t)); err == nil {
		t.Fatal("an import that never landed read as success")
	}
	row := store.only(t)
	if row.ImportState != devloop.ImportImporting {
		t.Errorf("state is %q, so the crash window is invisible", row.ImportState)
	}
	if row.TranscriptRef == "" || row.ImportKey == "" {
		t.Errorf("the row cannot rebuild its own call: %+v", row)
	}
	if _, err := os.Stat(row.TranscriptRef); err != nil {
		t.Errorf("the transcript reference points nowhere: %v", err)
	}
}

func TestRecoveryReplaysUnderTheSameKeyAndLandsOneThread(t *testing.T) {
	store, threads := &memStore{}, newFakeThreads()
	threads.err = errors.New("sutra unreachable")
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := captureLoop(store, ag, threads)
	if _, err := loop.Work(context.Background(), captureRequest(t)); err == nil {
		t.Fatal("expected the import failure")
	}
	firstKey := store.only(t).ImportKey

	threads.err = nil
	n, err := loop.RecoverImports(context.Background(), "issue-7", "actor-1")
	if err != nil {
		t.Fatalf("recover imports: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d imports", n)
	}
	if threads.calls[len(threads.calls)-1].key != firstKey {
		t.Error("recovery replayed under a different key")
	}
	if len(threads.byKey) != 1 {
		t.Errorf("%d threads exist for one session", len(threads.byKey))
	}
}

func TestAnImportThatLandedReturnsItsOriginalThread(t *testing.T) {
	// sutra accepted the import but the crash hit before the id was recorded.
	// Replaying under the same key must return the original, not a second.
	store, threads := &memStore{}, newFakeThreads()
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := captureLoop(store, ag, threads)
	got, err := loop.Work(context.Background(), captureRequest(t))
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	landed := got.ThreadRef

	// Rewind to the crash window: the import landed, the id was never written.
	got.ThreadRef, got.ImportState = "", devloop.ImportImporting
	if err := store.Upsert(context.Background(), got); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := loop.RecoverImports(context.Background(), "issue-7", "actor-1"); err != nil {
		t.Fatalf("recover imports: %v", err)
	}
	if store.only(t).ThreadRef != landed {
		t.Errorf("recorded %q, want the original %q", store.only(t).ThreadRef, landed)
	}
	if len(threads.byKey) != 1 {
		t.Errorf("%d threads exist for one session", len(threads.byKey))
	}
}

func TestTheTranscriptIsNeverRewritten(t *testing.T) {
	// A transcript regenerated at recovery time could differ from the one
	// sutra may already hold.
	store, threads := &memStore{}, newFakeThreads()
	threads.err = errors.New("sutra unreachable")
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := captureLoop(store, ag, threads)
	req := captureRequest(t)
	if _, err := loop.Work(context.Background(), req); err == nil {
		t.Fatal("expected the import failure")
	}
	ref := store.only(t).TranscriptRef
	original, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// A second session over the same workspace must not overwrite it.
	if _, err := loop.Work(context.Background(), req); err == nil {
		t.Fatal("expected the import failure")
	}
	after, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("read again: %v", err)
	}
	if string(original) != string(after) {
		t.Error("the transcript was rewritten under a landed import's reference")
	}
}

func TestTheKeyIsDeterministicPerSession(t *testing.T) {
	// Recovery must present the SAME key. A key mixing in anything recovery
	// reconstructs would differ and import a second thread.
	store, threads := &memStore{}, newFakeThreads()
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := captureLoop(store, ag, threads)
	req := captureRequest(t)
	first, err := loop.Work(context.Background(), req)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	second, err := loop.Work(context.Background(), req)
	if err != nil {
		t.Fatalf("work again: %v", err)
	}
	if first.SessionID != second.SessionID {
		t.Skip("the fake changed session id; determinism is not testable this way")
	}
	if first.ImportKey != second.ImportKey {
		t.Errorf("two runs of one session keyed %q and %q", first.ImportKey, second.ImportKey)
	}
}

func TestAnUnwritableWorkspaceStopsCapture(t *testing.T) {
	// Silently skipping capture would lose the transcript, which is the one
	// thing AC-thread-no-loss forbids.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, threads := &memStore{}, newFakeThreads()
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	req := captureRequest(t)
	req.Workspace = file
	if _, err := captureLoop(store, ag, threads).Work(context.Background(), req); err == nil {
		t.Fatal("a transcript that could not be written was passed over")
	}
	if len(threads.calls) != 0 {
		t.Error("an import was attempted with no persisted transcript")
	}
}

func TestNoCatalogConfiguredSkipsCapture(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	got, err := loop.Work(context.Background(), captureRequest(t))
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if got.ImportState != "" {
		t.Errorf("import state is %q with no catalog configured", got.ImportState)
	}
}

func TestContextAssemblyFailureStopsTheSession(t *testing.T) {
	// An agent started without its context would work from the prompt alone —
	// no ACs, no module law, no learnings — which is not the ticket it was
	// given.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := devloop.Loop{
		Agent: ag, Store: store, Context: failingContexts{},
		Now: fakes.NewClock(time.Unix(0, 0)),
	}
	if _, err := loop.Work(context.Background(), captureRequest(t)); err == nil {
		t.Fatal("a session started with no assembled context")
	}
	if len(ag.Requests) != 0 {
		t.Error("the agent ran without its context")
	}
}

// failingContexts refuses to assemble.
type failingContexts struct{}

func (failingContexts) Assemble(
	context.Context, string, kctx.Spec, kctx.Ticket, string,
) (kctx.Bundle, error) {
	return kctx.Bundle{}, errors.New("learning store unavailable")
}

func TestTheAssembledContextReachesTheAgentAsASystemFile(t *testing.T) {
	// Content, not instruction: the ticket's law competes with the task if it
	// is stated as one.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	req := captureRequest(t)
	loop := devloop.Loop{
		Agent: ag, Store: store, Context: bundleContexts{},
		Now: fakes.NewClock(time.Unix(0, 0)),
	}
	got, err := loop.Work(context.Background(), req)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if got.SystemFile == "" {
		t.Fatal("no system file was recorded on the session")
	}
	if ag.Requests[0].SystemFile != got.SystemFile {
		t.Errorf("the agent was pointed at %q", ag.Requests[0].SystemFile)
	}
	body, err := os.ReadFile(got.SystemFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "AC-valid-url") {
		t.Errorf("the file does not hold the assembled context: %s", body)
	}
}

// bundleContexts assembles a bundle naming the ticket's criteria.
type bundleContexts struct{}

func (bundleContexts) Assemble(
	_ context.Context, build string, _ kctx.Spec, ticket kctx.Ticket, _ string,
) (kctx.Bundle, error) {
	body, err := json.Marshal(map[string]any{"criteria": ticket.Criteria})
	if err != nil {
		return kctx.Bundle{}, err
	}
	return kctx.Bundle{Build: build, Content: body}, nil
}

func TestASessionWriteFailureStopsBeforeTheAgentRuns(t *testing.T) {
	// AC-thread-no-loss: a crash mid-session must leave a row recovery can
	// terminate. Running the agent with no row would make that impossible.
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	loop := devloop.Loop{
		Agent: ag, Store: failingSessions{}, Now: fakes.NewClock(time.Unix(0, 0)),
	}
	if _, err := loop.Work(context.Background(), captureRequest(t)); err == nil {
		t.Fatal("a session with no row read as started")
	}
	if len(ag.Requests) != 0 {
		t.Error("the agent ran with no session row")
	}
}

// failingSessions refuses every write.
type failingSessions struct{}

func (failingSessions) Upsert(context.Context, devloop.Session) error {
	return errors.New("disk full")
}

func (failingSessions) Importing(context.Context) ([]devloop.Session, error) {
	return nil, errors.New("disk full")
}

func (failingSessions) Find(context.Context, string) (devloop.Session, bool, error) {
	return devloop.Session{}, false, errors.New("disk full")
}

func TestAnUnreadableSessionListStopsImportRecovery(t *testing.T) {
	loop := devloop.Loop{
		Store: failingSessions{}, Threads: newFakeThreads(),
		Now: fakes.NewClock(time.Unix(0, 0)),
	}
	if _, err := loop.RecoverImports(context.Background(), "", "actor-1"); err == nil {
		t.Fatal("an unreadable store read as no imports in flight")
	}
}

func TestAMissingTranscriptStopsTheReplay(t *testing.T) {
	// The reference is what a replay reproduces the request from. Without the
	// file there is nothing to re-issue, and inventing one would send sutra a
	// transcript it may already differ from.
	store, threads := &memStore{}, newFakeThreads()
	if err := store.Upsert(context.Background(), devloop.Session{
		Run: "run-1", Ticket: "KRI-1", SessionID: "sess-42",
		TranscriptRef: filepath.Join(t.TempDir(), "gone.json"),
		ImportKey:     "key-1", ImportState: devloop.ImportImporting,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	loop := devloop.Loop{Store: store, Threads: threads, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.RecoverImports(context.Background(), "", "actor-1"); err == nil {
		t.Fatal("a replay with no transcript read as success")
	}
	if len(threads.calls) != 0 {
		t.Error("an import was attempted with no transcript")
	}
}

// learningRecorder records what a correction taught.
type learningRecorder struct {
	captured []kctx.Capture
	err      error
}

func (r *learningRecorder) Record(_ context.Context, c kctx.Capture) error {
	if r.err != nil {
		return r.err
	}
	r.captured = append(r.captured, c)
	return nil
}

func TestAFindingBecomesALearningAtTheMomentOfCorrection(t *testing.T) {
	// Not reconstructed afterwards, when what actually went wrong is a guess.
	store, threads := &memStore{}, newFakeThreads()
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low\n- **Problem**: pagination is zero-based here",
	}
	recorder := &learningRecorder{}
	loop := pairLoop(store, ag, c, rev, 5)
	loop.Threads = threads
	loop.Learnings = recorder
	req := captureRequest(t)
	req.ProjectKey = "/target"
	req.Modules = []string{"MOD-api"}
	if _, err := loop.Work(context.Background(), req); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(recorder.captured) != 1 {
		t.Fatalf("captured %d learnings for one finding", len(recorder.captured))
	}
	got := recorder.captured[0]
	if got.Module != "MOD-api" || got.Pattern == "" {
		t.Errorf("captured %+v — a learning with no tags is one no run will see", got)
	}
	if got.SourceKind != kctx.SourceReviewFinding || got.SourceRun != req.Run {
		t.Errorf("captured %+v", got)
	}
	// The TRIGGERING commit, not the fix: the lesson is about the code that
	// was reviewed.
	if got.SourceCommit != c.shas[0] {
		t.Errorf("anchored on %q, want the reviewed commit %q", got.SourceCommit, c.shas[0])
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the captured learning is not recordable: %v", err)
	}
}

func TestACleanRoundTeachesNothing(t *testing.T) {
	// A learning is a CORRECTION. A round that found nothing corrected
	// nothing, and recording one would fill the store with noise every future
	// run has to read past.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	recorder := &learningRecorder{}
	loop := pairLoop(store, ag, c, rev, 5)
	loop.Learnings = recorder
	req := captureRequest(t)
	req.ProjectKey = "/target"
	if _, err := loop.Work(context.Background(), req); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(recorder.captured) != 0 {
		t.Errorf("captured %+v from a clean round", recorder.captured)
	}
}

func TestALearningThatCannotBeRecordedStopsTheLoop(t *testing.T) {
	// Errors should never pass silently: a correction whose lesson vanished is
	// one the next run repeats.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings},
		findings: "- **Severity**: Low",
	}
	recorder := &learningRecorder{err: errors.New("disk full")}
	loop := pairLoop(store, ag, c, rev, 5)
	loop.Learnings = recorder
	req := captureRequest(t)
	req.ProjectKey = "/target"
	if _, err := loop.Work(context.Background(), req); err == nil {
		t.Fatal("a learning that vanished read as recorded")
	}
}
