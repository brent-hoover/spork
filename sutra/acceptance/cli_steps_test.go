package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/cucumber/godog"
	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"

	"sutra/internal/cli"
)

// cliWorld drives the CLI scenarios in-process — cli.Run against the
// scenario's API server, the handler-level parallel of the web
// decision.
type cliWorld struct {
	iw *issueWorld
	cw *closeWorld

	workdir    string
	tempDirs   []string
	lastOut    string
	lastErrOut string
	lastCode   int
	jsonOut    string
	yamlOut    string
	remoteOut  string
	remote     *importTarget
	// remotePairs holds one (command, local, remote) triple per command
	// FAMILY exercised against --server, so parity covers more than a
	// single read path (review 1924).
	remotePairs []remotePair
}

type remotePair struct{ command, local, remote string }

func (clw *cliWorld) reset() {
	for _, dir := range clw.tempDirs {
		_ = os.RemoveAll(dir)
	}
	if clw.remote != nil {
		clw.remote.close()
	}
	*clw = cliWorld{iw: clw.iw, cw: clw.cw}
}

// run invokes the CLI with the scenario server as SUTRA_SERVER and the
// operator as SUTRA_ACTOR, splitting a shell-ish command line.
func (clw *cliWorld) run(command string, extraEnv map[string]string) error {
	args := strings.Fields(command)
	if len(args) > 0 && args[0] == "sutra" {
		args = args[1:]
	}
	var out, errOut bytes.Buffer
	env := map[string]string{
		"SUTRA_SERVER": clw.iw.s.server.URL,
		"SUTRA_ACTOR":  clw.iw.identities["operator"],
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	workdir := clw.workdir
	if workdir == "" {
		workdir = os.TempDir()
	}
	clw.lastCode = cli.Run(cli.Env{
		Args:    args,
		Stdout:  &out,
		Stderr:  &errOut,
		Workdir: workdir,
		Getenv:  func(k string) string { return env[k] },
	})
	clw.lastOut = out.String()
	clw.lastErrOut = errOut.String()
	return nil
}

func (clw *cliWorld) expectSuccess() error {
	if clw.lastCode != 0 {
		return fmt.Errorf("cli exited %d: %s", clw.lastCode, clw.lastErrOut)
	}
	return nil
}

func registerCLISteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	clw := &cliWorld{iw: iw, cw: cw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		clw.reset()
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		clw.reset()
		return ctx, nil
	})

	// --- every api operation has a command
	sc.Step(`^the API contract's list of operations$`, func() error {
		return nil // loaded in the check step
	})
	sc.Step(`^the parity check maps operations to CLI commands$`, func() error {
		return nil // performed in the assertion step
	})
	sc.Step(`^every operation is covered by at least one command$`, func() error {
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile("../contracts/sutra.openapi.yaml")
		if err != nil {
			return err
		}
		return checkParity(doc, cli.OperationMapping())
	})
	sc.Step(`^the check fails if a new operation lacks one$`, func() error {
		// Prove the CHECKER, not just the absence of a sentinel: give it
		// a contract carrying an operation the CLI does not map, and a
		// mapping pointing at the wrong request, and require it to
		// report both. A checker that always succeeded would fail here
		// (review 1926).
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile("../contracts/sutra.openapi.yaml")
		if err != nil {
			return err
		}
		if err := checkParity(doc, cli.OperationMapping()); err != nil {
			return fmt.Errorf("the real contract should pass: %w", err)
		}

		synthetic := &openapi3.T{Paths: &openapi3.Paths{}}
		for path, item := range doc.Paths.Map() {
			synthetic.Paths.Set(path, item)
		}
		synthetic.Paths.Set("/synthetic-operation", &openapi3.PathItem{
			Get: &openapi3.Operation{OperationID: "syntheticUncoveredOperation"},
		})
		err = checkParity(synthetic, cli.OperationMapping())
		if err == nil {
			return fmt.Errorf("parity check accepted an operation with no CLI command")
		}
		if !strings.Contains(err.Error(), "syntheticUncoveredOperation") {
			return fmt.Errorf("parity failure does not name the uncovered operation: %v", err)
		}

		// The same for a mapping pointing at the wrong request.
		wrong := cli.OperationMapping()
		wrong["listProjects"] = cli.Mapping{Method: http.MethodDelete, Path: "/nowhere"}
		err = checkParity(doc, wrong)
		if err == nil {
			return fmt.Errorf("parity check accepted a command mapped to the wrong request")
		}
		if !strings.Contains(err.Error(), "listProjects") {
			return fmt.Errorf("parity failure does not name the mismapped operation: %v", err)
		}
		return nil
	})

	// --- remote server behaves like loopback
	sc.Step(`^a sutra server running on another host$`, func() error {
		// Seed the loopback server, then reconstruct the identical
		// state (uuids preserved) on a second server via export/import.
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/export", nil); err != nil {
			return err
		}
		exported := append([]byte{}, iw.s.lastBody...)
		remote, err := newImportTarget()
		if err != nil {
			return err
		}
		clw.remote = remote
		req, err := http.NewRequest(http.MethodPost, remote.server.URL+"/projects/import?actor="+iw.identities["operator"], bytes.NewReader(exported))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", newIdempotencyKey())
		resp, err := remote.server.Client().Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("seed remote: %d %s", resp.StatusCode, body)
		}
		return nil
	})
	sc.Step(`^each CLI command runs with --server pointing at it$`, func() error {
		// One representative per command family — the issue commands
		// and the generic api command — so a family that silently
		// ignored --server could not pass (review 1924).
		commands := []string{
			"sutra issue show SUT-1 --json",
			"sutra issue list --project SUT --json",
			"sutra api listIdentities --json",
			"sutra api listProjects --json",
		}
		clw.remotePairs = nil
		for _, command := range commands {
			if err := clw.run(command, nil); err != nil {
				return err
			}
			if err := clw.expectSuccess(); err != nil {
				return fmt.Errorf("local %q: %w", command, err)
			}
			local := clw.lastOut
			if err := clw.run(command+" --server "+clw.remote.server.URL, nil); err != nil {
				return err
			}
			if err := clw.expectSuccess(); err != nil {
				return fmt.Errorf("remote %q: %w", command, err)
			}
			clw.remotePairs = append(clw.remotePairs, remotePair{command: command, local: local, remote: clw.lastOut})
		}
		// The last pair stays in the single-result fields the other
		// steps read.
		last := clw.remotePairs[len(clw.remotePairs)-1]
		clw.lastOut, clw.remoteOut = last.local, last.remote

		// init is a MUTATION, so parity is not "same output" — it is
		// that --server decides WHERE the project lands. A regression
		// making init ignore the flag would otherwise go unseen
		// (review 1928).
		initIn := func(dir, key, extra string) (string, error) {
			saved := clw.workdir
			clw.workdir = dir
			defer func() { clw.workdir = saved }()
			if err := clw.run("sutra init --key "+key+" --name "+key+extra, nil); err != nil {
				return "", err
			}
			if err := clw.expectSuccess(); err != nil {
				return "", fmt.Errorf("init in %s: %w", dir, err)
			}
			// The marker links the directory to the project it created.
			raw, err := os.ReadFile(filepath.Join(dir, ".sutra"))
			if err != nil {
				return "", err
			}
			var marker struct {
				Project string `json:"project"`
			}
			if err := json.Unmarshal(raw, &marker); err != nil {
				return "", err
			}
			return marker.Project, nil
		}
		newDir := func() (string, error) {
			dir, err := os.MkdirTemp("", "sutra-parity-*")
			if err != nil {
				return "", err
			}
			clw.tempDirs = append(clw.tempDirs, dir)
			return dir, nil
		}
		localDir, err := newDir()
		if err != nil {
			return err
		}
		remoteDir, err := newDir()
		if err != nil {
			return err
		}
		localProject, err := initIn(localDir, "PARLOC", "")
		if err != nil {
			return err
		}
		remoteProject, err := initIn(remoteDir, "PARREM", " --server "+clw.remote.server.URL)
		if err != nil {
			return err
		}
		known := func(base, project string) (bool, error) {
			resp, err := http.Get(base + "/projects/" + project)
			if err != nil {
				return false, err
			}
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode == http.StatusOK, nil
		}
		for _, want := range []struct {
			base, label, project string
			present              bool
		}{
			{clw.iw.s.server.URL, "local", localProject, true},
			{clw.remote.server.URL, "remote", localProject, false},
			{clw.remote.server.URL, "remote", remoteProject, true},
			{clw.iw.s.server.URL, "local", remoteProject, false},
		} {
			got, err := known(want.base, want.project)
			if err != nil {
				return err
			}
			if got != want.present {
				return fmt.Errorf("project %s on the %s server: present=%v, want %v — init did not honor --server",
					want.project, want.label, got, want.present)
			}
		}
		return nil
	})
	sc.Step(`^results are identical to running against the local daemon$`, func() error {
		// Byte-identical modulo the feed watermark, which reflects each
		// server's event count (the import audit event shifts it).
		// Outputs are objects or arrays depending on the command, so
		// decode generically and strip the watermark when there is one.
		normalize := func(s string) (any, error) {
			var v any
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				return nil, err
			}
			if m, ok := v.(map[string]any); ok {
				delete(m, "feed_watermark")
			}
			return v, nil
		}
		for _, pair := range clw.remotePairs {
			local, err := normalize(pair.local)
			if err != nil {
				return fmt.Errorf("%s: local output: %w", pair.command, err)
			}
			remote, err := normalize(pair.remote)
			if err != nil {
				return fmt.Errorf("%s: remote output: %w", pair.command, err)
			}
			if !reflect.DeepEqual(local, remote) {
				return fmt.Errorf("remote diverged for %q:\nlocal:  %s\nremote: %s",
					pair.command, pair.local, pair.remote)
			}
		}
		return nil
	})

	// --- json output is complete and valid
	sc.Step(`^issue (SUT-\d+) exists with title, status, assignee, and labels$`, func(issueName string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		assignee, err := iw.identity("claude")
		if err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/assign",
			map[string]any{"assignee": assignee, "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": "cli-label"}); err != nil {
			return err
		}
		var label struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &label); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/labels",
			map[string]string{"label": label.ID, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^"sutra issue show (SUT-\d+) --json" is run$`, func(ref string) error {
		if err := clw.run("sutra issue show "+ref+" --json", nil); err != nil {
			return err
		}
		if err := clw.expectSuccess(); err != nil {
			return err
		}
		clw.jsonOut = clw.lastOut
		return nil
	})
	sc.Step(`^the output parses as JSON$`, func() error {
		var v map[string]any
		return json.Unmarshal([]byte(clw.jsonOut), &v)
	})
	sc.Step(`^it contains every field of (SUT-\d+)$`, func(issueName string) error {
		var got map[string]any
		if err := json.Unmarshal([]byte(clw.jsonOut), &got); err != nil {
			return err
		}
		for _, field := range []string{"id", "number", "title", "status", "project", "assignee", "labels", "created", "updated", "subtree_revision"} {
			if _, ok := got[field]; !ok {
				return fmt.Errorf("output missing field %q: %s", field, clw.jsonOut)
			}
		}
		if got["id"] != iw.issues[issueName] {
			return fmt.Errorf("output shows the wrong issue")
		}
		return nil
	})

	// --- yaml output matches json content
	sc.Step(`^"sutra issue show (SUT-\d+) --yaml" is run$`, func(ref string) error {
		if err := clw.run("sutra issue show "+ref+" --yaml", nil); err != nil {
			return err
		}
		if err := clw.expectSuccess(); err != nil {
			return err
		}
		clw.yamlOut = clw.lastOut
		// The comparison step needs the JSON form too.
		if err := clw.run("sutra issue show "+ref+" --json", nil); err != nil {
			return err
		}
		if err := clw.expectSuccess(); err != nil {
			return err
		}
		clw.jsonOut = clw.lastOut
		return nil
	})
	sc.Step(`^the output parses as YAML$`, func() error {
		var v map[string]any
		return yaml.Unmarshal([]byte(clw.yamlOut), &v)
	})
	sc.Step(`^its content equals the --json output for (SUT-\d+)$`, func(string) error {
		var fromYAML, fromJSON map[string]any
		if err := yaml.Unmarshal([]byte(clw.yamlOut), &fromYAML); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(clw.jsonOut), &fromJSON); err != nil {
			return err
		}
		// YAML decodes integers as int; normalize both through JSON.
		norm := func(v map[string]any) (map[string]any, error) {
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			var out map[string]any
			return out, json.Unmarshal(raw, &out)
		}
		a, err := norm(fromYAML)
		if err != nil {
			return err
		}
		b, err := norm(fromJSON)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(a, b) {
			return fmt.Errorf("yaml diverged from json:\nyaml: %s\njson: %s", clw.yamlOut, clw.jsonOut)
		}
		return nil
	})

	// --- output shape matches the api contract
	sc.Step(`^the API contract defines the issue schema$`, func() error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		_, err := iw.ensureIssue("SUT-1")
		return err
	})
	sc.Step(`^the output validates against the contract's issue schema$`, func() error {
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile("../contracts/sutra.openapi.yaml")
		if err != nil {
			return err
		}
		schema := doc.Components.Schemas["Issue"]
		if schema == nil {
			return fmt.Errorf("contract carries no Issue schema")
		}
		var v any
		if err := json.Unmarshal([]byte(clw.jsonOut), &v); err != nil {
			return err
		}
		return schema.Value.VisitJSON(v)
	})

	// --- init anchors a project to a repo
	sc.Step(`^a repository directory with no sutra project$`, func() error {
		dir, err := os.MkdirTemp("", "sutra-cli-*")
		if err != nil {
			return err
		}
		clw.tempDirs = append(clw.tempDirs, dir)
		clw.workdir = dir
		_, err = iw.identity("operator")
		return err
	})
	sc.Step(`^"sutra init --key SUT --name Sutra" is run there$`, func() error {
		if err := clw.run("sutra init --key SUT --name Sutra", nil); err != nil {
			return err
		}
		return clw.expectSuccess()
	})
	sc.Step(`^project "SUT" exists with the repo's path recorded$`, func() error {
		if err := iw.s.call(http.MethodGet, "/projects", nil); err != nil {
			return err
		}
		var list []struct {
			ID       string  `json:"id"`
			Key      string  `json:"key"`
			RepoPath *string `json:"repo_path"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &list); err != nil {
			return err
		}
		for _, p := range list {
			if p.Key == "SUT" {
				if p.RepoPath == nil || *p.RepoPath != clw.workdir {
					return fmt.Errorf("repo path %v, want %s", p.RepoPath, clw.workdir)
				}
				iw.project = p.ID
				return nil
			}
		}
		return fmt.Errorf("project SUT missing")
	})
	sc.Step(`^a local marker file links the directory to "SUT"$`, func() error {
		raw, err := os.ReadFile(clw.workdir + "/.sutra")
		if err != nil {
			return err
		}
		var m struct {
			Project string `json:"project"`
			Key     string `json:"key"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if m.Key != "SUT" || m.Project != iw.project {
			return fmt.Errorf("marker mismatched: %+v", m)
		}
		return nil
	})

	// --- repo context is implicit
	sc.Step(`^a repository initialized for project "SUT"$`, func() error {
		dir, err := os.MkdirTemp("", "sutra-cli-*")
		if err != nil {
			return err
		}
		clw.tempDirs = append(clw.tempDirs, dir)
		clw.workdir = dir
		if _, err := iw.identity("operator"); err != nil {
			return err
		}
		if err := clw.run("sutra init --key SUT --name Sutra", nil); err != nil {
			return err
		}
		if err := clw.expectSuccess(); err != nil {
			return err
		}
		// Resolve the project id the init created and add an issue.
		if err := iw.s.call(http.MethodGet, "/projects", nil); err != nil {
			return err
		}
		var list []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &list); err != nil {
			return err
		}
		for _, p := range list {
			if p.Key == "SUT" {
				iw.project = p.ID
			}
		}
		_, err = iw.ensureIssue("SUT-1")
		return err
	})
	sc.Step(`^"sutra issue list" is run inside it$`, func() error {
		if err := clw.run("sutra issue list", nil); err != nil {
			return err
		}
		return clw.expectSuccess()
	})
	sc.Step(`^it lists SUT's issues without a project flag$`, func() error {
		if !strings.Contains(clw.lastOut, iw.issues["SUT-1"]) {
			return fmt.Errorf("SUT-1 missing from listing: %s", clw.lastOut)
		}
		return nil
	})
	sc.Step(`^"sutra issue list" is run outside any initialized repo$`, func() error {
		dir, err := os.MkdirTemp("", "sutra-cli-bare-*")
		if err != nil {
			return err
		}
		clw.tempDirs = append(clw.tempDirs, dir)
		clw.workdir = dir
		return clw.run("sutra issue list", nil)
	})
	sc.Step(`^it requires an explicit project flag$`, func() error {
		if clw.lastCode == 0 {
			return fmt.Errorf("listing succeeded without project context")
		}
		if !strings.Contains(clw.lastErrOut, "--project") {
			return fmt.Errorf("error does not point at --project: %s", clw.lastErrOut)
		}
		return nil
	})
}

// checkParity compares the CLI's operation table against a contract:
// every declared operation must have a command, each command must call
// the method and path the contract declares, and no command may name an
// operation the contract does not declare. Extracted so the failure
// scenario can exercise it against a synthetic contract rather than
// assert around it (review 1926).
func checkParity(doc *openapi3.T, mapping map[string]cli.Mapping) error {
	missing, wrong := []string{}, []string{}
	declared := map[string]bool{}
	for pathTemplate, path := range doc.Paths.Map() {
		for method, operation := range path.Operations() {
			if operation.OperationID == "" {
				continue
			}
			declared[operation.OperationID] = true
			m, ok := mapping[operation.OperationID]
			if !ok {
				missing = append(missing, operation.OperationID)
				continue
			}
			if !strings.EqualFold(m.Method, method) || m.Path != pathTemplate {
				wrong = append(wrong, fmt.Sprintf("%s: CLI calls %s %s, contract declares %s %s",
					operation.OperationID, m.Method, m.Path, method, pathTemplate))
			}
		}
	}
	stale := []string{}
	for id := range mapping {
		if !declared[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(wrong)
	sort.Strings(stale)
	switch {
	case len(missing) > 0:
		return fmt.Errorf("operations without a CLI command: %v", missing)
	case len(wrong) > 0:
		return fmt.Errorf("CLI commands mapped to the wrong request: %v", wrong)
	case len(stale) > 0:
		return fmt.Errorf("CLI commands for operations the contract does not declare: %v", stale)
	}
	return nil
}
