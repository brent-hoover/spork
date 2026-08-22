package devloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kriya/internal/devloop"
	"kriya/internal/fakes"
)

type memStore struct{ rows []devloop.Session }

func (m *memStore) Upsert(_ context.Context, s devloop.Session) error {
	for i, existing := range m.rows {
		if existing.Run == s.Run {
			m.rows[i] = s
			return nil
		}
	}
	m.rows = append(m.rows, s)
	return nil
}

func (m *memStore) only(t *testing.T) devloop.Session {
	t.Helper()
	if len(m.rows) != 1 {
		t.Fatalf("expected one session, got %d", len(m.rows))
	}
	return m.rows[0]
}

func request() devloop.Request {
	return devloop.Request{
		Run: "run-1", Ticket: "KRI-1", Title: "Create a short link",
		Criteria:   []string{"AC-valid-url"},
		Workspace:  "/wt/kri-1",
		Commands:   map[string]string{"test": "go test ./...", "mutation": "gremlins"},
		AllowRules: []string{"Bash(/wt/kri-1/.kriya/gate-test *)"},
	}
}

func TestASessionIsRecordedBeforeTheAgentRuns(t *testing.T) {
	// AC-thread-no-loss: every outcome reaches the thread catalog, crashed
	// included. A crash mid-session must leave a row recovery can terminate
	// and import from, so the row exists before the agent is invoked.
	store := &memStore{}
	ag := &fakes.Agent{Err: errors.New("agent died")}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.Work(context.Background(), request()); err == nil {
		t.Fatal("expected the agent failure to surface")
	}
	row := store.only(t)
	if row.Ticket != "KRI-1" {
		t.Errorf("no session row survived the failure: %+v", row)
	}
	if row.Ended {
		t.Error("a crashed session is not ended")
	}
}

func TestACompletedSessionRecordsItsIdentity(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply()}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	got, err := loop.Work(context.Background(), request())
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if got.SessionID == "" {
		t.Error("no session id, so the transcript cannot be found from the ticket")
	}
	if got.Model == "" {
		t.Error("no model recorded")
	}
	if !store.only(t).Ended {
		t.Error("the stored session is not stamped ended")
	}
}

func TestTheAgentIsToldWhatWillJudgeIt(t *testing.T) {
	// The gate commands travel as CONTENT so the agent knows the bar; the
	// permission to run them is granted separately, and the prompt must not
	// be the only place either appears.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply()}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	req := ag.Requests[0]
	for _, want := range []string{"KRI-1", "AC-valid-url", "go test ./...", "test first"} {
		if !strings.Contains(req.Prompt, want) && !strings.Contains(req.Prompt, "Create a short link") {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	if req.Workspace != "/wt/kri-1" {
		t.Errorf("the agent was pointed at %q", req.Workspace)
	}
	if len(req.AllowRules) == 0 {
		t.Error("no permission rules were passed, so the agent gets defaults")
	}
	if req.Role != "dev" {
		t.Errorf("role is %q, want dev", req.Role)
	}
}
