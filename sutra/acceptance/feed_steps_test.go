package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cucumber/godog"
)

// feedWorld carries feed-scenario state on top of the shared testState.
type feedWorld struct {
	s *testState
	// operator is the identity every seeded mutation acts as.
	operator string
	// subjects maps display names used in scenarios (SUT-1) to the
	// uuid subjects the feed carries.
	subjects map[string]string
	cursor   string
	lastPage struct {
		Events []struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Subject string `json:"subject"`
		} `json:"events"`
		NextCursor string `json:"next_cursor"`
		Drained    bool   `json:"drained"`
	}
	watermark    string
	drainedFlags []bool
	seeded       []string // subjects of seeded events, in order
}

func (f *feedWorld) reset() {
	f.operator = ""
	f.subjects = map[string]string{}
	f.cursor = ""
	f.watermark = ""
	f.drainedFlags = nil
	f.seeded = nil
}

// ensureOperator creates the acting identity once per scenario.
func (f *feedWorld) ensureOperator() error {
	if f.operator != "" {
		return nil
	}
	if err := f.s.call(http.MethodPost, "/identities", map[string]string{"handle": "operator", "kind": "human"}); err != nil {
		return err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(f.s.lastBody, &created); err != nil {
		return fmt.Errorf("decode identity: %w", err)
	}
	f.operator = created.ID
	return nil
}

// occur produces one real feed event through the public API — a project
// creation, the only event-emitting mutation implemented so far.
func (f *feedWorld) occur(label string) error {
	if err := f.ensureOperator(); err != nil {
		return err
	}
	if err := f.s.call(http.MethodPost, "/projects", map[string]string{
		"key": label, "name": label, "actor": f.operator,
	}); err != nil {
		return err
	}
	if err := f.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(f.s.lastBody, &created); err != nil {
		return fmt.Errorf("decode project: %w", err)
	}
	f.seeded = append(f.seeded, created.ID)
	return nil
}

// seedRaw inserts a feed event directly at the store, for kinds whose
// emitting endpoints are not built yet. The events table is the real
// production sink, so shape and ordering are exercised faithfully.
func (f *feedWorld) seedRaw(kind, displaySubject string) error {
	if err := f.ensureOperator(); err != nil {
		return err
	}
	subject, ok := f.subjects[displaySubject]
	if !ok {
		subject = fmt.Sprintf("00000000-0000-7000-8000-%012d", len(f.subjects)+1)
		f.subjects[displaySubject] = subject
	}
	_, err := f.s.db.Exec(`
		INSERT INTO events (id, kind, subject, operation, actor, payload, created)
		VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		fmt.Sprintf("00000000-0000-7000-8000-9%011d", len(f.seeded)+1), kind, subject, subject, f.operator,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("seed event: %w", err)
	}
	f.seeded = append(f.seeded, subject)
	return nil
}

func (f *feedWorld) poll(query string) error {
	if err := f.s.call(http.MethodGet, "/events"+query, nil); err != nil {
		return err
	}
	if err := f.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	f.lastPage.Events = nil
	if err := json.Unmarshal(f.s.lastBody, &f.lastPage); err != nil {
		return fmt.Errorf("decode page: %w", err)
	}
	return nil
}

func registerFeedSteps(sc *godog.ScenarioContext, s *testState) {
	f := &feedWorld{s: s}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		f.reset()
		return ctx, nil
	})

	// --- cursor polling resumes losslessly
	sc.Step(`^events E1, E2, E3 occurred in order$`, func() error {
		for _, label := range []string{"E1", "E2", "E3"} {
			if err := f.occur(label); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^a consumer's cursor sits after E1$`, func() error {
		if err := f.poll("?limit=1"); err != nil {
			return err
		}
		if len(f.lastPage.Events) != 1 || f.lastPage.Events[0].Subject != f.seeded[0] {
			return fmt.Errorf("first page is not E1: %+v", f.lastPage.Events)
		}
		f.cursor = f.lastPage.NextCursor
		return nil
	})
	sc.Step(`^the consumer polls the feed$`, func() error {
		return f.poll("?cursor=" + f.cursor)
	})
	sc.Step(`^it receives E2 and E3 in order$`, func() error {
		if len(f.lastPage.Events) != 2 {
			return fmt.Errorf("expected 2 events, got %d", len(f.lastPage.Events))
		}
		if f.lastPage.Events[0].Subject != f.seeded[1] || f.lastPage.Events[1].Subject != f.seeded[2] {
			return fmt.Errorf("out of order: %+v (seeded %v)", f.lastPage.Events, f.seeded)
		}
		f.cursor = f.lastPage.NextCursor
		return nil
	})
	sc.Step(`^polling again from the new cursor returns nothing$`, func() error {
		if err := f.poll("?cursor=" + f.cursor); err != nil {
			return err
		}
		if len(f.lastPage.Events) != 0 {
			return fmt.Errorf("expected empty page, got %+v", f.lastPage.Events)
		}
		return nil
	})

	// --- filters narrow the feed
	sc.Step(`^events of kinds "([^"]*)" and "([^"]*)" exist for several issues$`, func(kindA, kindB string) error {
		for _, seed := range []struct{ kind, subject string }{
			{kindA, "SUT-1"}, {kindB, "SUT-1"}, {kindA, "SUT-2"}, {kindB, "SUT-3"},
		} {
			if err := f.seedRaw(seed.kind, seed.subject); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^a consumer polls with kind "([^"]*)" and subject SUT-1$`, func(kind string) error {
		return f.poll("?kind=" + kind + "&subject=" + f.subjects["SUT-1"])
	})
	sc.Step(`^it receives only review approvals for SUT-1$`, func() error {
		if len(f.lastPage.Events) != 1 {
			return fmt.Errorf("filter leaked: %+v", f.lastPage.Events)
		}
		e := f.lastPage.Events[0]
		if e.Kind != "review.approved" || e.Subject != f.subjects["SUT-1"] {
			return fmt.Errorf("wrong event passed the filter: %+v", e)
		}
		return nil
	})

	// --- draining to a watermark is bounded and provable
	sc.Step(`^a consumer drains the feed with until set to a captured watermark$`, func() error {
		for _, label := range []string{"D1", "D2"} {
			if err := f.occur(label); err != nil {
				return err
			}
		}
		// The current head position is the captured watermark: a fixed
		// bound taken before the drain begins.
		if err := f.poll("?limit=1000"); err != nil {
			return err
		}
		f.watermark = f.lastPage.NextCursor
		f.cursor = ""
		return nil
	})
	sc.Step(`^new events keep arriving concurrently$`, func() error {
		// Interleaved with the drain below: one page is consumed, then
		// a new event lands past the bound, then the drain continues.
		if err := f.poll("?limit=1&until=" + f.watermark); err != nil {
			return err
		}
		f.drainedFlags = append(f.drainedFlags, f.lastPage.Drained)
		f.cursor = f.lastPage.NextCursor
		return f.occur("CONCURRENT")
	})
	sc.Step(`^each page reports drained false until everything through the fixed watermark has been returned$`, func() error {
		if len(f.drainedFlags) == 0 || f.drainedFlags[0] {
			return fmt.Errorf("first partial page must report drained false: %v", f.drainedFlags)
		}
		return nil
	})
	sc.Step(`^the page that completes the drain reports drained true — later concurrent events do not move the bound$`, func() error {
		for range 100 {
			if err := f.poll("?limit=1&until=" + f.watermark + "&cursor=" + f.cursor); err != nil {
				return err
			}
			f.cursor = f.lastPage.NextCursor
			if f.lastPage.Drained {
				return nil
			}
			if len(f.lastPage.Events) == 0 {
				return fmt.Errorf("feed exhausted without drained=true; the concurrent event moved the bound")
			}
		}
		return fmt.Errorf("drain never completed")
	})
}
