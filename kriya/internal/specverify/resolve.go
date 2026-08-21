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

// Model is `avspec resolve` output: the spec as a build engine needs it.
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

// Resolve runs `avspec resolve <dir>` and parses the build model.
//
// Same exit contract as Verify: 0 or 1 with a parseable payload is a result,
// anything else is an error.
func (c CLI) Resolve(ctx context.Context, dir string) (Model, error) {
	out, err := c.run(ctx, "resolve", dir)
	if err != nil {
		return Model{}, err
	}
	var m Model
	if err := json.Unmarshal(out, &m); err != nil {
		return Model{}, fmt.Errorf("specverify: parse model: %w", err)
	}
	return m, nil
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
