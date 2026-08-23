package owner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kriya/internal/agent"
	"kriya/internal/fakes"
	"kriya/internal/owner"
)

type memStore struct {
	rows map[string]owner.Validation
	err  error
}

func newMemStore() *memStore { return &memStore{rows: map[string]owner.Validation{}} }

func key(build string, attempt int) string {
	return build + ":" + string(rune('0'+attempt))
}

func (m *memStore) Upsert(_ context.Context, v owner.Validation) error {
	if m.err != nil {
		return m.err
	}
	m.rows[key(v.Build, v.Attempt)] = v
	return nil
}

func (m *memStore) Find(_ context.Context, build string, attempt int) (owner.Validation, bool, error) {
	if m.err != nil {
		return owner.Validation{}, false, m.err
	}
	v, ok := m.rows[key(build, attempt)]
	return v, ok, nil
}

// gateResults answers the precondition question.
type gateResults struct {
	missing string
	err     error
	asked   []string
}

func (g *gateResults) AllPassed(_ context.Context, _, _, commit string) (bool, string, error) {
	g.asked = append(g.asked, commit)
	if g.err != nil {
		return false, "", g.err
	}
	if g.missing != "" {
		return false, g.missing, nil
	}
	return true, "", nil
}

func poReply(verdict, notes string) []agent.Result {
	body, err := json.Marshal(map[string]string{"verdict": verdict, "notes": notes})
	if err != nil {
		panic(err)
	}
	return []agent.Result{{SessionID: "po-1", Model: "test-model", Structured: body}}
}

func validator(store *memStore, gates *gateResults, ag *fakes.Agent) owner.Owner {
	return owner.Owner{
		Agent: ag, Store: store, Gates: gates, Now: fakes.NewClock(time.Unix(0, 0)),
	}
}

func request() owner.Request {
	return owner.Request{
		Build: "run-1", Module: "MOD-a", Ticket: "KRI-1", Commit: "sha-a", Attempt: 1,
		Criteria: []string{"AC-one", "AC-two", "AC-three"},
	}
}

func TestValidationRefusesUntilEveryGateHasPassed(t *testing.T) {
	// AC-po-position: it cannot be skipped OR REORDERED around the gap, so the
	// precondition is checked here rather than assumed of the caller.
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	gates := &gateResults{missing: "mutation"}
	_, err := validator(store, gates, ag).Validate(context.Background(), request())
	if err == nil {
		t.Fatal("validation ran with a gate outstanding")
	}
	if !strings.Contains(err.Error(), "mutation") {
		t.Errorf("got %v, want the missing gate named", err)
	}
	if len(ag.Requests) != 0 {
		t.Error("the product owner was invoked for ungated code")
	}
	if len(store.rows) != 0 {
		t.Error("a verdict was recorded for ungated code")
	}
}

func TestThePreconditionIsCheckedAtTheHeadCommit(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	gates := &gateResults{}
	if _, err := validator(store, gates, ag).Validate(context.Background(), request()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(gates.asked) != 1 || gates.asked[0] != "sha-a" {
		t.Errorf("the gates were asked about %v", gates.asked)
	}
}

func TestAPassingVerdictIsRecordedWithItsReasons(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "each AC has a real test")}
	got, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !got.Passed() {
		t.Errorf("verdict is %q", got.Verdict)
	}
	saved := store.rows[key("run-1", 1)]
	if saved.Notes != "each AC has a real test" || saved.Commit != "sha-a" {
		t.Errorf("recorded %+v", saved)
	}
}

func TestAGamingVerdictDoesNotPass(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictTestsGamed,
		"assert_true(True) in test_valid_url")}
	got, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.Passed() {
		t.Error("a tests-gamed verdict passed")
	}
	if !strings.Contains(store.rows[key("run-1", 1)].Notes, "assert_true") {
		t.Error("the gaming evidence was not recorded")
	}
}

func TestAVerdictWithNoReasonsIsRefused(t *testing.T) {
	// A verdict with no reasons is not actionable, and AC-po-verdict wants the
	// reasons to survive alongside it.
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictNotSatisfied, "")}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err == nil {
		t.Fatal("a verdict carrying no reasons was accepted")
	}
	if len(store.rows) != 0 {
		t.Error("an unreasoned verdict was recorded")
	}
}

func TestTheProductOwnerIsToldWhatCountsAsGaming(t *testing.T) {
	// Named explicitly rather than left to judgement: a PO asked only "is this
	// good" reliably answers yes to a tautology.
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	prompt := ag.Requests[0].Prompt
	for _, want := range []string{
		"tautolog", "special-cases test inputs", "weakened or deleted",
		"AC-one", "AC-two", "AC-three", "INTENT",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt omits %q:\n%s", want, prompt)
		}
	}
}

func TestTheProductOwnerJudgesAndDoesNotFix(t *testing.T) {
	// An implementation edited by its own validator is one nothing independent
	// has looked at.
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rules := strings.Join(ag.Requests[0].AllowRules, ",")
	if rules == "" {
		t.Fatal("no rules were passed, so the agent gets defaults")
	}
	for _, mutating := range []string{"Write", "Edit", "Bash", "MultiEdit"} {
		if strings.Contains(rules, mutating) {
			t.Errorf("the product owner was granted %s", mutating)
		}
	}
	if ag.Requests[0].Role != agent.RolePO {
		t.Errorf("ran as %q", ag.Requests[0].Role)
	}
}

func TestTheVerdictIsSchemaConstrained(t *testing.T) {
	// A free-text verdict would have to be parsed, and a parse that guessed
	// wrong would record "satisfied" for a run the PO rejected.
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	schema := string(ag.Requests[0].Schema)
	for _, want := range []string{"satisfied", "not-satisfied", "tests-gamed", "minLength"} {
		if !strings.Contains(schema, want) {
			t.Errorf("the schema omits %q", want)
		}
	}
}

func TestTheAllowRulesCannotBeWidenedByACaller(t *testing.T) {
	first := owner.AllowRules()
	first[0] = "Bash"
	if owner.AllowRules()[0] == "Bash" {
		t.Error("a caller widened the product owner's toolset")
	}
}

func TestAnUnreadableReplyIsAFailure(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: []agent.Result{{
		SessionID: "po-1", Structured: json.RawMessage("not json"),
	}}}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err == nil {
		t.Fatal("a reply that is not a verdict was accepted")
	}
}

func TestAnAgentFailureSurfaces(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Err: errors.New("agent died")}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err == nil {
		t.Fatal("an agent that never answered read as a verdict")
	}
}

func TestAnUnreadableGateResultStopsValidation(t *testing.T) {
	// "I could not read the gates" is not "the gates passed".
	store := newMemStore()
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	gates := &gateResults{err: errors.New("store unavailable")}
	if _, err := validator(store, gates, ag).Validate(context.Background(), request()); err == nil {
		t.Fatal("unreadable gate results were treated as passes")
	}
}

func TestAnUnrecordableVerdictIsAFailure(t *testing.T) {
	store := newMemStore()
	store.err = errors.New("disk full")
	ag := &fakes.Agent{Replies: poReply(owner.VerdictSatisfied, "fine")}
	if _, err := validator(store, &gateResults{}, ag).Validate(context.Background(), request()); err == nil {
		t.Fatal("a verdict that could not be recorded read as recorded")
	}
}
