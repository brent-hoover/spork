package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"kriya/internal/specverify"
)

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
	Created          time.Time
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
func hashSnapshot(content map[string]string, commands map[string]map[string]string) string {
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
	content, err := readArtifacts(dir, model.Artifacts)
	if err != nil {
		return Snapshot{}, err
	}
	commands := make(map[string]map[string]string, len(model.Modules))
	for _, m := range model.Modules {
		commands[m.ID] = m.Commands
	}
	s := Snapshot{
		Hash:             hashSnapshot(content, commands),
		Content:          content,
		ResolvedCommands: commands,
		Created:          i.Now.Now(),
	}
	if err := i.Snapshots.Put(ctx, s); err != nil {
		return Snapshot{}, fmt.Errorf("pin snapshot: %w", err)
	}
	return s, nil
}
