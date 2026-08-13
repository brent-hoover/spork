#!/usr/bin/env bash
# Per-arm condition coverage for a multi-package Go module.
#
# gobco measures whether each conditional arm — the true side AND the
# false side of every atomic condition — was actually evaluated. Go's own
# cover tool cannot: it counts STATEMENTS, so `if a && b` scores covered
# the first time it is reached, whichever way it went.
#
# Three things make gobco work on a module like this one, none of them
# obvious, all three established by measurement on 2026-08-11:
#
#  1. gobco takes DIRECTORIES, not import paths. It os.Open()s its
#     argument, so `gobco sutra/internal/api` panics with "no such file
#     or directory" — which reads exactly like a tool that cannot handle
#     multiple packages, and is really a path that does not exist.
#     `./internal/api` works. `./...` reports "nothing to instrument"
#     because the module root holds no .go files of its own.
#
#  2. A symbol used by a package's EXTERNAL test package must not live
#     only in an in-package test file. gobco type-checks the `foo` and
#     `foo_test` packages separately, resolving the import through the
#     source importer, which loads non-test files only. The classic
#     export_test.go hook is therefore undefined at that point and
#     aborts the whole package.
#
#     This script works around that in a THROWAWAY COPY of the module,
#     renaming export_test.go to a plain .go file so the source importer
#     can see it. Moving those hooks into production code would have
#     worked too, and was wrong: `deadcode` run without -test then
#     reports them as unreachable from main, which is the honest verdict
#     for a setter no production path calls. A measurement workaround
#     belongs in the measuring tool, not in the code being measured.
#     The constraint this places on export_test.go is that it holds
#     accessors and no conditional logic — renaming makes gobco treat it
#     as production code, so any condition in it would be counted as an
#     arm to cover. Both current files satisfy that (api's arm total is
#     2460 either way).
#
#  3. Attribution must cross package boundaries, because the suite that
#     exercises these packages is the acceptance suite, driving them
#     end-to-end over HTTP. gobco only persists counters from a TestMain
#     it injects into the instrumented package, so a foreign test binary
#     records nothing on exit. gobco's -immediately flag persists at
#     every check point instead, which does work but rewrites the entire
#     stats file per condition evaluation: fine for identity's 19
#     conditions, and api (1230) produced no result in 900s. This script
#     injects a flush ticker into the STAGED copy instead, which gets
#     the same numbers at the same speed as the suite alone (identity:
#     22/38 arms either way, 18.1s vs 18.4s vs a 16.9s baseline).
#
#     The ticker alone loses every hit since the last tick, because a
#     test binary ends in os.Exit — Go's generated main calls it even
#     when the package declares no TestMain, past any deferred code.
#     So each foreign driver is also made to persist SYNCHRONOUSLY at
#     exit, through gobco's own GobcoFinish: an injected TestMain when
#     the driver has none, or a rewrite of its os.Exit calls when it
#     does. A driver whose TestMain returns instead of exiting cannot be
#     hooked either way and FAILS the package rather than measuring less
#     than it reports.
#
# The measured spread is the reason cross-package attribution matters:
# identity's own unit tests reach 13/38 arms, the acceptance suite 22/38.
#
# Usage: scripts/branch-coverage.sh [package-dir ...]
# With no arguments, every package in the module.
set -euo pipefail

cd "$(dirname "$0")/.."
GOBCO=github.com/rillig/gobco@v1.3.4

work=$(mktemp -d)
roots=$work/roots
: >"$roots"
cleanup() {
	while read -r r; do [ -n "$r" ] && rm -rf "$r"; done <"$roots"
	rm -rf "$work"
}
# Registered before anything is staged, so a failure mid-copy still
# takes its temporary directories with it.
trap cleanup EXIT

# Everything below runs against a copy, never the real tree.
cp -a . "$work/mod"
rm -rf "$work/mod/scripts"
for hook in "$work"/mod/*/*/export_test.go "$work"/mod/*/export_test.go; do
	[ -f "$hook" ] || continue
	mv "$hook" "${hook%export_test.go}export_gobco.go"
done
cd "$work/mod"

# Dependency closure per test binary, so each instrumented package is
# driven only by the test packages that actually link it. A package with
# no test files of its own builds no test binary and is not a driver.
targets=()
while read -r importpath; do
	targets+=("$importpath")
	go list -deps -test "$importpath" >"$work/deps-$(echo "$importpath" | tr / _)" 2>/dev/null || : >"$work/deps-$(echo "$importpath" | tr / _)"
done < <(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)

# Tests that measure the BUILD rather than behavior cannot pass against
# an instrumented build. Each entry needs its reason; a skipped test's
# arms simply go uncovered, so this list can only ever UNDERSTATE
# coverage — never inflate it.
#   TestScanDuplicateKeysStreamsValues asserts a 1 MiB allocation
#   ceiling while scanning a 64 MiB value. gobco's per-condition
#   counters in the byte lexer push it to ~3 MiB — still ~20x under the
#   ~64 MiB a value-materializing tokenizer would cost, which is the
#   failure mode the guard exists to catch — so the property holds under
#   instrumentation and only the slack does not.
skiptests='^TestScanDuplicateKeysStreamsValues$'

# A package with no production .go files has no conditions to
# instrument; gobco's own generated file becomes the package's only
# non-test source and the directory stops type-checking as one package.
if [ "$#" -gt 0 ]; then
	subjects=("$@")
else
	subjects=()
	while read -r dir; do subjects+=("./${dir#"$PWD"/}"); done < <(go list -f '{{if .GoFiles}}{{.Dir}}{{end}}' ./... | grep -v "^$PWD$")
fi

failed=0
for subject in "${subjects[@]}"; do
	importpath=$(go list -f '{{.ImportPath}}' "$subject")
	pkgname=$(go list -f '{{.Name}}' "$subject")
	rel=${subject#./}
	slug=$(echo "$importpath" | tr / _)

	if [ -z "$(go list -f '{{if .GoFiles}}y{{end}}' "$subject")" ]; then
		printf '%-28s no production code\n' "$importpath"
		continue
	fi

	# Instrument only. -run=^$ skips gobco's own test run (4s instead of
	# minutes); the package's own tests run below as one of the drivers,
	# where their contribution is recorded like any other.
	out=$work/instr-$slug.log
	go run "$GOBCO" -keep -test '-run=^$' "$subject" >"$out" 2>&1 || {
		echo "FAIL $importpath: instrumentation aborted"
		sed -n '1,5p' "$out"
		failed=1
		continue
	}
	root=$(sed -n 's/^gobco: the temporary files are in //p' "$out")
	echo "$root" >>"$roots"
	module=$(find "$root" -maxdepth 1 -type d -name 'module-*' | head -1)

	# gobco reports the arm total even with every test skipped, which
	# makes it the authoritative denominator: the merged stats below must
	# account for exactly this many arms. Missing or partial stats then
	# fail the package instead of shrinking the thing being measured.
	expected=$(sed -n 's/^Condition coverage: [0-9]*\/\([0-9]*\)$/\1/p' "$out" | tail -1)
	if [ -z "$expected" ]; then
		echo "FAIL $importpath: gobco printed no coverage summary"
		failed=1
		continue
	fi

	# A package with no conditions has nothing to persist, so it gets no
	# flush machinery — but its drivers still run below. Skipping them
	# would let a package with no branches hide its failing tests.
	flush=1
	[ "$expected" = "0" ] && flush=0

	if [ "$flush" = 1 ]; then
		cat >"$module/$rel/gobco_flush.go" <<EOF
package $pkgname

import "time"

func init() {
	go func() {
		for range time.NewTicker(200 * time.Millisecond).C {
			gobcoCounts.persist()
		}
	}()
}
EOF
	fi

	drivers=0
	for t in "${targets[@]}"; do
		grep -qxF "$importpath" "$work/deps-$(echo "$t" | tr / _)" || continue
		drivers=$((drivers + 1))
		tdir=$(cd "$module" && go list -f '{{.Dir}}' "$t")

		# Teach this driver to persist synchronously at exit. The
		# subject's own test binary needs no hook: gobco injects a
		# TestMain there that already persists, and importing the
		# subject from inside itself would not compile.
		if [ "$flush" = 1 ] && [ "$t" != "$importpath" ]; then
			mainholder=$(grep -l 'func TestMain(' "$tdir"/*_test.go 2>/dev/null | head -1 || true)
			if [ -z "$mainholder" ]; then
				# No TestMain: supply one. Go's generated main
				# would otherwise exit without flushing.
				driverpkg=$(sed -n 's/^package \([A-Za-z0-9_]*\).*/\1/p' "$(ls "$tdir"/*_test.go | head -1)" | head -1)
				cat >"$tdir/gobco_main_test.go" <<EOF
package $driverpkg

import (
	"os"
	"testing"

	gobcosubject "$importpath"
)

// TestMain persists the instrumented package's counters before the
// process goes away. GobcoFinish returns the code it was handed.
func TestMain(m *testing.M) { os.Exit(gobcosubject.GobcoFinish(m.Run())) }
EOF
			elif grep -q 'os\.Exit(' "$mainholder"; then
				# Has a TestMain that exits: reroute every
				# os.Exit in this driver's test files, then
				# define the replacement. Written after the
				# rewrite so its own os.Exit survives.
				# The trailing reference keeps "os" used: a file
				# that imported it only to exit would otherwise
				# stop compiling.
				driverpkg=$(sed -n 's/^package \([A-Za-z0-9_]*\).*/\1/p' "$mainholder" | head -1)
				while read -r f; do
					perl -pi -e 's/os\.Exit\(/gobcoExit(/g' "$f"
					printf '\nvar _ = os.Exit\n' >>"$f"
				done < <(grep -l 'os\.Exit(' "$tdir"/*_test.go)
				cat >"$tdir/gobco_exit_test.go" <<EOF
package $driverpkg

import (
	"os"

	gobcosubject "$importpath"
)

// gobcoExit persists the instrumented package's counters before the
// process goes away. GobcoFinish returns the code it was handed.
func gobcoExit(code int) { os.Exit(gobcosubject.GobcoFinish(code)) }
EOF
			else
				echo "FAIL $importpath: driver $t declares a TestMain that returns instead of calling os.Exit; its counters cannot be flushed synchronously"
				failed=1
				continue 2
			fi
		fi

		stats=$work/stats-$slug-$(echo "$t" | tr / _).json
		if ! (cd "$module" && GOBCO_STATS="$stats" go test -count=1 -skip "$skiptests" "$t" \
			>"$work/driver-$slug.log" 2>&1); then
			echo "FAIL $importpath: driver $t did not pass"
			sed -n '1,10p' "$work/driver-$slug.log"
			failed=1
			continue 2
		fi

		# A driver that passed but wrote nothing contributed nothing,
		# and the merge below could not tell: every other driver's
		# report already carries the complete condition list, so the
		# denominator check would still balance.
		if [ "$flush" = 1 ] && ! jq -e 'length > 0' "$stats" >/dev/null 2>&1; then
			echo "FAIL $importpath: driver $t passed but wrote no coverage data"
			failed=1
			continue 2
		fi
	done

	if [ "$flush" = 0 ]; then
		printf '%-28s no conditions  (%s drivers)\n' "$importpath" "$drivers"
		continue
	fi

	# Sum every driver's counts per condition; an arm is covered when
	# some driver evaluated it at least once.
	reports=$(ls "$work"/stats-"$slug"-*.json 2>/dev/null | wc -l | tr -d ' ')
	if [ "$reports" != "$drivers" ]; then
		echo "FAIL $importpath: $reports coverage reports from $drivers drivers"
		failed=1
		continue
	fi
	if ! report=$(jq -se '
		add
		| group_by(.Start)
		| map({Start: .[0].Start, Code: .[0].Code,
		       t: map(.TrueCount) | add, f: map(.FalseCount) | add})
	' "$work"/stats-"$slug"-*.json 2>/dev/null); then
		echo "FAIL $importpath: no usable coverage data from $drivers drivers"
		failed=1
		continue
	fi

	total=$(echo "$report" | jq 'length * 2')
	if [ "$total" != "$expected" ]; then
		echo "FAIL $importpath: stats cover $total arms, gobco instrumented $expected"
		failed=1
		continue
	fi
	covered=$(echo "$report" | jq '[.[] | (if .t > 0 then 1 else 0 end) + (if .f > 0 then 1 else 0 end)] | add // 0')
	printf '%-28s %s/%s arms  (%s drivers)\n' "$importpath" "$covered" "$total" "$drivers"
	echo "$report" | jq -r '.[] | select(.t == 0 or .f == 0)
		| "    \(.Start): \(.Code) never \(if .t == 0 and .f == 0 then "evaluated" elif .t == 0 then "true" else "false" end)"'
	[ "$covered" = "$total" ] || failed=1
done

exit "$failed"
