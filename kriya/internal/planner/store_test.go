package planner_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"kriya/internal/planner"
)

func sqlDB(t *testing.T, schemas ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, schema := range schemas {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate: %v", err)
			}
		}
	}
	return db
}

func TestASnapshotSurvivesTheRoundTrip(t *testing.T) {
	s := planner.SQLSnapshots{DB: sqlDB(t, planner.Migration, planner.LawMigration)}
	want := planner.Snapshot{
		Hash:    "hash-1",
		Content: map[string]string{"avspec.yaml": "body"},
		ResolvedCommands: map[string]map[string]string{
			"engine": {"test": "go test ./..."},
		},
		Created: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
	}
	if err := s.Put(context.Background(), want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(context.Background(), "hash-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content["avspec.yaml"] != "body" {
		t.Errorf("content came back %v", got.Content)
	}
	if got.ResolvedCommands["engine"]["test"] != "go test ./..." {
		t.Errorf("commands came back %v", got.ResolvedCommands)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("created came back %s", got.Created)
	}
}

func TestPinningTheSameContentTwiceIsNotAnError(t *testing.T) {
	// Snapshots are content-addressed, so an identical hash is the same
	// snapshot and a re-intake of unchanged files must not fail.
	s := planner.SQLSnapshots{DB: sqlDB(t, planner.Migration, planner.LawMigration)}
	snap := planner.Snapshot{Hash: "hash-1", Created: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)}
	if err := s.Put(context.Background(), snap); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := s.Put(context.Background(), snap); err != nil {
		t.Fatalf("second put: %v", err)
	}
	n, err := s.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("pinned %d snapshots for one hash", n)
	}
}

func TestAnUnpinnedHashIsAnError(t *testing.T) {
	// Building against a snapshot nothing pinned would build against a spec
	// that was never admitted.
	s := planner.SQLSnapshots{DB: sqlDB(t, planner.Migration, planner.LawMigration)}
	if _, err := s.Get(context.Background(), "hash-absent"); err == nil {
		t.Fatal("a hash that was never pinned read as a snapshot")
	}
}

func TestASnapshotStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := planner.SQLSnapshots{DB: sqlDB(t)}
	if _, err := s.Count(context.Background()); err == nil {
		t.Error("a missing table counted as zero snapshots")
	}
	if err := s.Put(context.Background(), planner.Snapshot{Hash: "h"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}

func TestATargetSurvivesEverythingRecoveryNeeds(t *testing.T) {
	// A write-ahead row that cannot rebuild its own call is not write-ahead:
	// a recovery that reconstructed a different actor would present a
	// different idempotency key and create a duplicate.
	s := planner.SQLTargets{DB: sqlDB(t, planner.TargetMigration)}
	want := planner.BuildTarget{
		TargetKey: "key-1", SpecHash: "hash-1", ProjectID: "p-1", EpicID: "e-1",
		EpicState: planner.EpicPending, ProjectKey: "KRIYA0a1b2c",
		Name: "kriya", Actor: "actor-1",
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "key-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestAFirstIntakeHasNoRowRatherThanAnError(t *testing.T) {
	s := planner.SQLTargets{DB: sqlDB(t, planner.TargetMigration)}
	_, found, err := s.Find(context.Background(), "key-absent")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("a target that was never recorded was found")
	}
}

func TestPendingTargetsComeBackInADeclaredOrder(t *testing.T) {
	s := planner.SQLTargets{DB: sqlDB(t, planner.TargetMigration)}
	for _, target := range []planner.BuildTarget{
		{TargetKey: "key-b", EpicState: planner.EpicPending},
		{TargetKey: "key-a", EpicState: planner.EpicPending},
		{TargetKey: "key-c", EpicState: "ready"},
	} {
		if err := s.Upsert(context.Background(), target); err != nil {
			t.Fatalf("upsert %s: %v", target.TargetKey, err)
		}
	}
	got, err := s.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(got) != 2 || got[0].TargetKey != "key-a" || got[1].TargetKey != "key-b" {
		t.Fatalf("listed %+v", got)
	}
}

func TestATargetStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	// A short list reads exactly like "nothing left to recover".
	s := planner.SQLTargets{DB: sqlDB(t)}
	if _, err := s.Pending(context.Background()); err == nil {
		t.Error("a missing table listed as no pending targets")
	}
	if _, _, err := s.Find(context.Background(), "key-1"); err == nil {
		t.Error("a missing table read as a target that does not exist")
	}
	if err := s.Upsert(context.Background(), planner.BuildTarget{TargetKey: "k"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}

func TestAPlannedTicketKeepsItsCriteria(t *testing.T) {
	// A pop returns an id and a title. The criteria are what the product owner
	// validates against, and they exist nowhere else kriya can read.
	s := planner.SQLTickets{DB: sqlDB(t, planner.TicketMigration, planner.TicketKindMigration, planner.TicketSliceMigration)}
	want := planner.Ticket{
		Title: "Create a short link", Body: "the walking skeleton",
		Criteria: []string{"AC-valid-url", "AC-redirect"}, IssueID: "issue-7",
	}
	if err := s.Put(context.Background(), "/target", want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, found, err := s.Find(context.Background(), "/target", "issue-7")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got.Title != want.Title || got.Body != want.Body {
		t.Errorf("read back %+v", got)
	}
	if len(got.Criteria) != 2 || got.Criteria[0] != "AC-valid-url" {
		t.Errorf("criteria came back %v", got.Criteria)
	}
}

func TestReDecomposingATicketReplacesIt(t *testing.T) {
	s := planner.SQLTickets{DB: sqlDB(t, planner.TicketMigration, planner.TicketKindMigration, planner.TicketSliceMigration)}
	ticket := planner.Ticket{Title: "first", IssueID: "issue-7", Criteria: []string{"AC-a"}}
	if err := s.Put(context.Background(), "/target", ticket); err != nil {
		t.Fatalf("put: %v", err)
	}
	ticket.Title, ticket.Criteria = "second", []string{"AC-b"}
	if err := s.Put(context.Background(), "/target", ticket); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, _, err := s.Find(context.Background(), "/target", "issue-7")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.Title != "second" || got.Criteria[0] != "AC-b" {
		t.Errorf("read back %+v", got)
	}
}

func TestAnIssueNothingPlannedIsNotFound(t *testing.T) {
	s := planner.SQLTickets{DB: sqlDB(t, planner.TicketMigration, planner.TicketKindMigration, planner.TicketSliceMigration)}
	_, found, err := s.Find(context.Background(), "/target", "issue-absent")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("an issue no decomposition produced was found")
	}
}

func TestATicketStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := planner.SQLTickets{DB: sqlDB(t)}
	if _, _, err := s.Find(context.Background(), "/target", "issue-7"); err == nil {
		t.Error("a missing table read as an unplanned issue")
	}
	if err := s.Put(context.Background(), "/target", planner.Ticket{IssueID: "i"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}

func TestATicketFromAnotherTargetIsNotFound(t *testing.T) {
	// A pop is identity-wide: sutra offers whatever is assigned to the
	// popping identity, from any plan. A ticket this target's decomposition
	// did not produce is one whose code lives in another repository, and
	// building it here would work the wrong codebase against the wrong
	// snapshot.
	s := planner.SQLTickets{DB: sqlDB(t, planner.TicketMigration, planner.TicketKindMigration, planner.TicketSliceMigration)}
	mine := planner.Ticket{Title: "Create a short link", IssueID: "issue-7"}
	theirs := planner.Ticket{Title: "Add billing", IssueID: "issue-9"}
	if err := s.Put(context.Background(), "/mine", mine); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Put(context.Background(), "/theirs", theirs); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, found, err := s.Find(context.Background(), "/mine", "issue-9"); err != nil {
		t.Fatalf("find: %v", err)
	} else if found {
		t.Error("another target's ticket was offered to this one")
	}
	got, found, err := s.Find(context.Background(), "/mine", "issue-7")
	if err != nil || !found {
		t.Fatalf("this target's own ticket was not found: %v found=%v", err, found)
	}
	if got.Title != mine.Title {
		t.Errorf("found %+v", got)
	}
}

func TestAPlannedTicketKeepsItsPlanShape(t *testing.T) {
	// Layers and skeleton reached the store as WRITE-ONLY columns once: Put
	// recorded them and neither read path selected them, so every ticket read
	// back had no layers and was never the skeleton. Recovery reasons about
	// plan shape from the store, so it would have recovered a different plan
	// than the one decomposition filed.
	s := planner.SQLTickets{DB: sqlDB(t,
		planner.TicketMigration, planner.TicketKindMigration, planner.TicketSliceMigration)}
	want := planner.Ticket{
		Title: "Create a short link", IssueID: "issue-7", Kind: planner.KindImplementation,
		Criteria: []string{"AC-valid-url"}, Layers: []string{"http", "store", "cli"},
		Skeleton: true,
	}
	if err := s.Put(context.Background(), "/target", want); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, found, err := s.Find(context.Background(), "/target", "issue-7")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if !slices.Equal(got.Layers, want.Layers) {
		t.Errorf("Find read layers back as %v, want %v", got.Layers, want.Layers)
	}
	if !got.Skeleton {
		t.Error("Find read the ticket back as not the walking skeleton")
	}

	// BOTH read paths. One decoding a column the other drops is how these
	// went missing in the first place.
	set, err := s.ForTarget(context.Background(), "/target")
	if err != nil {
		t.Fatalf("for target: %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("ForTarget returned %d tickets", len(set))
	}
	if !slices.Equal(set[0].Layers, want.Layers) {
		t.Errorf("ForTarget read layers back as %v, want %v", set[0].Layers, want.Layers)
	}
	if !set[0].Skeleton {
		t.Error("ForTarget read the ticket back as not the walking skeleton")
	}
}
