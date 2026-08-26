//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kriya/internal/cli"
	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	_ "modernc.org/sqlite"

	"kriya/internal/specverify"
)

// world is one scenario's state.
//
// The verifier is REAL: intake's whole job is holding a spec to avspec's
// judgement, and a fake verifier would prove kriya against a restatement of
// kriya's own assumptions. The tracker and agent are fakes, because these
// scenarios are about what kriya does with an agent's answer, not about the
// agent's judgement or sutra's storage — both of which have their own
// integration tests.
type world struct {
	dir         string
	cleanup     []func()
	out         bytes.Buffer
	err         error
	store       *memSnapshots
	targets     *memTargets
	attempts    *memAttempts
	token       string
	retried     planner.IntakeAttempt
	reserved    []planner.IntakeAttempt
	tickets     []planner.Ticket
	firstEpicID string
	tracker     *recordingTracker
	agent       *fakes.Agent
	// plans and ticketRows are decomposition's own durable output. The world
	// reads its ticket set from HERE rather than from the tracker: the
	// tracker only ever sees a title and a body, so tickets recovered from it
	// carry no kind, no layers and no criteria — and an assertion about
	// those would pass against three empty structs.
	plans      *memPlans
	ticketRows *worldTickets
	// pair is the pair-loop scenarios' state, nil until one starts.
	pair *pairWorld
	// finding is the spike-finding scenarios' state, nil until one starts.
	finding *findingWorld
	// spike is the risk-first scenarios' state, nil until one starts.
	spike *spikeWorld
	// status is the CLI-status scenario's state, nil until it starts.
	status *statusWorld
	// fence is the pop-fence scenarios' state, nil until one starts.
	fence *fenceWorld
	// restore is the parked-plan scenarios' state, nil until one starts.
	restore *restoreWorld
	// retire is the retirement scenarios' state, nil until one starts.
	retire *retireWorld
	// head is the plan-head scenarios' state, nil until one starts.
	head *headWorld
	// resolve is the same-key-retry scenarios' state, nil until one starts.
	resolve *resolveWorld
	// close is the epic-close scenarios' state, nil until one starts.
	close *closeWorld
	// claim is the completion-submission scenarios' state, nil until one starts.
	claim *claimWorld
	// epoch is the completion-epoch scenarios' state, nil until one starts.
	epoch *epochWorld
	// detect is the completion-detection scenarios' state, nil until one starts.
	detect *detectWorld
	// gates is the gate scenarios' state, nil until one starts.
	gates *gateWorld
	// ctxw is the context-assembly scenarios' state, nil until one starts.
	ctxw *contextWorld
	// ws is the workspace scenarios' state, nil until one starts.
	ws *workspaceWorld
	// threads is the thread-capture scenarios' state, nil until one starts.
	threads *threadWorld
	// sa is the architect scenarios' state, nil until one starts.
	sa *saWorld
	// po is the product-owner scenarios' state, nil until one starts.
	po *poWorld
	// submit is the review-submission scenarios' state, nil until one starts.
	submit *submitWorld
	// merge is the merge-queue scenarios' state, nil until one starts.
	merge *mergeWorld
	// complete is the completion scenarios' state, nil until one starts.
	complete *completeWorld
	// pop is the pop-loop scenarios' state, nil until one starts.
	pop *popWorld
	// learn is the learning-loop scenarios' state, nil until one starts.
	learn *learningWorld
}

func newWorld() *world {
	return &world{
		store:      newMemSnapshots(),
		targets:    newMemTargets(),
		attempts:   newMemAttempts(),
		token:      "token-1",
		tracker:    &recordingTracker{},
		plans:      &memPlans{rows: map[string]planner.Plan{}},
		ticketRows: newWorldTickets(),
		agent: fakes.NewAgent(
			`{"tickets":[{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]}]}`),
	}
}

// tempDir makes a directory this scenario owns.
func (w *world) tempDir() (string, error) {
	dir, err := os.MkdirTemp("", "kriya-acceptance-")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	w.cleanup = append(w.cleanup, func() { _ = os.RemoveAll(dir) })
	return dir, nil
}

// recover runs whichever recovery the scenario is about.
//
// The step sentence is shared, so the world it built is what selects the
// module: a scenario that seeded a completion claim recovers completion, one
// that seeded a review round recovers rounds.
func (w *world) recover() error {
	switch {
	case w.finding != nil:
		_, err := w.finding.r.RecoverFindings(context.Background(),
			func(orchestrator.BuildRun) (string, error) { return "p-1", nil })
		return err
	case w.claim != nil:
		return w.claim.recover()
	case w.pair != nil:
		_, w.pair.err = w.pair.bridge.RecoverRounds(context.Background())
		return w.pair.err
	}
	return errors.New("no recovery is in scope: the scenario built no world")
}

// done releases what the scenario created.
func (w *world) done() {
	for _, fn := range w.cleanup {
		fn()
	}
}

// writeSpec puts a manifest on disk.
func (w *world) writeSpec(manifest string) error {
	dir, err := w.tempDir()
	if err != nil {
		return err
	}
	w.dir = dir
	if err := os.WriteFile(filepath.Join(dir, "avspec.yaml"), []byte(manifest), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// copyLinkshort copies the conformance example, which verifies ready with no
// findings and declares all six commands on every module.
func (w *world) copyLinkshort() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	dir, err := w.tempDir()
	if err != nil {
		return err
	}
	w.dir = dir
	return copyTree(filepath.Join(root, "avspec", "examples", "linkshort"), dir)
}

// run points kriya at the project, as the operator would.
func (w *world) run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	v, err := realVerifier()
	if err != nil {
		return err
	}
	in := planner.Intaker{
		Verify:    v,
		Snapshots: w.store,
		Targets:   w.targets,
		Attempts:  w.attempts,
		Tracker:   w.tracker,
		Agent:     w.agent,
		Plans:     w.plans,
		Tickets:   w.ticketRows,
		Now:       fakes.NewClock(time.Unix(0, 0)),
	}
	w.err = cli.Build(ctx, &w.out, in, w.dir, "01a02852-0000-7000-8000-000000000000", w.token, nil)
	if w.err == nil {
		if t, found, _ := w.targets.Find(ctx, w.dir); found && w.firstEpicID == "" {
			w.firstEpicID = t.EpicID
		}
		w.tickets, _ = w.ticketRows.ForTarget(ctx, w.dir)
	}
	return nil
}

func (w *world) refusal() (*planner.Refusal, error) {
	var r *planner.Refusal
	if !errors.As(w.err, &r) {
		return nil, fmt.Errorf("expected a refusal, got %v", w.err)
	}
	return r, nil
}

func (w *world) output() string { return w.out.String() }

func (w *world) snapshotCount() int {
	n, _ := w.store.Count(context.Background())
	return n
}

// realVerifier runs the actual avspec, under uv when the repo has not
// installed it.
func realVerifier() (specverify.CLI, error) {
	root, err := repoRoot()
	if err != nil {
		return specverify.CLI{}, err
	}
	return specverify.CLI{
		Argv:    []string{"uv", "run", "avspec"},
		WorkDir: filepath.Join(root, "avspec"),
	}, nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "avspec", "pyproject.toml")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("avspec/ not found above the working directory")
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// recordingTracker records what kriya asked the tracker to do.
type recordingTracker struct {
	projects, issues, relations, epics int
	// tickets records what decomposition asked for, so a scenario can assert
	// against what kriya actually filed rather than what the fake replied.
	tickets []planner.Ticket
	// wired records every relation, so a blocking edge that was or was not
	// created is a fact rather than a count.
	wired []wiredRelation
	// assignOrder is the sequence assignments went out in. sutra's work stack
	// is FIFO and no tracker-side priority is assumed, so the order IS the
	// risk-first guarantee.
	assignOrder []string
	// calls is every mutation in the order it was made, tagged by phase.
	// The barrier claims are claims about ORDER ACROSS KINDS — "every ticket
	// created before any relation is wired" — which per-kind counters cannot
	// answer: they record how many, never which came first.
	calls []string
	// titles names each issue, so an ordering assertion reads as an order of
	// tickets rather than of opaque ids.
	titles map[string]string
}

// phases returns the call log collapsed to the sequence of phases it visited.
// "create, create, relation, assign" collapses to create → relation → assign,
// and a plan that interleaved them shows the repeat.
func (r *recordingTracker) phases() []string {
	out := []string{}
	for _, c := range r.calls {
		if len(out) == 0 || out[len(out)-1] != c {
			out = append(out, c)
		}
	}
	return out
}

// titleOf names the ticket behind an issue id.
func (r *recordingTracker) titleOf(issue string) string { return r.titles[issue] }

// wiredRelation is one relation the tracker was asked for.
type wiredRelation struct{ from, kind, to string }

func newRecordingTracker() *recordingTracker { return &recordingTracker{} }

// blocks reports whether a blocking relation was wired between two issues.
func (r *recordingTracker) blocks(from, to string) bool {
	for _, rel := range r.wired {
		if rel.kind == "blocks" && rel.from == from && rel.to == to {
			return true
		}
	}
	return false
}

// reset clears the counters so a later run can be measured on its own.
func (r *recordingTracker) reset() {
	r.projects, r.issues, r.relations, r.epics = 0, 0, 0, 0
}

func (r *recordingTracker) CreateProject(context.Context, string, string, string, string) (string, error) {
	r.projects++
	return "project-1", nil
}

func (r *recordingTracker) CreateIssue(_ context.Context, _, title, _, _, _ string) (string, error) {
	r.issues++
	if r.titles == nil {
		r.titles = map[string]string{}
	}
	if strings.HasPrefix(title, "Build ") {
		r.epics++
		r.titles["epic-1"] = title
		return "epic-1", nil
	}
	r.calls = append(r.calls, "create")
	r.tickets = append(r.tickets, planner.Ticket{Title: title})
	id := fmt.Sprintf("issue-%d", r.issues)
	r.titles[id] = title
	return id, nil
}

func (r *recordingTracker) AddRelation(
	_ context.Context, from, kind, to, _, _ string,
) error {
	// parent_of is part of CREATING a ticket, not the relation phase: the
	// barrier the spec draws is around the BLOCKING wiring, which is what
	// decides whether a ticket is workable. Counting parenting as a relation
	// would make every correct plan look interleaved.
	if kind == "blocks" {
		r.calls = append(r.calls, "relation")
	}
	r.relations++
	r.wired = append(r.wired, wiredRelation{from: from, kind: kind, to: to})
	return nil
}

// enqueued reports whether anything reached the tracker.
func (r *recordingTracker) enqueued() bool {
	return r.projects+r.issues+r.relations > 0
}

type memSnapshots struct{ stored map[string]planner.Snapshot }

func newMemSnapshots() *memSnapshots {
	return &memSnapshots{stored: map[string]planner.Snapshot{}}
}

func (s *memSnapshots) Put(_ context.Context, snap planner.Snapshot) error {
	s.stored[snap.Hash] = snap
	return nil
}

func (s *memSnapshots) Get(_ context.Context, hash string) (planner.Snapshot, error) {
	snap, ok := s.stored[hash]
	if !ok {
		return planner.Snapshot{}, errors.New("no snapshot " + hash)
	}
	return snap, nil
}

func (s *memSnapshots) Count(_ context.Context) (int, error) { return len(s.stored), nil }

// only returns the single pinned snapshot.
func (s *memSnapshots) only() (planner.Snapshot, error) {
	if len(s.stored) != 1 {
		return planner.Snapshot{}, fmt.Errorf("expected exactly one snapshot, got %d", len(s.stored))
	}
	for _, snap := range s.stored {
		return snap, nil
	}
	return planner.Snapshot{}, errors.New("unreachable")
}

type memTargets struct {
	rows map[string]planner.BuildTarget
}

func newMemTargets() *memTargets { return &memTargets{rows: map[string]planner.BuildTarget{}} }

func (t *memTargets) Upsert(_ context.Context, b planner.BuildTarget) error {
	t.rows[b.TargetKey] = b
	return nil
}

func (t *memTargets) Find(_ context.Context, key string) (planner.BuildTarget, bool, error) {
	b, ok := t.rows[key]
	return b, ok, nil
}

func (t *memTargets) Pending(context.Context) ([]planner.BuildTarget, error) { return nil, nil }

func (t *memTargets) All(context.Context) ([]planner.BuildTarget, error) { return nil, nil }

// requiredCommands is what AC-intake-commands demands of every module.
var requiredCommands = specverify.RequiredCommands

// writeFeature drops the feature file the fixture's acceptance criterion
// references. avspec checks that a cited test file exists, so a manifest
// citing one that does not would not verify ready.
func (w *world) writeFeature() error {
	dir := filepath.Join(w.dir, "verification")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("make verification dir: %w", err)
	}
	body := "Feature: A\n\n  Scenario: the thing is done\n    Given a thing\n"
	if err := os.WriteFile(filepath.Join(dir, "REQ-a.feature"), []byte(body), 0o644); err != nil {
		return fmt.Errorf("write feature: %w", err)
	}
	return nil
}

// citeCriteria programs the fake PM to cite these ids.
func (w *world) citeCriteria(ids ...string) {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = `"` + id + `"`
	}
	w.agent = fakes.NewAgent(`{"tickets":[{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":[` +
		strings.Join(quoted, ",") + `],"layers":["http","store"]}]}`)
	// A supersession decomposes again, and the PM answering the same way is
	// exactly what "carried-forward tickets" means.
	w.agent.Repeat = true
}

// memAttempts is an in-memory AttemptStore for the acceptance world.
type memAttempts struct {
	byToken map[string]planner.IntakeAttempt
	next    map[string]int
	mapping map[string]planner.SpecMapping
}

func newMemAttempts() *memAttempts {
	return &memAttempts{
		byToken: map[string]planner.IntakeAttempt{},
		next:    map[string]int{},
		mapping: map[string]planner.SpecMapping{},
	}
}

func (m *memAttempts) Reserve(_ context.Context, token, targetKey string) (planner.IntakeAttempt, error) {
	if a, ok := m.byToken[token]; ok {
		return a, nil
	}
	m.next[targetKey]++
	a := planner.IntakeAttempt{
		Token: token, TargetKey: targetKey,
		Generation: m.next[targetKey], State: planner.AttemptPending,
	}
	m.byToken[token] = a
	return a, nil
}

func (m *memAttempts) Complete(_ context.Context, token, specHash string) error {
	a := m.byToken[token]
	a.SpecHash = specHash
	a.State = planner.AttemptComplete
	m.byToken[token] = a
	return nil
}

func (m *memAttempts) MapSpec(_ context.Context, s planner.SpecMapping) error {
	if cur, ok := m.mapping[s.TargetKey]; ok && cur.Generation >= s.Generation {
		return nil
	}
	m.mapping[s.TargetKey] = s
	return nil
}

func (m *memAttempts) Mapping(_ context.Context, targetKey string) (planner.SpecMapping, bool, error) {
	s, ok := m.mapping[targetKey]
	return s, ok, nil
}

// AssignIssue puts a ticket on the popping identity's work stack. Without it
// the tracker's pop never offers it to anyone.
func (r *recordingTracker) AssignIssue(
	_ context.Context, issue, _, _, _ string,
) error {
	r.calls = append(r.calls, "assign")
	r.assignOrder = append(r.assignOrder, issue)
	return nil
}

// worldTickets is the acceptance world's TicketStore.
//
// It keeps INSERTION ORDER as well as the rows, because several claims are
// about the order a decomposition filed its tickets in and a map would lose
// exactly that.
type worldTickets struct {
	rows  map[string][]planner.Ticket
	byKey map[string]planner.Ticket
}

func newWorldTickets() *worldTickets {
	return &worldTickets{rows: map[string][]planner.Ticket{}, byKey: map[string]planner.Ticket{}}
}

// Put upserts on (plan, ordinal), which is the real store's key. Keying on
// the issue id cannot work for a WRITE-AHEAD row: it is written before the
// create call, so it has no issue id yet.
func (t *worldTickets) Put(_ context.Context, targetKey string, ticket planner.Ticket) error {
	set := t.rows[targetKey]
	for n, existing := range set {
		if existing.Plan == ticket.Plan && existing.Ordinal == ticket.Ordinal {
			set[n] = ticket
			t.byKey[targetKey+"\x00"+ticket.IssueID] = ticket
			return nil
		}
	}
	t.rows[targetKey] = append(set, ticket)
	t.byKey[targetKey+"\x00"+ticket.IssueID] = ticket
	return nil
}

func (t *worldTickets) Find(_ context.Context, targetKey, issue string) (planner.Ticket, bool, error) {
	ticket, ok := t.byKey[targetKey+"\x00"+issue]
	return ticket, ok, nil
}

func (t *worldTickets) ForTarget(_ context.Context, targetKey string) ([]planner.Ticket, error) {
	return t.rows[targetKey], nil
}

func (t *worldTickets) Consume(
	_ context.Context, key string, ordinal int, disposition string,
) error {
	for _, set := range t.rows {
		for n, ticket := range set {
			if ticket.Plan == key && ticket.Ordinal == ordinal {
				set[n].Consumed, set[n].Disposition = true, disposition
				return nil
			}
		}
	}
	return nil
}

func (t *worldTickets) BumpDeferAttempt(
	_ context.Context, key string, ordinal int,
) (int, error) {
	for _, set := range t.rows {
		for n, ticket := range set {
			if ticket.Plan == key && ticket.Ordinal == ordinal {
				set[n].DeferAttempt++
				return set[n].DeferAttempt, nil
			}
		}
	}
	return 0, nil
}

func (t *worldTickets) ForPlan(_ context.Context, key string) ([]planner.Ticket, error) {
	var out []planner.Ticket
	for _, set := range t.rows {
		for _, ticket := range set {
			if ticket.Plan == key {
				out = append(out, ticket)
			}
		}
	}
	return out, nil
}
