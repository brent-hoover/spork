//go:build acceptance

package acceptance

import (
	"os"
	"testing"

	gherkin "github.com/cucumber/gherkin/go/v26"
	messages "github.com/cucumber/messages/go/v21"
)

// wantScenarioRuns is the definition of done for the whole build.
//
// The 19 feature files carry 155 Scenario/Scenario Outline headers, but three
// Outlines expand — REQ-run-to-complete.feature (2 rows and 4 rows) and
// REQ-submit-review.feature (4 rows) — so a runner sees 162. Counting headers
// instead would understate the target by seven and let seven behaviours ship
// unproven.
const (
	wantScenarioRuns = 162
	wantFeatureFiles = 19
)

// TestDiscovery asserts the suite finds every scenario, and is deliberately
// separate from whether they PASS. Discovery must hold from M1; passing is
// per-milestone and stays red until M6. Collapsing the two would make the
// count unobservable exactly when it is most likely to drift.
func TestDiscovery(t *testing.T) {
	files := featureFiles(t)
	if len(files) != wantFeatureFiles {
		t.Errorf("found %d feature files, want %d", len(files), wantFeatureFiles)
	}

	newID := (&messages.Incrementing{}).NewId
	total := 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatalf("open %s: %v", f, err)
		}
		doc, err := gherkin.ParseGherkinDocument(fh, newID)
		_ = fh.Close()
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		// Pickles are the expanded, runnable scenarios — one per Outline row.
		total += len(gherkin.Pickles(*doc, f, newID))
	}
	if total != wantScenarioRuns {
		t.Errorf("discovered %d scenario runs, want %d", total, wantScenarioRuns)
	}
}
