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
