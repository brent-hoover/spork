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
	// pair is the pair-loop scenarios' state, nil until one starts.
	pair *pairWorld
	// gates is the gate scenarios' state, nil until one starts.
	gates *gateWorld
}

func newWorld() *world {
	return &world{
		store:    newMemSnapshots(),
		targets:  newMemTargets(),
		attempts: newMemAttempts(),
		token:    "token-1",
		tracker:  &recordingTracker{},
		agent: fakes.NewAgent(
			`{"tickets":[{"title":"walking skeleton","body":"","criteria":["AC-valid-url"]}]}`),
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
		Now:       fakes.NewClock(time.Unix(0, 0)),
	}
	w.err = cli.Build(ctx, &w.out, in, w.dir, "01a02852-0000-7000-8000-000000000000", w.token, nil)
	if w.err == nil {
		if t, found, _ := w.targets.Find(ctx, w.dir); found && w.firstEpicID == "" {
			w.firstEpicID = t.EpicID
		}
		w.tickets = w.tracker.tickets
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
	if strings.HasPrefix(title, "Build ") {
		r.epics++
		return "epic-1", nil
	}
	r.tickets = append(r.tickets, planner.Ticket{Title: title})
	return fmt.Sprintf("issue-%d", r.issues), nil
}

func (r *recordingTracker) AddRelation(context.Context, string, string, string, string, string) error {
	r.relations++
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
	w.agent = fakes.NewAgent(`{"tickets":[{"title":"walking skeleton","body":"","criteria":[` +
		strings.Join(quoted, ",") + `]}]}`)
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
