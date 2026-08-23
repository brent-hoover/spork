package devloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Import states a session's transcript passes through.
//
// The marker is advanced BEFORE the call and stamped with the thread id after,
// so a crash in between leaves a row that says an import may be in flight —
// which is exactly what recovery replays.
const (
	ImportNone      = "none"
	ImportImporting = "importing"
	ImportImported  = "imported"
)

// Exchange is one turn kriya had with an agent.
type Exchange struct {
	Role   string `json:"role"`
	Prompt string `json:"prompt"`
	Reply  string `json:"reply"`
	Model  string `json:"model"`
}

// Threads imports a transcript into the tracker's thread catalog.
type Threads interface {
	// Import returns the thread id. The key is the caller's, sent as the
	// Idempotency-Key, so a replay returns the original thread rather than
	// creating a second one.
	Import(ctx context.Context, title string, transcript json.RawMessage,
		session, issue, actor, key string) (string, error)
}

// importKey is deterministic per session.
//
// Derived from the session id and nothing else: recovery must present the SAME
// key the original call used, and a key that mixed in anything recovery
// reconstructs — a timestamp, an attempt number — would differ and import a
// second thread.
func importKey(session string) string {
	sum := sha256.Sum256([]byte("kriya-thread-import:" + session))
	return hex.EncodeToString(sum[:])
}

// writeTranscript persists the conversation and returns its reference.
//
// Written BEFORE the import and never rewritten: the reference is immutable so
// a replay reproduces the EXACT request, and a transcript regenerated at
// recovery time could differ from the one sutra may already hold.
func writeTranscript(workspace, session string, turns []Exchange) (string, error) {
	body, err := json.Marshal(turns)
	if err != nil {
		return "", fmt.Errorf("encode transcript: %w", err)
	}
	dir := filepath.Join(workspace, ".kriya", "transcripts")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("make %s: %w", dir, err)
	}
	path := filepath.Join(dir, session+".json")
	if _, err := os.Stat(path); err == nil {
		// Already written. Rewriting would change the content a landed import
		// was made from.
		return path, nil
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// readTranscript loads a persisted transcript by reference.
func readTranscript(ref string) (json.RawMessage, error) {
	body, err := os.ReadFile(ref)
	if err != nil {
		return nil, fmt.Errorf("read transcript %s: %w", ref, err)
	}
	return body, nil
}

// capture imports a finished session's transcript.
//
// AC-thread-no-loss: every outcome reaches the thread catalog, crashed
// included. The reference and the key are persisted together, in one write,
// before sutra is called.
func (l Loop) capture(ctx context.Context, req Request, session *Session, turns []Exchange) error {
	if l.Threads == nil {
		return nil
	}
	ref, err := writeTranscript(req.Workspace, session.SessionID, turns)
	if err != nil {
		return err
	}
	session.TranscriptRef = ref
	session.ImportKey = importKey(session.SessionID)
	session.ImportState = ImportImporting
	if err := l.Store.Upsert(ctx, *session); err != nil {
		return fmt.Errorf("record pending import: %w", err)
	}
	return l.finishImport(ctx, req.Issue, req.Actor, session)
}

// finishImport performs the call the write-ahead row promised.
//
// Shared with recovery, so a replayed import takes exactly the same path as a
// fresh one — two implementations of one protocol would leave only one tested.
func (l Loop) finishImport(ctx context.Context, issue, actor string, session *Session) error {
	transcript, err := readTranscript(session.TranscriptRef)
	if err != nil {
		return err
	}
	thread, err := l.Threads.Import(ctx, session.Ticket, transcript,
		session.SessionID, issue, actor, session.ImportKey)
	if err != nil {
		return fmt.Errorf("import transcript for %s: %w", session.SessionID, err)
	}
	session.ThreadRef = thread
	session.ImportState = ImportImported
	if err := l.Store.Upsert(ctx, *session); err != nil {
		return fmt.Errorf("record imported thread: %w", err)
	}
	return nil
}

// RecoverImports replays imports a crash left in flight.
//
// Replayed under the SAME key: sutra returns the original thread for an import
// that landed and creates one otherwise, so exactly one thread exists for the
// session either way.
func (l Loop) RecoverImports(ctx context.Context, issue, actor string) (int, error) {
	sessions, err := l.Store.Importing(ctx)
	if err != nil {
		return 0, fmt.Errorf("list importing sessions: %w", err)
	}
	for i := range sessions {
		if err := l.finishImport(ctx, issue, actor, &sessions[i]); err != nil {
			return 0, err
		}
	}
	return len(sessions), nil
}
