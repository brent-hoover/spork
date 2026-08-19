package docs_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"sutra/internal/docs"
	_ "sutra/internal/faultsql"
)

// docs has forty-five uncovered arms and they come in four shapes, so this
// file is mostly tables. Writing them out longhand would bury the handful
// that are genuinely interesting — the duplicate-name resolutions, the
// rename collision whose name is nil, and the delete that cannot count its
// own rows.

func handle(t *testing.T, suffix, fault string) *sql.DB {
	t.Helper()
	dsn := "file:" + t.Name() + suffix + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	if fault != "" {
		dsn += "&" + fault
	}
	h, err := sql.Open("sqlite-fault", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

type fixture struct {
	doc, version, template string
}

// seed migrates and creates one document with two versions and one
// template, then returns a SECOND handle with the fault armed — so the
// fixtures never consume the injected failure.
func seed(t *testing.T, fault string) (*sql.DB, fixture) {
	t.Helper()
	setup := handle(t, "", "")
	if err := docs.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	project := "11111111-1111-7111-8111-111111111111"
	author := "22222222-2222-7222-8222-222222222222"
	doc, _, err := docs.Create(tx, project, "Seeded", nil, "line one\n", author)
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	v2, err := docs.SaveVersion(tx, doc.ID, "line one\nline two\n", author)
	if err != nil {
		t.Fatalf("save version: %v", err)
	}
	tmpl, err := docs.CreateTemplate(tx, "seeded-template", "body")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault), fixture{doc: doc.ID, version: v2.ID, template: tmpl.ID}
}

// run drives one call inside a transaction on an armed handle and asserts
// the error carries the context that identifies WHICH arm answered. The
// context string is the whole point: an error alone does not say the test
// stood where it meant to.
func run(t *testing.T, fault string, call func(*sql.Tx, fixture) error, want string) {
	t.Helper()
	h, fx := seed(t, fault)
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = call(tx, fx)
	if err == nil {
		t.Fatalf("expected a failure carrying %q, got success", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected an error containing %q, got: %v", want, err)
	}
}

const absent = "99999999-9999-7999-8999-999999999999"

// Every read here has two answers to keep apart: the row is not there, and
// the database could not say. Reporting the second as the first turns a
// broken database into a confident 404, so each pair is asserted together.
func TestReadsSeparateAbsenceFromFailure(t *testing.T) {
	reads := []struct {
		name     string
		call     func(*sql.Tx, fixture) error
		absent   func(*sql.Tx) error
		wantCtx  string
		notFound func(error) bool
	}{
		{
			name: "Get", wantCtx: "get document",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.Get(tx, fx.doc); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.Get(tx, absent); return err },
			notFound: func(err error) bool {
				var e *docs.NotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name: "MetaByID", wantCtx: "document meta",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.MetaByID(tx, fx.doc); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.MetaByID(tx, absent); return err },
			notFound: func(err error) bool {
				var e *docs.NotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name: "VersionMetaByID", wantCtx: "version meta",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.VersionMetaByID(tx, fx.version); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.VersionMetaByID(tx, absent); return err },
			notFound: func(err error) bool {
				var e *docs.VersionNotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name: "VersionByID", wantCtx: "doc version",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.VersionByID(tx, fx.version); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.VersionByID(tx, absent); return err },
			notFound: func(err error) bool {
				var e *docs.VersionNotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name: "VersionAt", wantCtx: "version 1 of",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.VersionAt(tx, fx.doc, 1); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.VersionAt(tx, absent, 1); return err },
			notFound: func(err error) bool {
				var e *docs.VersionNotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name: "CurrentVersionMeta", wantCtx: "current version meta",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.CurrentVersionMeta(tx, fx.doc); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.CurrentVersionMeta(tx, absent); return err },
			notFound: func(err error) bool {
				var e *docs.VersionNotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name: "GetTemplate", wantCtx: "get template",
			call:   func(tx *sql.Tx, fx fixture) error { _, err := docs.GetTemplate(tx, fx.template); return err },
			absent: func(tx *sql.Tx) error { _, err := docs.GetTemplate(tx, absent); return err },
			notFound: func(err error) bool {
				var e *docs.TemplateNotFoundError
				return errors.As(err, &e)
			},
		},
	}
	for _, r := range reads {
		t.Run(r.name+"/absent", func(t *testing.T) {
			h, _ := seed(t, "")
			tx, err := h.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			err = r.absent(tx)
			if !r.notFound(err) {
				t.Fatalf("expected a not-found error, got: %v", err)
			}
		})
		t.Run(r.name+"/failed query", func(t *testing.T) {
			h, fx := seed(t, "fault_op=query&fault_after=1")
			tx, err := h.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			err = r.call(tx, fx)
			if err == nil {
				t.Fatal("reported success on a failing query")
			}
			if r.notFound(err) {
				t.Fatalf("a query failure must not be reported as not-found: %v", err)
			}
			if !strings.Contains(err.Error(), r.wantCtx) {
				t.Fatalf("expected an error containing %q, got: %v", r.wantCtx, err)
			}
		})
	}
}

// The write paths, each identified by the context its error carries. Where
// a call performs several operations the ordinal picks the one under test,
// which is why they are listed with their fault rather than sharing one.
func TestWritesReportTheirFailures(t *testing.T) {
	author := "22222222-2222-7222-8222-222222222222"
	project := "11111111-1111-7111-8111-111111111111"
	cases := []struct {
		name, fault, want string
		call              func(*sql.Tx, fixture) error
	}{
		{"Create's insert", "fault_op=exec&fault_after=1", "insert document",
			func(tx *sql.Tx, _ fixture) error {
				_, _, err := docs.Create(tx, project, "T", nil, "c", author)
				return err
			}},
		{"SaveVersion's next-number read", "fault_op=query&fault_after=2", "next version of",
			func(tx *sql.Tx, fx fixture) error { _, err := docs.SaveVersion(tx, fx.doc, "c", author); return err }},
		{"SaveVersion's insert", "fault_op=exec&fault_after=1", "insert version",
			func(tx *sql.Tx, fx fixture) error { _, err := docs.SaveVersion(tx, fx.doc, "c", author); return err }},
		{"SaveVersion's current-version move", "fault_op=exec&fault_after=2", "move current version",
			func(tx *sql.Tx, fx fixture) error { _, err := docs.SaveVersion(tx, fx.doc, "c", author); return err }},
		{"SetIssue's update", "fault_op=exec&fault_after=1", "set issue of",
			func(tx *sql.Tx, fx fixture) error { _, err := docs.SetIssue(tx, fx.doc, nil); return err }},
		{"SetIssue's read", "fault_op=query&fault_after=1", "get document",
			func(tx *sql.Tx, fx fixture) error { _, err := docs.SetIssue(tx, fx.doc, nil); return err }},
		{"CreateTemplate's insert", "fault_op=exec&fault_after=1", "insert template",
			func(tx *sql.Tx, _ fixture) error { _, err := docs.CreateTemplate(tx, "fresh", "b"); return err }},
		{"UpdateTemplate's read", "fault_op=query&fault_after=1", "get template",
			func(tx *sql.Tx, fx fixture) error {
				name := "renamed"
				_, err := docs.UpdateTemplate(tx, fx.template, &name, nil)
				return err
			}},
		{"UpdateTemplate's update", "fault_op=exec&fault_after=1", "update template",
			func(tx *sql.Tx, fx fixture) error {
				name := "renamed"
				_, err := docs.UpdateTemplate(tx, fx.template, &name, nil)
				return err
			}},
		{"DeleteTemplate's delete", "fault_op=exec&fault_after=1", "delete template",
			func(tx *sql.Tx, fx fixture) error { return docs.DeleteTemplate(tx, fx.template) }},
		{"VersionSizesOK's size read", "fault_op=query&fault_after=1", "size version",
			func(tx *sql.Tx, fx fixture) error { return docs.VersionSizesOK(tx, fx.doc, 1, 2) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { run(t, tc.fault, tc.call, tc.want) })
	}
}

// The streaming helpers, each with the same three failures: the query, a row
// that will not scan, and a caller that stops. Separate code per helper, so
// separately reachable.
func TestStreamsHandleEveryFailure(t *testing.T) {
	project := "11111111-1111-7111-8111-111111111111"
	streams := []struct {
		name, queryCtx, scanCtx string
		each                    func(*sql.Tx, fixture, func() error) error
	}{
		{"VersionsEach", "versions of", "scan version",
			func(tx *sql.Tx, fx fixture, stop func() error) error {
				return docs.VersionsEach(tx, fx.doc, func(docs.Version) error { return stop() })
			}},
		{"IDsByProject", "list document ids", "scan document id",
			func(tx *sql.Tx, _ fixture, _ func() error) error {
				_, err := docs.IDsByProject(tx, project)
				return err
			}},
		{"SearchIDs", "list document ids", "scan document id",
			func(tx *sql.Tx, _ fixture, _ func() error) error {
				_, err := docs.SearchIDs(tx, &project, "line")
				return err
			}},
	}
	for _, s := range streams {
		t.Run(s.name+"/failed query", func(t *testing.T) {
			run(t, "fault_op=query&fault_after=1",
				func(tx *sql.Tx, fx fixture) error { return s.each(tx, fx, func() error { return nil }) },
				s.queryCtx)
		})
		t.Run(s.name+"/unscannable row", func(t *testing.T) {
			run(t, "fault_op=badrow&fault_after=1",
				func(tx *sql.Tx, fx fixture) error { return s.each(tx, fx, func() error { return nil }) },
				s.scanCtx)
		})
	}

	t.Run("VersionsEach/callback stops the stream", func(t *testing.T) {
		h, fx := seed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		sentinel := errors.New("caller stopped")
		err = docs.VersionsEach(tx, fx.doc, func(docs.Version) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}

// The two document cursors and the template cursor: open failure, plus the
// three arms of Each.
func TestCursorsHandleEveryFailure(t *testing.T) {
	project := "11111111-1111-7111-8111-111111111111"

	t.Run("openDocuments fails", func(t *testing.T) {
		run(t, "fault_op=query&fault_after=1",
			func(tx *sql.Tx, _ fixture) error { _, err := docs.OpenByProject(tx, project); return err },
			"list documents")
	})
	t.Run("OpenTemplates fails", func(t *testing.T) {
		run(t, "fault_op=query&fault_after=1",
			func(tx *sql.Tx, _ fixture) error { _, err := docs.OpenTemplates(tx); return err },
			"list templates")
	})

	t.Run("document cursor's row will not scan", func(t *testing.T) {
		h, _ := seed(t, "fault_op=badrow&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		cursor, err := docs.OpenByProject(tx, project)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		err = cursor.Each(func(docs.Document) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "scan document") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})
	t.Run("document cursor breaks mid-stream", func(t *testing.T) {
		h, _ := seed(t, "fault_op=next&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		cursor, err := docs.OpenByProject(tx, project)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		if err := cursor.Each(func(docs.Document) error { return nil }); err == nil {
			t.Fatal("Each reported success on a broken cursor")
		}
	})
	t.Run("document cursor's caller stops", func(t *testing.T) {
		h, _ := seed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		cursor, err := docs.OpenByProject(tx, project)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		sentinel := errors.New("caller stopped")
		if err := cursor.Each(func(docs.Document) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})

	t.Run("template cursor's row will not scan", func(t *testing.T) {
		h, _ := seed(t, "fault_op=badrow&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		cursor, err := docs.OpenTemplates(tx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		err = cursor.Each(func(docs.Template) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "scan template") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})
	t.Run("template cursor's caller stops", func(t *testing.T) {
		h, _ := seed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		cursor, err := docs.OpenTemplates(tx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		sentinel := errors.New("caller stopped")
		if err := cursor.Each(func(docs.Template) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
	t.Run("template cursor breaks mid-stream", func(t *testing.T) {
		h, _ := seed(t, "fault_op=next&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		cursor, err := docs.OpenTemplates(tx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		if err := cursor.Each(func(docs.Template) error { return nil }); err == nil {
			t.Fatal("Each reported success on a broken cursor")
		}
	})
}

// --- the arms that are not routine ---------------------------------------

// docs.go:605 — CreateTemplate's unique-violation test taking its FALSE
// side: the insert failed for some other reason. The suite had only ever
// produced duplicates here.
func TestCreateTemplateReportsANonDuplicateFailure(t *testing.T) {
	h, _ := seed(t, "fault_op=exec&fault_after=1")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = docs.CreateTemplate(tx, "unrelated-failure", "b")
	var dup *docs.DuplicateTemplateNameError
	if errors.As(err, &dup) {
		t.Fatal("a non-duplicate failure took the duplicate path")
	}
	if err == nil || !strings.Contains(err.Error(), "insert template") {
		t.Fatalf("expected the insert failure, got: %v", err)
	}
}

// docs.go:609 — the duplicate name was real and the read that resolves WHICH
// template holds it then failed. The caller reads ExistingID off that error,
// so an unresolved duplicate must not be reported as a resolved one.
func TestCreateTemplateReportsAFailedDuplicateResolution(t *testing.T) {
	h, _ := seed(t, "fault_op=query&fault_after=1")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// "seeded-template" already exists, so the insert hits the constraint
	// honestly; the resolution read is this handle's first query.
	_, err = docs.CreateTemplate(tx, "seeded-template", "b")
	var dup *docs.DuplicateTemplateNameError
	if errors.As(err, &dup) {
		t.Fatal("an unresolved duplicate must not be reported as a resolved one")
	}
	if err == nil || !strings.Contains(err.Error(), "resolve duplicate template") {
		t.Fatalf("expected the resolution failure, got: %v", err)
	}
}

// docs.go:679 and :681 — UpdateTemplate's rename collision. Two arms:
// the resolution read failing, and the case where the UPDATE reports a
// unique violation while name is nil.
//
// That second one cannot arise from SQLite — only the name column is
// unique, so a content-only update has nothing to collide with. It is
// reachable exactly because the injector can choose the error TEXT, and it
// is worth reaching: the branch decides whether a collision is reported as
// a duplicate name or as a plain update failure, and getting it wrong on a
// content-only update would invent a name conflict out of nothing.
func TestUpdateTemplateRenameCollision(t *testing.T) {
	t.Run("resolution read fails", func(t *testing.T) {
		setup := handle(t, "", "")
		if err := docs.Migrate(setup); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		tx, err := setup.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		first, err := docs.CreateTemplate(tx, "taken", "b")
		if err != nil {
			t.Fatalf("create first: %v", err)
		}
		_ = first
		second, err := docs.CreateTemplate(tx, "free", "b")
		if err != nil {
			t.Fatalf("create second: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// GetTemplate is the first query; the resolution read is the second.
		armed := handle(t, "", "fault_op=query&fault_after=2")
		tx2, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx2.Rollback() }()
		name := "taken"
		_, err = docs.UpdateTemplate(tx2, second.ID, &name, nil)
		var dup *docs.DuplicateTemplateNameError
		if errors.As(err, &dup) {
			t.Fatal("an unresolved collision must not be reported as a resolved one")
		}
		if err == nil || !strings.Contains(err.Error(), "resolve rename collision") {
			t.Fatalf("expected the resolution failure, got: %v", err)
		}
	})

	t.Run("a unique violation with no rename", func(t *testing.T) {
		h, fx := seed(t, "fault_op=exec&fault_after=1&fault_msg=UNIQUE+constraint+failed:+doc_templates.name")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		content := "content only, no rename"
		_, err = docs.UpdateTemplate(tx, fx.template, nil, &content)
		var dup *docs.DuplicateTemplateNameError
		if errors.As(err, &dup) {
			t.Fatal("a content-only update must not invent a name conflict")
		}
		if err == nil || !strings.Contains(err.Error(), "update template") {
			t.Fatalf("expected the plain update failure, got: %v", err)
		}
	})
}

// docs.go:698 — DeleteTemplate's `err != nil || n == 0`. Both halves report
// the same not-found answer, and both are reachable: an id that names
// nothing, and a delete whose row count cannot be read. SQLite never fails
// the count, which is what the injector's rowsaffected mode is for.
func TestDeleteTemplateReportsNotFoundForBothHalves(t *testing.T) {
	t.Run("no such template", func(t *testing.T) {
		h, _ := seed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = docs.DeleteTemplate(tx, absent)
		var nf *docs.TemplateNotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected TemplateNotFoundError, got: %v", err)
		}
	})
	t.Run("row count unavailable", func(t *testing.T) {
		h, fx := seed(t, "fault_op=rowsaffected&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = docs.DeleteTemplate(tx, fx.template)
		var nf *docs.TemplateNotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected TemplateNotFoundError, got: %v", err)
		}
	})
}

// docs.go:445 — VersionSizesOK's ErrNoRows arm, which CONTINUES rather than
// failing: the load path reports the 404, so a missing version here is not
// this function's business.
func TestVersionSizesOKSkipsAbsentVersions(t *testing.T) {
	h, fx := seed(t, "")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := docs.VersionSizesOK(tx, fx.doc, 1, 99); err != nil {
		t.Fatalf("an absent version must be skipped, not reported: %v", err)
	}
}

// docs.go:112 — Migrate's `err != nil`.
func TestMigrateReportsAFailedExec(t *testing.T) {
	h := handle(t, "", "fault_op=exec&fault_after=1")
	if err := docs.Migrate(h); err == nil {
		t.Fatal("Migrate reported success on a failing exec")
	} else if !strings.Contains(err.Error(), "migrate docs") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// docs.go:288 — SaveVersion's `err != nil` from the MetaByID that proves the
// document exists, and docs.go:126 — Create propagating that same failure
// out of the SaveVersion it calls. The document insert is an exec, so a
// query fault leaves it alone and lands on the existence check.
func TestSaveVersionReportsAFailedExistenceCheck(t *testing.T) {
	t.Run("called directly", func(t *testing.T) {
		h, fx := seed(t, "fault_op=query&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = docs.SaveVersion(tx, fx.doc, "c", "22222222-2222-7222-8222-222222222222")
		if err == nil {
			t.Fatal("SaveVersion reported success when the document could not be read")
		}
		var nf *docs.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("a query failure must not be reported as an absent document")
		}
		if !strings.Contains(err.Error(), "document meta") {
			t.Fatalf("error lost its context: %v", err)
		}
	})
	t.Run("through Create", func(t *testing.T) {
		h, _ := seed(t, "fault_op=query&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, _, err = docs.Create(tx, "11111111-1111-7111-8111-111111111111", "T", nil, "c",
			"22222222-2222-7222-8222-222222222222")
		if err == nil {
			t.Fatal("Create reported success when its first version could not be saved")
		}
		if !strings.Contains(err.Error(), "document meta") {
			t.Fatalf("the failure did not come through SaveVersion: %v", err)
		}
	})
}
