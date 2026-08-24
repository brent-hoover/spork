package context

import (
	stdctx "context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"kriya/internal/clock"
)

// Migration is the context module's schema.
const Migration = `
CREATE TABLE context_bundle (
    build   TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    created TEXT NOT NULL
);
CREATE TABLE learning (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope       TEXT NOT NULL,
    project_key TEXT NOT NULL DEFAULT '',
    lesson      TEXT NOT NULL,
    module      TEXT NOT NULL,
    pattern     TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_run  TEXT NOT NULL DEFAULT '',
    created     TEXT NOT NULL
)`

// ProvenanceMigration adds what a captured learning must carry to be
// traceable to its origin.
const ProvenanceMigration = `
ALTER TABLE learning ADD COLUMN source_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE learning ADD COLUMN source_doc TEXT NOT NULL DEFAULT '';
ALTER TABLE learning ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE learning ADD COLUMN source_session TEXT NOT NULL DEFAULT ''`

// SQLBundles persists context bundles.
type SQLBundles struct{ DB *sql.DB }

// Put records what an agent was given.
func (s SQLBundles) Put(ctx stdctx.Context, b Bundle) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO context_bundle (build, content, created) VALUES (?, ?, ?)
		 ON CONFLICT(build) DO UPDATE SET
		   content = excluded.content, created = excluded.created`,
		b.Build, string(b.Content), b.Created.UTC().Format(timeLayout))
	if err != nil {
		return fmt.Errorf("upsert context bundle: %w", err)
	}
	return nil
}

// Get reads the bundle a run was given.
func (s SQLBundles) Get(ctx stdctx.Context, build string) (Bundle, bool, error) {
	var body, created string
	err := s.DB.QueryRowContext(ctx,
		`SELECT content, created FROM context_bundle WHERE build = ?`, build).
		Scan(&body, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Bundle{}, false, nil
	}
	if err != nil {
		return Bundle{}, false, fmt.Errorf("read context bundle: %w", err)
	}
	b := Bundle{Build: build, Content: []byte(body)}
	if b.Created, err = parseTime(created); err != nil {
		return Bundle{}, false, err
	}
	return b, true, nil
}

// SQLLearnings reads and writes the learning store.
type SQLLearnings struct {
	DB  *sql.DB
	Now clock.Clock
}

// Record persists a captured learning.
//
// Validated at write time, because ENT-learning's conditional requirements are
// ones the format cannot express — and the write is the last moment anything
// knows enough to reject an entry that would be unmatched or untraceable
// forever after.
func (s SQLLearnings) Record(ctx stdctx.Context, c Capture) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("reject learning: %w", err)
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO learning (scope, project_key, lesson, module, pattern,
		   source_kind, source_run, source_commit, source_doc, source_ref,
		   source_session, created)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Scope, c.ProjectKey, c.Lesson, c.Module, c.Pattern, c.SourceKind,
		c.SourceRun, c.SourceCommit, c.SourceDoc, c.SourceRef, c.SourceSession,
		s.Now.Now().UTC().Format(timeLayout))
	if err != nil {
		return fmt.Errorf("record learning: %w", err)
	}
	return nil
}

// Provenance reads a learning's origin back.
//
// Exists because "every learning knows its origin" is a claim about what was
// STORED, and the feed-forward path deliberately reads none of it.
func (s SQLLearnings) Provenance(ctx stdctx.Context, lesson string) (Capture, bool, error) {
	var c Capture
	err := s.DB.QueryRowContext(ctx,
		`SELECT scope, project_key, lesson, module, pattern, source_kind,
		   source_run, source_commit, source_doc, source_ref, source_session
		   FROM learning WHERE lesson = ? ORDER BY id DESC LIMIT 1`, lesson).
		Scan(&c.Scope, &c.ProjectKey, &c.Lesson, &c.Module, &c.Pattern, &c.SourceKind,
			&c.SourceRun, &c.SourceCommit, &c.SourceDoc, &c.SourceRef, &c.SourceSession)
	if errors.Is(err, sql.ErrNoRows) {
		return Capture{}, false, nil
	}
	if err != nil {
		return Capture{}, false, fmt.Errorf("read learning provenance: %w", err)
	}
	return c, true, nil
}

// Matching returns learnings tagged with any of these modules or patterns.
//
// Either tag matching is enough: a lesson about a failure pattern is worth
// having on a module that has not hit it yet, which is the whole point of
// feeding it forward.
func (s SQLLearnings) Matching(
	ctx stdctx.Context, projectKey string, modules, patterns []string,
) ([]Learning, error) {
	if len(modules) == 0 && len(patterns) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(modules)+len(patterns))
	var clauses []string
	// An empty tag set contributes NO clause. SQLite accepts `x IN ()` and
	// evaluates it false, so an empty list would not error — it would quietly
	// widen nothing while looking like a filter.
	for _, tag := range []struct {
		column string
		values []string
	}{{"module", modules}, {"pattern", patterns}} {
		if len(tag.values) == 0 {
			continue
		}
		clauses = append(clauses, tag.column+" IN ("+placeholders(len(tag.values))+")")
		for _, v := range tag.values {
			args = append(args, v)
		}
	}
	// Global learnings and THIS project's, never another project's. A global
	// lesson is visible outside its project of origin — that is its purpose —
	// but a project-scoped one is not, and one database can hold several
	// projects. A manual learning is indistinguishable here from a captured
	// one, which is what makes the feed-forward path treat them alike.
	args = append([]any{projectKey}, args...)
	rows, err := s.DB.QueryContext(ctx,
		`SELECT lesson, module, pattern FROM learning
		   WHERE (scope = 'global' OR project_key = ?)
		     AND (`+strings.Join(clauses, " OR ")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query learnings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Learning
	for rows.Next() {
		var l Learning
		if err := rows.Scan(&l.Lesson, &l.Module, &l.Pattern); err != nil {
			return nil, fmt.Errorf("scan learning: %w", err)
		}
		out = append(out, l)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "nothing has been learned yet".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learnings: %w", err)
	}
	return out, nil
}

// placeholders builds "?, ?, ?" for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
