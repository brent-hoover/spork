package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"kriya/internal/specverify"
)

// manifestName is the file every spec has and every snapshot must pin.
const manifestName = "avspec.yaml"

// Snapshot is the pinned, content-addressed spec a build reads from.
//
// Builds and recovery read gate commands, acceptance criteria, scenarios, and
// contracts from here and never from the mutable working tree, so edits made
// after intake cannot change what a pinned build executes
// (AC-intake-snapshot-authority).
type Snapshot struct {
	Hash string
	// Content maps each artifact's spec-relative path to its bytes: the
	// manifest, every referenced feature file, every contract.
	Content map[string]string
	// ResolvedCommands maps module id to its effective commands, exactly as
	// intake validated them.
	ResolvedCommands map[string]map[string]string
	// Law is each module's declared boundary and contracts, and the project's
	// constitution. Pinned alongside the files because context assembly is
	// judged against what was ADMITTED — a boundary edited since would let an
	// agent be told law nothing validated.
	Law          []specverify.Module
	Constitution []specverify.ConstitutionEntry
	Created      time.Time
}

// SnapshotStore persists pinned snapshots.
type SnapshotStore interface {
	Put(ctx context.Context, s Snapshot) error
	Get(ctx context.Context, hash string) (Snapshot, error)
	Count(ctx context.Context) (int, error)
}

// readArtifacts loads every file the model names, relative to dir.
//
// A missing artifact is an error, not an omission: the spec referenced it, so
// a snapshot without it would be silently incomplete and a build reading from
// that snapshot would diverge from the spec that was validated.
func readArtifacts(dir string, paths []string) (map[string]string, error) {
	content := make(map[string]string, len(paths))
	for _, rel := range paths {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return nil, fmt.Errorf("read artifact %s: %w", rel, err)
		}
		content[rel] = string(b)
	}
	return content, nil
}

// hashSnapshot content-addresses the artifact set and the resolved commands.
//
// Both are hashed, not just the files: two intakes of identical files whose
// modules resolve different commands are different builds. Keys are sorted so
// the hash depends on content rather than map iteration order, and each field
// is length-prefixed so no concatenation of one input can imitate another.
func hashSnapshot(
	content map[string]string,
	commands map[string]map[string]string,
	law []specverify.Module,
	constitution []specverify.ConstitutionEntry,
) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			// hash.Hash documents that Write never returns an error, so there
			// is nothing to handle; discarding explicitly says so rather than
			// leaving it to look like an oversight.
			_, _ = fmt.Fprintf(h, "%d:%s", len(p), p)
		}
	}
	for _, path := range sortedKeys(content) {
		write("artifact", path, content[path])
	}
	for _, module := range sortedKeys(commands) {
		for _, name := range sortedKeys(commands[module]) {
			write("command", module, name, commands[module][name])
		}
	}
	// The law is hashed too. It is derived from the manifest, which is already
	// in the artifact set, but hashing what the snapshot STORES rather than
	// what it could be re-derived from is what makes the hash an address for
	// this snapshot rather than for its inputs.
	for _, module := range law {
		write("law", module.ID, module.Name)
		for _, dep := range module.MayImport {
			write("may-import", module.ID, dep)
		}
		for _, c := range module.Contracts {
			write("contract", module.ID, c.ID, c.Type, c.Path)
		}
	}
	for _, entry := range constitution {
		write("constitution", entry.ID, entry.Statement)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pin builds and stores the snapshot for an admitted spec.
func (i Intaker) pin(ctx context.Context, dir string, model specverify.Model) (Snapshot, error) {
	// The manifest specifically, not merely a non-empty list: a set carrying
	// some other readable file but not avspec.yaml would pin a "full artifact
	// set" with no spec in it, and every later read of the snapshot would find
	// nothing to read.
	if !slices.Contains(model.Artifacts, manifestName) {
		return Snapshot{}, fmt.Errorf("spec at %s resolved an artifact set without %s: %v",
			dir, manifestName, model.Artifacts)
	}
	content, err := readArtifacts(dir, model.Artifacts)
	if err != nil {
		return Snapshot{}, err
	}
	commands := make(map[string]map[string]string, len(model.Modules))
	for _, m := range model.Modules {
		commands[m.ID] = m.Commands
	}
	s := Snapshot{
		Hash:             hashSnapshot(content, commands, model.Modules, model.Constitution),
		Content:          content,
		ResolvedCommands: commands,
		Law:              model.Modules,
		Constitution:     model.Constitution,
		Created:          i.Now.Now(),
	}
	if err := i.Snapshots.Put(ctx, s); err != nil {
		return Snapshot{}, fmt.Errorf("pin snapshot: %w", err)
	}
	return s, nil
}
