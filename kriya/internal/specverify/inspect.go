package specverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
)

// RequiredCommands are the gates kriya runs per module. `install` is
// deliberately absent: AC-intake-commands names these six and only these.
var RequiredCommands = []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"}

// Module is one module's resolved effective commands.
//
// Commands maps a command name to its resolved value. An absent or empty
// value means the command is not declared anywhere in the module's effective
// stack — avspec rejects blank commands, so empty is unambiguous.
type Module struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Commands map[string]string `json:"commands"`
}

// Missing lists the required commands this module does not resolve.
func (m Module) Missing() []string {
	var out []string
	for _, name := range RequiredCommands {
		if m.Commands[name] == "" {
			out = append(out, name)
		}
	}
	return out
}

// Model is `avspec inspect` output: the spec as a build engine needs it.
type Model struct {
	OK      bool `json:"ok"`
	Project struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"project"`
	Commands map[string]string `json:"commands"`
	Modules  []Module          `json:"modules"`
	// Artifacts is every file the spec references, relative to its directory:
	// the manifest, each acceptance criterion's feature file, and each
	// module's contracts. This is the content set a SpecSnapshot must pin.
	Artifacts []string `json:"artifacts"`
}

// Inspect runs `avspec inspect <dir>` and parses the build model.
//
// Same exit contract as Verify: 0 or 1 with a parseable payload is a result,
// anything else is an error.
func (c CLI) Inspect(ctx context.Context, dir string) (Model, error) {
	out, err := c.run(ctx, "inspect", dir)
	if err != nil {
		return Model{}, err
	}
	return parseModel(out)
}

// rawModel mirrors the payload with pointers on every field a SUCCESSFUL
// resolve must state, so a payload missing one is rejected rather than
// silently zero-valued.
//
// Deliberately NOT an embedded Model: encoding/json binds a name to the
// SHALLOWEST field, so an outer Modules would capture "modules" and leave the
// embedded copy empty — which is exactly the fail-open this validation was
// added to prevent, and it slipped in on the first attempt.
type rawModel struct {
	OK        *bool             `json:"ok"`
	Project   json.RawMessage   `json:"project"`
	Commands  map[string]string `json:"commands"`
	Modules   *[]Module         `json:"modules"`
	Artifacts *[]string         `json:"artifacts"`
}

func parseModel(out []byte) (Model, error) {
	var raw rawModel
	if err := json.Unmarshal(out, &raw); err != nil {
		return Model{}, fmt.Errorf("specverify: parse model: %w", err)
	}
	if raw.OK == nil {
		return Model{}, errors.New("specverify: model has no ok")
	}
	m := Model{OK: *raw.OK, Commands: raw.Commands}
	if raw.Artifacts != nil {
		m.Artifacts = *raw.Artifacts
	}
	if !m.OK {
		// ok:false is avspec reporting it could not load the manifest.
		// Demanding the rest would turn a legitimate refusal into an error.
		return m, nil
	}
	if err := raw.requireSuccessFields(); err != nil {
		return Model{}, err
	}
	m.Modules = *raw.Modules
	if err := json.Unmarshal(raw.Project, &m.Project); err != nil {
		return Model{}, fmt.Errorf("specverify: parse model project: %w", err)
	}
	return m, nil
}

// requireSuccessFields checks everything a successful model must carry.
func (r rawModel) requireSuccessFields() error {
	switch {
	case r.Modules == nil:
		// Even an empty list is an answer; absent is not.
		return errors.New("specverify: successful model has no modules")
	case len(r.Project) == 0:
		return errors.New("specverify: successful model has no project")
	case r.Commands == nil:
		return errors.New("specverify: successful model has no commands")
	case r.Artifacts == nil:
		// Artifacts drives the snapshot's content set. Absent would pin a
		// "full artifact set" containing nothing, not even the manifest.
		return errors.New("specverify: successful model has no artifacts")
	}
	seen := map[string]bool{}
	for i, mod := range *r.Modules {
		// A module with six commands but no identity passes the command check
		// and is then unaddressable: gate results pin to a module id, and a
		// refusal has to name one.
		if mod.ID == "" || mod.Name == "" {
			return fmt.Errorf("specverify: module %d has no id or name", i)
		}
		// Duplicates must be rejected, not deduplicated. Commands are pinned
		// into a map keyed by id, so a later duplicate silently overwrites an
		// earlier module and the snapshot no longer holds every module's
		// validated commands — while intake, which walks the list, validated
		// both.
		if seen[mod.ID] {
			return fmt.Errorf("specverify: module id %q appears more than once", mod.ID)
		}
		seen[mod.ID] = true
	}
	return nil
}

// run executes an avspec subcommand and returns its stdout.
func (c CLI) run(ctx context.Context, sub, dir string, extra ...string) ([]byte, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("specverify: no command configured")
	}
	args := append(append([]string{}, c.Argv[1:]...), sub, dir)
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, c.Argv[0], args...)
	cmd.Dir = c.WorkDir

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return nil, fmt.Errorf("specverify: run %s: %w", c.Argv[0], err)
	}
	if code := ee.ExitCode(); code != 1 {
		return nil, fmt.Errorf("specverify: %s %s exited %d: %s", c.Argv[0], sub, code, ee.Stderr)
	}
	return out, nil
}
