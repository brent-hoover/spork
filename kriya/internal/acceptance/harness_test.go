//go:build acceptance

// Package acceptance drives kriya's Gherkin specification.
//
// It sits behind the `acceptance` build tag so that `go test ./...` — which
// IS the project's test gate — stays green while scenarios are still
// pending. Run it with `go test -tags acceptance ./internal/acceptance/`.
// The test gate runs TestDiscovery under the tag, so the scenario count
// cannot drift while the package is hidden from the default gate.
package acceptance

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
)

// opts is bound to command-line flags so a caller can select scenarios.
//
// godog runs from inside a Go test, so `go test -run` filters TEST FUNCTIONS
// and does nothing to scenarios — a per-milestone command written that way
// would either run every pending scenario or match no test and pass having
// run nothing. Selection must be godog's own:
//
//	go test -tags acceptance ./internal/acceptance/ -godog.paths ../../verification/REQ-spec-intake.feature
var opts = godog.Options{Format: "progress", Strict: true}

// BindFlags, not BindCommandLineFlags: the latter binds to pflag, which a Go
// test binary never parses — testing parses stdlib flag.CommandLine — so the
// flags silently do not exist and every -godog.* argument is rejected.
func init() { godog.BindFlags("godog.", flag.CommandLine, &opts) }

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
	o := opts
	if len(o.Paths) == 0 {
		o.Paths = []string{featuresDir}
	}
	o.TestingT = t
	suite := godog.TestSuite{
		Name:                "kriya",
		ScenarioInitializer: InitializeScenario,
		Options:             &o,
	}
	if got := suite.Run(); got != 0 {
		t.Fatalf("acceptance suite exit=%d; scenarios are undefined until their milestone lands", got)
	}
}

// InitializeScenario registers step definitions.
//
// Steps land with the milestone that needs them, not up front: several
// scenarios carry a dozen or more steps under one header, and definitions
// written before the module APIs exist would only be rewritten.
func InitializeScenario(sc *godog.ScenarioContext) {
	// ONE world per scenario, shared by every registration. Each file having
	// its own Before hook gave each its own world, so a Given in one file set
	// a directory the When in another never saw — and the scenario passed or
	// failed for reasons unrelated to what it asserts.
	w := &world{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = *newWorld()
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.done()
		return ctx, nil
	})
	registerSpecIntake(sc, w)
	registerModuleCommands(sc, w)
	registerTierRouting(sc)
	registerGenerations(sc, w)
	registerDecompose(sc, w)
	registerPairLoop(sc, w)
	registerGates(sc, w)
	registerCoverageAndMutationGates(sc, w)
	registerContextAssembly(sc, w)
	registerWorkspaces(sc, w)
	registerThreadCapture(sc, w)
	registerArchitect(sc, w)
	registerOwner(sc, w)
	registerSubmit(sc, w)
	registerResubmit(sc, w)
	registerMerge(sc, w)
	registerFences(sc, w)
	registerComplete(sc, w)
	registerMergeRefusals(sc, w)
	registerAttemptScoping(sc, w)
	registerPopLoop(sc, w)
	registerRework(sc, w)
	registerSerialization(sc, w)
	registerLearningLoop(sc, w)
	registerCompletionDetection(sc, w)
	registerEpoch(sc, w)
	registerClaim(sc, w)
}

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
