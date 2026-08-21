//go:build acceptance

// Package acceptance drives kriya's Gherkin specification.
//
// It sits behind the `acceptance` build tag so that `go test ./...` — which
// IS the project's test gate — stays green while scenarios are still
// pending. Run it with `go test -tags acceptance ./internal/acceptance/`.
// CI compiles it on every run (`go vet -tags acceptance ./...`) so it cannot
// rot unnoticed while hidden from the default gate.
package acceptance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
)

// featuresDir holds the Gherkin that is the executable definition of done.
// It is the spec's own verification/ directory, never a copy.
const featuresDir = "../../verification"

// TestAcceptance runs the suite through m.Run rather than TestMain.
//
// This is deliberate and it differs from sutra, whose harness calls
// godog.TestSuite{}.Run() inside TestMain and exits on its status BEFORE
// m.Run() is reached. The go test timeout watchdog is armed by m.Run, so
// sutra's entire acceptance suite executes outside the runtime that enforces
// -timeout — a hang there hangs forever. Sutra logged that in its
// spec-gaps.md and never changed it. Passing Options.TestingT and running
// from an ordinary Test function keeps the suite under the watchdog.
func TestAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "kriya",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "progress",
			Paths:    []string{featuresDir},
			Strict:   true,
			TestingT: t,
		},
	}
	if got := suite.Run(); got != 0 {
		t.Fatalf("acceptance suite exit=%d; scenarios are undefined until their milestone lands", got)
	}
}

// InitializeScenario registers step definitions. Steps land with the
// milestone that needs them, not up front: several scenarios carry a dozen
// or more steps under one header, and definitions written before the module
// APIs exist would only be rewritten.
func InitializeScenario(_ *godog.ScenarioContext) {}

// featureFiles lists the .feature files on disk.
func featureFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(featuresDir)
	if err != nil {
		t.Fatalf("read %s: %v", featuresDir, err)
	}
	var out []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".feature" {
			out = append(out, filepath.Join(featuresDir, e.Name()))
		}
	}
	return out
}
