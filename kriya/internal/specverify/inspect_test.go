package specverify_test

import (
	"context"
	"strings"
	"testing"
)

// inspect drives the real exec path with a stubbed avspec, which is what makes
// these tests about the contract rather than about a hand-built struct.
func inspect(t *testing.T, body string) error {
	t.Helper()
	_, err := stub(t, body, 0).Inspect(context.Background(), t.TempDir())
	return err
}

func TestAModelWithNoOKIsRejected(t *testing.T) {
	if err := inspect(t, `{"modules":[]}`); err == nil {
		t.Fatal("a model that never says whether it succeeded was accepted")
	}
}

func TestARefusalNeedsNothingElse(t *testing.T) {
	// ok:false is avspec reporting it could not load the manifest. Demanding
	// the rest would turn a legitimate refusal into an error.
	if err := inspect(t, `{"ok":false}`); err != nil {
		t.Fatalf("a refusal was rejected: %v", err)
	}
}

func TestASuccessfulModelMustCarryEveryPart(t *testing.T) {
	full := map[string]string{
		"modules":   `"modules":[{"id":"MOD-a","name":"a"}]`,
		"project":   `"project":{"language":"go"}`,
		"commands":  `"commands":{"test":"go test ./..."}`,
		"artifacts": `"artifacts":["avspec.yaml"]`,
	}
	for missing := range full {
		parts := []string{`"ok":true`}
		for name, fragment := range full {
			if name != missing {
				parts = append(parts, fragment)
			}
		}
		err := inspect(t, "{"+strings.Join(parts, ",")+"}")
		if err == nil {
			t.Errorf("a successful model with no %s was accepted", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the error for a missing %s says %q", missing, err)
		}
	}
}

func TestAnEmptyListIsAnAnswerButAbsentIsNot(t *testing.T) {
	err := inspect(t, `{"ok":true,"modules":[],"project":{},"commands":{},"artifacts":[]}`)
	if err != nil {
		t.Fatalf("an empty but present model was rejected: %v", err)
	}
}

func TestAModuleWithNoIdentityIsUnaddressable(t *testing.T) {
	// Gate results pin to a module id, and a refusal has to name one.
	for _, mod := range []string{`{"name":"a"}`, `{"id":"MOD-a"}`} {
		body := `{"ok":true,"modules":[` + mod + `],"project":{},"commands":{},"artifacts":[]}`
		if err := inspect(t, body); err == nil {
			t.Errorf("module %s was accepted", mod)
		}
	}
}

func TestDuplicateModuleIDsAreRejectedNotDeduplicated(t *testing.T) {
	// Commands are pinned into a map keyed by id, so a later duplicate would
	// silently overwrite an earlier module while intake, walking the list,
	// validated both.
	body := `{"ok":true,"modules":[{"id":"MOD-a","name":"a"},{"id":"MOD-a","name":"b"}],` +
		`"project":{},"commands":{},"artifacts":[]}`
	err := inspect(t, body)
	if err == nil {
		t.Fatal("a duplicate module id was accepted")
	}
	if !strings.Contains(err.Error(), "MOD-a") {
		t.Errorf("the error %q does not name the duplicate", err)
	}
}

func TestAnUnparseableProjectIsAnError(t *testing.T) {
	body := `{"ok":true,"modules":[],"project":"not-an-object","commands":{},"artifacts":[]}`
	if err := inspect(t, body); err == nil {
		t.Fatal("a project that is not an object was accepted")
	}
}

func TestOutputThatIsNotAModelIsAnError(t *testing.T) {
	if err := inspect(t, "not json"); err == nil {
		t.Fatal("output that is not a model was accepted")
	}
}
