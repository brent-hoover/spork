#!/usr/bin/env bash
# Regression suite for branch-coverage.sh.
#
# Every case here is a way the gate could report a number it had not
# earned. Each was found by a review or by running the gate, and each
# was verified by hand exactly once before this file existed — which is
# the reason it exists.
#
# The fixtures are a throwaway Go module, so this suite says nothing
# about any real app and cannot rot when one changes.
#
# Usage: scripts/branch-coverage-test.sh
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
gate=$here/branch-coverage.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pass=0
fail=0

# check NAME EXPECTED-EXIT PATTERN -- runs the gate in the fixture
# module and requires both the exit status and a line of output. An
# expected exit alone is not enough: "0 because it measured nothing" is
# the bug this whole file is about.
check() {
	local name=$1 wantexit=$2 wantout=$3
	shift 3
	local out status
	out=$(cd "$work/mod" && "$gate" "$@" 2>&1)
	status=$?
	if [ "$status" != "$wantexit" ]; then
		printf 'FAIL %s: exit %s, wanted %s\n%s\n' "$name" "$status" "$wantexit" "$out"
		fail=$((fail + 1))
		return
	fi
	if ! printf '%s' "$out" | grep -qE "$wantout"; then
		printf 'FAIL %s: output did not match /%s/\n%s\n' "$name" "$wantout" "$out"
		fail=$((fail + 1))
		return
	fi
	printf 'ok   %s\n' "$name"
	pass=$((pass + 1))
}

reset() {
	rm -rf "$work/mod"
	mkdir -p "$work/mod"
	cat >"$work/mod/go.mod" <<'EOF'
module fixture

go 1.25
EOF
}

# A subject package with exactly one condition, so its arm total is 2
# and "half covered" is expressible.
subject() {
	mkdir -p "$work/mod/subject"
	cat >"$work/mod/subject/subject.go" <<'EOF'
package subject

func Greeting(loud bool) string {
	if loud {
		return "HI"
	}
	return "hi"
}
EOF
}

echo "== a conditionless package still runs its drivers =="
reset
mkdir -p "$work/mod/nocond"
cat >"$work/mod/nocond/nocond.go" <<'EOF'
package nocond

func Greeting() string { return "hi" }
EOF
cat >"$work/mod/nocond/nocond_test.go" <<'EOF'
package nocond

import "testing"

func TestGreeting(t *testing.T) {
	if Greeting() != "deliberately wrong" {
		t.Fatal("deliberate failure")
	}
}
EOF
check "conditionless package with a failing test fails" 1 'driver fixture/nocond did not pass' ./nocond
perl -pi -e 's/deliberately wrong/hi/' "$work/mod/nocond/nocond_test.go"
check "conditionless package with a passing test passes" 0 'no conditions' ./nocond

echo
echo "== a test-only package is not instrumented =="
reset
subject
mkdir -p "$work/mod/harness"
cat >"$work/mod/harness/harness_test.go" <<'EOF'
package harness_test

import (
	"testing"

	"fixture/subject"
)

func TestBoth(t *testing.T) {
	if subject.Greeting(true) != "HI" || subject.Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
check "a package with no production code is skipped" 0 'no production code' ./harness

echo
echo "== cross-package attribution, every way a driver can end =="
# The harness above has no TestMain: Go's generated main exits without
# flushing unless the gate injects one.
check "a foreign driver with no TestMain is attributed" 0 'fixture/subject +2/2 arms' ./subject

cat >"$work/mod/harness/main_test.go" <<'EOF'
package harness_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }
EOF
check "a foreign driver whose TestMain exits is attributed" 0 'fixture/subject +2/2 arms' ./subject

# The case review 2003 named: finding one os.Exit does not prove every
# path takes it. This TestMain exits only on failure, so the passing run
# RETURNS and the ticker is the only thing left — which is exactly what
# loses the final hits.
cat >"$work/mod/harness/main_test.go" <<'EOF'
package harness_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if code != 0 {
		os.Exit(code)
	}
}
EOF
check "a foreign driver whose TestMain returns is attributed" 0 'fixture/subject +2/2 arms' ./subject

# And with no os.Exit anywhere, so the rewrite has nothing to catch and
# only the wrapper's deferred flush can save it.
cat >"$work/mod/harness/main_test.go" <<'EOF'
package harness_test

import "testing"

func TestMain(m *testing.M) { m.Run() }
EOF
check "a foreign driver whose TestMain never exits is attributed" 0 'fixture/subject +2/2 arms' ./subject

echo
echo "== an uncovered arm fails =="
reset
subject
mkdir -p "$work/mod/harness"
cat >"$work/mod/harness/harness_test.go" <<'EOF'
package harness_test

import (
	"testing"

	"fixture/subject"
)

func TestQuietOnly(t *testing.T) {
	if subject.Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
check "an arm never taken fails the package" 1 'never true' ./subject

echo
echo "== the skip file is honoured, and only understates =="
reset
subject
cat >"$work/mod/subject/subject_test.go" <<'EOF'
package subject

import "testing"

func TestLoud(t *testing.T) {
	if Greeting(true) != "HI" {
		t.Fatal("wrong")
	}
}

// TestUninstrumentable stands in for a test that measures the build
// rather than behavior: it passes only when the package is NOT
// instrumented, so the gate must skip it or fail.
func TestUninstrumentable(t *testing.T) {
	t.Fatal("cannot survive instrumentation")
}
EOF
check "an unskipped uninstrumentable test fails the gate" 1 'did not pass' ./subject
cat >"$work/mod/branch-coverage-skip" <<'EOF'
# Measures the build, not behavior.
^TestUninstrumentable$
EOF
check "the skip file removes it, and the rest still measures" 1 'fixture/subject +1/2 arms' ./subject

echo
echo "== the module root is a package like any other =="
# Reviews 2007/2008: excluding $PWD skipped the root package entirely,
# failing tests and all, in any module that keeps code there.
reset
cat >>"$work/mod/go.mod" <<'EOF'
EOF
cat >"$work/mod/root.go" <<'EOF'
package fixture

func Greeting(loud bool) string {
	if loud {
		return "HI"
	}
	return "hi"
}
EOF
cat >"$work/mod/root_test.go" <<'EOF'
package fixture

import "testing"

func TestQuiet(t *testing.T) {
	if Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
check "a root package with an uncovered arm fails" 1 'never true'
cat >>"$work/mod/root_test.go" <<'EOF'

func TestLoud(t *testing.T) {
	if Greeting(true) != "HI" {
		t.Fatal("wrong")
	}
}
EOF
check "a root package fully covered passes" 0 'fixture +2/2 arms'

echo
echo "== a skip file of nothing but comments is not an error =="
reset
subject
cat >"$work/mod/subject/subject_test.go" <<'EOF'
package subject

import "testing"

func TestBoth(t *testing.T) {
	if Greeting(true) != "HI" || Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
cat >"$work/mod/branch-coverage-skip" <<'EOF'
# Nothing to skip yet.

EOF
check "a comments-only skip file skips nothing and does not abort" 0 'fixture/subject +2/2 arms' ./subject

echo
echo "== exits are found through the syntax tree, not through grep =="
# Reviews 2013/2014: a textual rewrite of `os.Exit(` misses every one of
# these. The first two are legitimate and must be HANDLED; the last two
# cannot be rewritten and must be REFUSED, because a missed exit loses
# the final hits behind a report that still looks complete.
harness() {
	mkdir -p "$work/mod/harness"
	cat >"$work/mod/harness/harness_test.go" <<EOF
package harness_test

import (
	$1
	"testing"

	"fixture/subject"
)

$2

func TestBoth(t *testing.T) {
	if subject.Greeting(true) != "HI" || subject.Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
}

reset
subject
harness 'stdos "os"' 'func TestMain(m *testing.M) { stdos.Exit(m.Run()) }'
check "an aliased os import in a block is handled" 0 'fixture/subject +2/2 arms' ./subject

reset
subject
mkdir -p "$work/mod/harness"
cat >"$work/mod/harness/harness_test.go" <<'EOF'
package harness_test

import stdos "os"

import "testing"

import "fixture/subject"

func TestMain(m *testing.M) { stdos.Exit(m.Run()) }

func TestBoth(t *testing.T) {
	if subject.Greeting(true) != "HI" || subject.Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
check "a single-line aliased import is handled" 0 'fixture/subject +2/2 arms' ./subject

reset
subject
harness '"os"' 'func TestMain(m *testing.M) {
	exit := os.Exit
	exit(m.Run())
}'
check "an exit taken as a function value is refused" 1 'as a value' ./subject

reset
subject
harness '"syscall"' 'func TestMain(m *testing.M) {
	syscall.Exit(m.Run())
}'
check "syscall.Exit is refused" 1 'syscall.Exit' ./subject

reset
subject
harness '. "os"' 'func TestMain(m *testing.M) { Exit(m.Run()) }'
check "a dot import of os is refused" 1 'dot-imports' ./subject

# A plain test — no TestMain at all — that exits directly. The injected
# TestMain cannot help, so the exit itself has to be rewritten.
reset
subject
mkdir -p "$work/mod/harness"
cat >"$work/mod/harness/harness_test.go" <<'EOF'
package harness_test

import (
	"os"
	"testing"

	"fixture/subject"
)

func TestBoth(t *testing.T) {
	if subject.Greeting(true) != "HI" || subject.Greeting(false) != "hi" {
		t.Error("wrong")
		os.Exit(1)
	}
}
EOF
check "os.Exit in a plain test, with no TestMain, is rewritten" 0 'fixture/subject +2/2 arms' ./subject

echo
echo "== a module that does not load is fatal, not smaller =="
# Review 2013: a go list failure used to vanish — through process
# substitution, where set -e cannot see it, and by converting a failed
# dependency listing into an empty file. Either one drops a driver, and
# the survivors then satisfy a check that never knew one was missing.
reset
subject
mkdir -p "$work/mod/broken"
cat >"$work/mod/broken/broken_test.go" <<'EOF'
package broken

import "fixture/nonexistent"

func init() { _ = nonexistent.Nothing }
EOF
check "an unloadable test package aborts the run" 2 'go list' ./subject

echo
echo "== only the test files this build compiles are rewritten =="
# Review 2007: a *_test.go glob also matches files excluded by build
# constraints. Rewriting an inactive platform's TestMain would reference
# a gobcoInnerTestMain that is never compiled, or collide with the active
# one. Both files below declare TestMain; exactly one is ever built.
reset
subject
mkdir -p "$work/mod/harness"
cat >"$work/mod/harness/harness_test.go" <<'EOF'
package harness_test

import (
	"testing"

	"fixture/subject"
)

func TestBoth(t *testing.T) {
	if subject.Greeting(true) != "HI" || subject.Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
cat >"$work/mod/harness/main_never_test.go" <<'EOF'
//go:build gobco_never

package harness_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }
EOF
cat >"$work/mod/harness/main_always_test.go" <<'EOF'
//go:build !gobco_never

package harness_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }
EOF
check "a build-excluded TestMain is left alone" 0 'fixture/subject +2/2 arms' ./subject

echo
echo "== export_test.go hooks are visible to the type checker =="
reset
subject
cat >"$work/mod/subject/export_test.go" <<'EOF'
package subject

func LoudForTest() string { return Greeting(true) }
EOF
cat >"$work/mod/subject/subject_x_test.go" <<'EOF'
package subject_test

import (
	"testing"

	"fixture/subject"
)

func TestThroughHook(t *testing.T) {
	if subject.LoudForTest() != "HI" || subject.Greeting(false) != "hi" {
		t.Fatal("wrong")
	}
}
EOF
check "an external test package reaching an export_test hook works" 0 'fixture/subject +2/2 arms' ./subject

echo
printf '%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
