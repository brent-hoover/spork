// Package cli — see MOD-cli in avspec.yaml. The CLI is a pure HTTP
// client of the API (may_import is empty): every command reaches the
// daemon over the wire, so --server against any instance behaves
// identically to loopback (AC-parity-remote) — there is no private
// path to daemon internals.
package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// op maps one contract operation to its method and path template
// ({param} segments fill from --path key=value pairs).
type op struct {
	method string
	path   string
}

// operations enumerates EVERY contract operationId (AC-parity-coverage):
// the generic `sutra api <operationId>` command invokes any of them, and
// the parity scenario diffs this table against the contract — a new
// operation missing here fails the build.
var operations = map[string]op{
	"createProject":           {http.MethodPost, "/projects"},
	"listProjects":            {http.MethodGet, "/projects"},
	"getProject":              {http.MethodGet, "/projects/{projectId}"},
	"archiveProject":          {http.MethodPost, "/projects/{projectId}/archive"},
	"exportProject":           {http.MethodGet, "/projects/{projectId}/export"},
	"importProject":           {http.MethodPost, "/projects/import"},
	"createIssue":             {http.MethodPost, "/projects/{projectId}/issues"},
	"listIssues":              {http.MethodGet, "/projects/{projectId}/issues"},
	"getIssue":                {http.MethodGet, "/issues/{issueId}"},
	"updateIssue":             {http.MethodPatch, "/issues/{issueId}"},
	"updateIssueStatus":       {http.MethodPost, "/issues/{issueId}/status"},
	"assignIssue":             {http.MethodPost, "/issues/{issueId}/assign"},
	"attachLabel":             {http.MethodPost, "/issues/{issueId}/labels"},
	"detachLabel":             {http.MethodDelete, "/issues/{issueId}/labels/{labelId}"},
	"addIssueRelation":        {http.MethodPost, "/issues/{issueId}/relations"},
	"listIssueRelations":      {http.MethodGet, "/issues/{issueId}/relations"},
	"removeIssueRelation":     {http.MethodDelete, "/issues/{issueId}/relations/{relationId}"},
	"listIssueEvents":         {http.MethodGet, "/issues/{issueId}/events"},
	"listIssueDocuments":      {http.MethodGet, "/issues/{issueId}/documents"},
	"listIssueThreads":        {http.MethodGet, "/issues/{issueId}/threads"},
	"popWorkStack":            {http.MethodPost, "/identities/{identityId}/work-stack/pop"},
	"createLabel":             {http.MethodPost, "/labels"},
	"listLabels":              {http.MethodGet, "/labels"},
	"createComment":           {http.MethodPost, "/comments"},
	"listComments":            {http.MethodGet, "/comments"},
	"createDocument":          {http.MethodPost, "/projects/{projectId}/documents"},
	"listProjectDocuments":    {http.MethodGet, "/projects/{projectId}/documents"},
	"getDocument":             {http.MethodGet, "/documents/{documentId}"},
	"saveDocVersion":          {http.MethodPost, "/documents/{documentId}/versions"},
	"getDocumentMeta":         {http.MethodGet, "/documents/{documentId}/meta"},
	"listDocVersions":         {http.MethodGet, "/documents/{documentId}/versions"},
	"getDocVersion":           {http.MethodGet, "/doc-versions/{docVersionId}"},
	"diffDocVersions":         {http.MethodGet, "/documents/{documentId}/diff"},
	"linkDocumentToIssue":     {http.MethodPost, "/documents/{documentId}/issue"},
	"unlinkDocumentFromIssue": {http.MethodDelete, "/documents/{documentId}/issue"},
	"createTemplate":          {http.MethodPost, "/templates"},
	"listTemplates":           {http.MethodGet, "/templates"},
	"getTemplate":             {http.MethodGet, "/templates/{templateId}"},
	"updateTemplate":          {http.MethodPut, "/templates/{templateId}"},
	"deleteTemplate":          {http.MethodDelete, "/templates/{templateId}"},
	"importThread":            {http.MethodPost, "/threads"},
	"getThread":               {http.MethodGet, "/threads/{threadId}"},
	"searchThreads":           {http.MethodGet, "/threads/search"},
	"setThreadAnchor":         {http.MethodPost, "/threads/{threadId}/anchor"},
	"createReview":            {http.MethodPost, "/reviews"},
	"listReviews":             {http.MethodGet, "/reviews"},
	"getReview":               {http.MethodGet, "/reviews/{reviewId}"},
	"setReviewVerdict":        {http.MethodPost, "/reviews/{reviewId}/verdict"},
	"consumeReviewApproval":   {http.MethodPost, "/reviews/{reviewId}/consume"},
	"resubmitReview":          {http.MethodPost, "/reviews/{reviewId}/resubmit"},
	"getReviewDeliverable":    {http.MethodGet, "/reviews/{reviewId}/deliverable"},
	"listEvents":              {http.MethodGet, "/events"},
	"createIdentity":          {http.MethodPost, "/identities"},
	"listIdentities":          {http.MethodGet, "/identities"},
	"search":                  {http.MethodGet, "/search"},
}

// Mapping is the method and path template the CLI invokes for one
// operationId.
type Mapping struct{ Method, Path string }

// OperationMapping exposes the whole table so the parity scenario can
// check that each command reaches the operation the contract declares —
// a correct id pointing at the wrong method or path is still a parity
// break (review 1924).
func OperationMapping() map[string]Mapping {
	out := make(map[string]Mapping, len(operations))
	for id, o := range operations {
		out[id] = Mapping{Method: o.method, Path: o.path}
	}
	return out
}

// CoveredOperations returns every operationId the CLI can invoke — the
// parity scenario compares it against the contract.
func CoveredOperations() []string {
	out := make([]string, 0, len(operations))
	for id := range operations {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Env carries the process surroundings so tests drive the CLI
// in-process — the handler-level parallel of the web decision.
type Env struct {
	Args    []string // without the program name
	Stdout  io.Writer
	Stderr  io.Writer
	Workdir string
	Getenv  func(string) string
}

// markerFile links a directory to its project (AC-project-init).
const markerFile = ".sutra"

type marker struct {
	Project string `json:"project"`
	Key     string `json:"key"`
}

// Run executes one CLI invocation; the exit code is the return.
func Run(env Env) int {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	args, flags := splitFlags(env.Args)
	server := flags["server"]
	if server == "" {
		server = env.Getenv("SUTRA_SERVER")
	}
	if server == "" {
		server = "http://127.0.0.1:7357"
	}
	c := &client{base: strings.TrimRight(server, "/"), env: env, flags: flags}
	if len(args) == 0 {
		_, _ = fmt.Fprintln(env.Stderr, "usage: sutra <init|issue|api> …")
		return 2
	}
	var err error
	switch args[0] {
	case "init":
		err = c.initProject()
	case "issue":
		err = c.issue(args[1:])
	case "api":
		err = c.generic(args[1:])
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		_, _ = fmt.Fprintln(env.Stderr, "sutra:", err)
		return 1
	}
	return 0
}

type client struct {
	base  string
	env   Env
	flags map[string]string
}

// splitFlags separates --flag value / --flag=value pairs from
// positional arguments. Boolean flags (--json, --yaml) get "true".
func splitFlags(args []string) ([]string, map[string]string) {
	positional := []string{}
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimPrefix(a, "--")
		if k, v, ok := strings.Cut(name, "="); ok {
			flags[k] = v
			continue
		}
		switch name {
		case "json", "yaml":
			flags[name] = "true"
		default:
			if i+1 < len(args) {
				flags[name] = args[i+1]
				i++
			} else {
				flags[name] = "true"
			}
		}
	}
	return positional, flags
}

func (c *client) actor() (string, error) {
	if a := c.flags["actor"]; a != "" {
		return a, nil
	}
	if a := c.env.Getenv("SUTRA_ACTOR"); a != "" {
		return a, nil
	}
	return "", fmt.Errorf("an actor identity is required: pass --actor or set SUTRA_ACTOR")
}

func (c *client) do(method, path string, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	return c.doRaw(method, path, raw)
}

// doRaw sends pre-encoded bytes verbatim.
func (c *client) doRaw(method, path string, body []byte) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, payload)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", newKey())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

// initProject anchors the working directory's repo to a new project
// and writes the marker file (AC-project-init).
func (c *client) initProject() error {
	key, name := c.flags["key"], c.flags["name"]
	if key == "" || name == "" {
		return fmt.Errorf("init requires --key and --name")
	}
	actor, err := c.actor()
	if err != nil {
		return err
	}
	status, body, err := c.do(http.MethodPost, "/projects", map[string]string{
		"key": key, "name": name, "actor": actor, "repo_path": c.env.Workdir})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("create project: %d %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return err
	}
	raw, err := json.Marshal(marker{Project: created.ID, Key: key})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.env.Workdir, markerFile), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.env.Stdout, "initialized %s -> project %s\n", key, created.ID)
	return nil
}

// projectFromContext resolves the project: --project KEY wins, else
// the marker file in the working directory (AC-project-context).
func (c *client) projectFromContext() (string, error) {
	if key := c.flags["project"]; key != "" {
		return c.projectIDByKey(key)
	}
	raw, err := os.ReadFile(filepath.Join(c.env.Workdir, markerFile))
	if err != nil {
		return "", fmt.Errorf("not inside an initialized repo: pass --project <key> or run sutra init")
	}
	var m marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("malformed %s marker: %w", markerFile, err)
	}
	return m.Project, nil
}

func (c *client) projectIDByKey(key string) (string, error) {
	// An EXPLICIT key resolves across archived projects too: archiving
	// hides a project from listings and freezes writes, but its
	// content stays readable (review 1885).
	status, body, err := c.do(http.MethodGet, "/projects?includeArchived=true", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("list projects: %d %s", status, body)
	}
	var list []struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", err
	}
	for _, p := range list {
		if p.Key == key {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("no project with key %q", key)
}

func (c *client) issue(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: sutra issue <list|show> …")
	}
	switch args[0] {
	case "list":
		projectID, err := c.projectFromContext()
		if err != nil {
			return err
		}
		// The listing is unbounded; its body flows straight into the
		// element walk — never buffered whole.
		resp, err := http.DefaultClient.Get(c.base + "/projects/" + projectID + "/issues")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			return fmt.Errorf("list issues: %d %s", resp.StatusCode, msg)
		}
		return c.emitIssueList(resp.Body)
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("usage: sutra issue show <KEY-N>")
		}
		// Split at the LAST hyphen: project keys may contain hyphens
		// (MY-PROJ-42 is project MY-PROJ, issue 42).
		cut := strings.LastIndex(args[1], "-")
		if cut <= 0 || cut == len(args[1])-1 {
			return fmt.Errorf("issue reference %q must be KEY-N", args[1])
		}
		key, number := args[1][:cut], args[1][cut+1:]
		if n, err := strconv.ParseInt(number, 10, 64); err != nil || n < 1 {
			return fmt.Errorf("issue reference %q must end in a positive number", args[1])
		}
		c.flags["project"] = key
		projectID, err := c.projectIDByKey(key)
		if err != nil {
			return err
		}
		status, body, err := c.do(http.MethodGet, "/projects/"+projectID+"/issues?number="+url.QueryEscape(number), nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("show issue: %d %s", status, body)
		}
		var page struct {
			Issues []json.RawMessage `json:"issues"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		if len(page.Issues) == 0 {
			return fmt.Errorf("no issue %s", args[1])
		}
		return c.emit(page.Issues[0])
	default:
		return fmt.Errorf("unknown issue command %q", args[0])
	}
}

// generic invokes any contract operation: sutra api <operationId>
// [--path.<name> <value>]… [--query.<name> <value>]… [--body '<json>'].
func (c *client) generic(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: sutra api <operationId> [--path.<name> <value>] [--query.<name> <value>] [--body <json>]")
	}
	o, ok := operations[args[0]]
	if !ok {
		return fmt.Errorf("unknown operation %q", args[0])
	}
	path := o.path
	for k, v := range c.flags {
		if name, found := strings.CutPrefix(k, "path."); found {
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(v))
		}
	}
	if strings.Contains(path, "{") {
		return fmt.Errorf("unfilled path parameters in %s (pass --path.<name> <value>)", path)
	}
	query := url.Values{}
	for k, v := range c.flags {
		if name, found := strings.CutPrefix(k, "query."); found {
			query.Set(name, v)
		}
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var body []byte
	if raw := c.flags["body"]; raw != "" {
		// The original bytes go on the wire UNCHANGED: a decode/re-marshal
		// would compact transcript formatting and round numbers past 2^53,
		// corrupting verbatim imports.
		if !json.Valid([]byte(raw)) {
			return fmt.Errorf("--body is not valid JSON")
		}
		body = []byte(raw)
	}
	// Successful responses stream to stdout — exports and listings are
	// unbounded and the server streams them deliberately; only error
	// bodies buffer (bounded) for the message.
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequest(o.method, c.base+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.method != http.MethodGet {
		req.Header.Set("Idempotency-Key", newKey())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_, _ = fmt.Fprintln(c.env.Stdout, string(msg))
		return fmt.Errorf("%s returned %d", args[0], resp.StatusCode)
	}
	if _, err := io.Copy(c.env.Stdout, resp.Body); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(c.env.Stdout)
	return nil
}

// expectDelim consumes one structural token and requires it to be the
// expected delimiter.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("malformed listing: expected %q, found %v", want, tok)
	}
	return nil
}

// emitIssueList walks the listing's issues array element by element —
// issue bodies are unbounded, so the list never re-materializes as one
// value.
func (c *client) emitIssueList(body io.Reader) error {
	dec := json.NewDecoder(body)
	// Delimiters are CHECKED, not merely consumed: a truncated or
	// wrong-shaped response must fail rather than print a plausible
	// listing (review 1938).
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		if keyTok == "issues" {
			if err := expectDelim(dec, '['); err != nil {
				return err
			}
			yamlMode := c.flags["yaml"] == "true"
			if !yamlMode {
				_, _ = fmt.Fprintln(c.env.Stdout, "[")
			}
			first := true
			emitted := false
			for dec.More() {
				var elem json.RawMessage
				if err := dec.Decode(&elem); err != nil {
					return err
				}
				if yamlMode {
					// Each element emits as one YAML sequence item as
					// it decodes — the listing never accumulates.
					generic, err := decodeNumeric(elem)
					if err != nil {
						return err
					}
					emitted = true
					_, _ = fmt.Fprintln(c.env.Stdout, "-")
					writeYAML(c.env.Stdout, generic, 1)
					continue
				}
				if !first {
					_, _ = fmt.Fprintln(c.env.Stdout, ",")
				}
				first = false
				_, _ = c.env.Stdout.Write(elem)
			}
			if err := expectDelim(dec, ']'); err != nil {
				return err
			}
			// The envelope must CLOSE and the stream must END. Without
			// this, a response truncated right after the issues array
			// printed as a complete listing.
			//
			// Inside an object a value cannot be decoded until its KEY
			// is consumed — Decode reports "not at beginning of value"
			// otherwise — so any property following issues needs both
			// steps. Field order is insignificant in JSON, so a server
			// that emits the watermark last must still work.
			for dec.More() {
				if _, err := dec.Token(); err != nil { // the key
					return err
				}
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					return err
				}
			}
			if err := expectDelim(dec, '}'); err != nil {
				return err
			}
			if _, err := dec.Token(); err != io.EOF {
				return fmt.Errorf("listing carries trailing data")
			}
			if !yamlMode {
				_, _ = fmt.Fprintln(c.env.Stdout, "\n]")
			} else if !emitted {
				// An empty sequence is [], never an empty document.
				_, _ = fmt.Fprintln(c.env.Stdout, "[]")
			}
			return nil
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return fmt.Errorf("listing carries no issues field")
}

// emit writes v as JSON (default and --json) or YAML (--yaml) —
// identical content either way (AC-cli-yaml).
func (c *client) emit(v any) error {
	if c.flags["yaml"] == "true" {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		generic, err := decodeNumeric(raw)
		if err != nil {
			return err
		}
		writeYAML(c.env.Stdout, generic, 0)
		return nil
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(c.env.Stdout, string(raw))
	return nil
}

// writeYAML renders decoded JSON as YAML: maps with sorted keys,
// arrays, and scalars — a small emitter instead of a dependency (new
// dependencies need approval; output shapes are our own).
func writeYAML(w io.Writer, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch val := v.(type) {
	case map[string]any:
		if len(val) == 0 {
			_, _ = fmt.Fprintf(w, "%s{}\n", pad)
			return
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch child := val[k].(type) {
			case map[string]any, []any:
				if isEmptyContainer(child) {
					_, _ = fmt.Fprintf(w, "%s%s: %s\n", pad, k, emptyLiteral(child))
					continue
				}
				_, _ = fmt.Fprintf(w, "%s%s:\n", pad, k)
				writeYAML(w, child, indent+1)
			default:
				_, _ = fmt.Fprintf(w, "%s%s: %s\n", pad, k, yamlScalar(child))
			}
		}
	case []any:
		if len(val) == 0 {
			_, _ = fmt.Fprintf(w, "%s[]\n", pad)
			return
		}
		for _, item := range val {
			switch child := item.(type) {
			case map[string]any, []any:
				if isEmptyContainer(child) {
					_, _ = fmt.Fprintf(w, "%s- %s\n", pad, emptyLiteral(child))
					continue
				}
				_, _ = fmt.Fprintf(w, "%s-\n", pad)
				writeYAML(w, child, indent+1)
			default:
				_, _ = fmt.Fprintf(w, "%s- %s\n", pad, yamlScalar(child))
			}
		}
	default:
		_, _ = fmt.Fprintf(w, "%s%s\n", pad, yamlScalar(v))
	}
}

func isEmptyContainer(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		return len(val) == 0
	case []any:
		return len(val) == 0
	}
	return false
}

func emptyLiteral(v any) string {
	if _, ok := v.(map[string]any); ok {
		return "{}"
	}
	return "[]"
}

// decodeNumeric decodes JSON with UseNumber so integers past 2^53
// survive the trip into YAML verbatim (AC-cli-yaml content parity).
func decodeNumeric(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// yamlScalar quotes strings so any JSON string round-trips: JSON
// string escaping is a valid YAML double-quoted scalar; numbers render
// their original tokens.
func yamlScalar(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case string:
		raw, _ := json.Marshal(val)
		return string(raw)
	case bool:
		return fmt.Sprintf("%t", val)
	case json.Number:
		return val.String()
	default:
		raw, _ := json.Marshal(val)
		return string(raw)
	}
}

func newKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return fmt.Sprintf("cli-%x", b)
}
