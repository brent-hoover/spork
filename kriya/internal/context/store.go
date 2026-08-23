package context

import (
	stdctx "context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
type SQLLearnings struct{ DB *sql.DB }

// Matching returns learnings tagged with any of these modules or patterns.
//
// Either tag matching is enough: a lesson about a failure pattern is worth
// having on a module that has not hit it yet, which is the whole point of
// feeding it forward.
func (s SQLLearnings) Matching(ctx stdctx.Context, modules, patterns []string) ([]Learning, error) {
	if len(modules) == 0 && len(patterns) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(modules)+len(patterns))
	var clauses []string
	if len(modules) > 0 {
		clauses = append(clauses, "module IN ("+placeholders(len(modules))+")")
		for _, m := range modules {
			args = append(args, m)
		}
	}
	if len(patterns) > 0 {
		clauses = append(clauses, "pattern IN ("+placeholders(len(patterns))+")")
		for _, p := range patterns {
			args = append(args, p)
		}
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT lesson, module, pattern FROM learning
		   WHERE `+strings.Join(clauses, " OR ")+` ORDER BY id`, args...)
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
