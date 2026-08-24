package cli_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/cli"
	kctx "kriya/internal/context"
)

// recorder captures what the command asked to record.
type recorder struct {
	got kctx.Capture
	err error
}

func (r *recorder) Record(_ context.Context, c kctx.Capture) error {
	r.got = c
	return r.err
}

func TestAManualLearningIsRecordedWithItsTags(t *testing.T) {
	var out bytes.Buffer
	r := &recorder{}
	c := kctx.Capture{
		Scope: kctx.ScopeProject, ProjectKey: "/target",
		Lesson: "never swallow an exception", Module: "MOD-api",
		Pattern: "error-handling", SourceKind: kctx.SourceOperator,
	}
	if err := cli.Learn(context.Background(), &out, r, c); err != nil {
		t.Fatalf("learn: %v", err)
	}
	if r.got != c {
		t.Errorf("recorded %+v", r.got)
	}
	if !strings.Contains(out.String(), "MOD-api") {
		t.Errorf("the operator was told %q", out.String())
	}
}

func TestARefusalReachesTheOperator(t *testing.T) {
	// The write is what enforces ENT-learning's conditional rules, so its
	// refusal is the operator's answer — and one they cannot see is one they
	// cannot act on.
	var out bytes.Buffer
	r := &recorder{err: errors.New("a project learning needs its project key")}
	err := cli.Learn(context.Background(), &out, r, kctx.Capture{
		Scope: kctx.ScopeProject, Lesson: "x", Module: "m", Pattern: "p",
		SourceKind: kctx.SourceOperator,
	})
	if err == nil {
		t.Fatal("a refused learning read as recorded")
	}
	if !strings.Contains(out.String(), "project key") {
		t.Errorf("the refusal did not reach the operator: %q", out.String())
	}
}

func TestOutputThatVanishedIsAnErrorForLearnToo(t *testing.T) {
	if err := cli.Learn(context.Background(), brokenWriter{}, &recorder{}, kctx.Capture{
		Scope: kctx.ScopeGlobal, Lesson: "x", Module: "m", Pattern: "p",
		SourceKind: kctx.SourceOperator,
	}); err == nil {
		t.Fatal("a confirmation that vanished read as delivered")
	}
}
