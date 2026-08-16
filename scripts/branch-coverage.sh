#!/usr/bin/env bash
# Per-arm condition coverage for a multi-package Go module.
#
# SHARED BY EVERY GO APP IN THIS REPO. It takes no knowledge of any one
# of them: it measures the module rooted at the CURRENT DIRECTORY, so an
# app's coverage gate is `../scripts/branch-coverage.sh` run from the
# app's own directory. The one thing an app may need to say for itself —
# which of its tests cannot survive instrumentation — it says in its own
# branch-coverage-skip file (see SKIP FILE below), not in here.
#
# gobco measures whether each conditional arm — the true side AND the
# false side of every atomic condition — was actually evaluated. Go's own
# cover tool cannot: it counts STATEMENTS, so `if a && b` scores covered
# the first time it is reached, whichever way it went.
#
# Three things make gobco work on a module like these, none of them
# obvious, all three established by measurement on 2026-08-11:
#
#  1. gobco takes DIRECTORIES, not import paths. It os.Open()s its
#     argument, so `gobco sutra/internal/api` panics with "no such file
#     or directory" — which reads exactly like a tool that cannot handle
#     multiple packages, and is really a path that does not exist.
#     `./internal/api` works. `./...` reports "nothing to instrument"
#     because a module root holds no .go files of its own.
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
#     arm to cover.
#
#  3. Attribution must cross package boundaries, because the suites that
#     exercise these packages are acceptance suites, driving them
#     end-to-end over HTTP. gobco only persists counters from a TestMain
#     it injects into the instrumented package, so a foreign test binary
#     records nothing on exit. gobco's -immediately flag persists at
#     every check point instead, which does work but rewrites the entire
#     stats file per condition evaluation: fine for a package with 19
#     conditions, and one with 1230 produced no result in 900s. This
#     script injects a flush ticker into the STAGED copy instead, which
#     gets the same numbers at the same speed as the suite alone
#     (measured: 22/38 arms either way, 18.1s vs 18.4s vs a 16.9s
#     baseline).
#
#     The ticker alone loses every hit since the last tick, because a
#     test binary ends in os.Exit — Go's generated main calls it even
#     when the package declares no TestMain, past any deferred code. So
#     each foreign driver is also made to persist SYNCHRONOUSLY, through
#     gobco's own GobcoFinish, on BOTH ways out: its os.Exit calls are
#     rewritten, and its TestMain is wrapped so a plain return flushes
#     too. Finding one os.Exit does not prove every path takes it.
#
# The measured spread is the reason cross-package attribution matters:
# one package's own unit tests reached 13/38 arms, the acceptance suite
# 22/38.
#
# WHAT MAKES IT A GATE. Every way of measuring less than it reports is a
# failure, not a smaller number: gobco's own arm total is the
# authoritative denominator, a driver that fails or writes no usable
# report fails the package, every driver's report must carry the
# complete condition set, and the report count must equal the driver
# count. scripts/branch-coverage-test.sh exercises each of those.
#
# SKIP FILE. A module may place a `branch-coverage-skip` file beside its
# go.mod holding one Go test-name regex per line, `#` comments and blank
# lines ignored. It is for tests that measure the BUILD rather than
# behavior and so cannot pass against an instrumented one — an
# allocation ceiling, a timing bound. Every entry needs its reason in a
# comment. A skipped test's arms simply go uncovered, so the file can
# only ever UNDERSTATE coverage, never inflate it.
#
# Usage: ../scripts/branch-coverage.sh [package-dir ...]
# Run from the module root. With no arguments, every package in it.
set -euo pipefail

GOBCO=github.com/rillig/gobco@v1.3.4

if [ ! -f go.mod ]; then
	echo "branch-coverage: no go.mod in $PWD — run this from a module root" >&2
	exit 2
fi
module_root=$PWD

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

skiptests=
if [ -f branch-coverage-skip ]; then
	skiptests=$(sed 's/#.*//' branch-coverage-skip | tr -d '[:space:]' | grep -v '^$' | paste -sd '|' -)
fi

# Everything below runs against a copy, never the real tree.
cp -a "$module_root" "$work/mod"
while read -r hook; do
	mv "$hook" "${hook%export_test.go}export_gobco.go"
done < <(find "$work/mod" -name export_test.go -type f)
cd "$work/mod"

# Dependency closure per test binary, so each instrumented package is
# driven only by the test packages that actually link it. A package with
# no test files of its own builds no test binary and is not a driver.
targets=()
while read -r importpath; do
	targets+=("$importpath")
	go list -deps -test "$importpath" >"$work/deps-$(echo "$importpath" | tr / _)" 2>/dev/null || : >"$work/deps-$(echo "$importpath" | tr / _)"
done < <(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)

# A package with no production .go files has no conditions to
# instrument; gobco's generated file would become the directory's only
# non-test source and it would stop type-checking as one package.
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
		printf '%-32s no production code\n' "$importpath"
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
	conditions=$((expected / 2))

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

		# Teach this driver to persist synchronously however it ends. The
		# subject's own test binary needs no hook: gobco injects a
		# TestMain there that already persists, and importing the
		# subject from inside itself would not compile.
		if [ "$flush" = 1 ] && [ "$t" != "$importpath" ]; then
			mainholder=$(grep -l 'func TestMain(' "$tdir"/*_test.go 2>/dev/null | head -1 || true)
			if [ -z "$mainholder" ]; then
				# No TestMain: supply one. Go's generated main would
				# otherwise exit without flushing.
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
			else
				# Has a TestMain: cover BOTH ways out of it. Its
				# os.Exit calls are rewritten to flush first, and it is
				# renamed and wrapped so a plain return — or a
				# conditional path that never reaches an os.Exit —
				# flushes through the deferred call. Neither the exit
				# code nor the generated main's handling of it changes.
				# The trailing reference keeps "os" used: a file that
				# imported it only to exit would otherwise stop
				# compiling.
				driverpkg=$(sed -n 's/^package \([A-Za-z0-9_]*\).*/\1/p' "$mainholder" | head -1)
				while read -r f; do
					perl -pi -e 's/os\.Exit\(/gobcoExit(/g' "$f"
					printf '\nvar _ = os.Exit\n' >>"$f"
				done < <(grep -l 'os\.Exit(' "$tdir"/*_test.go || true)
				perl -pi -e 's/\bfunc TestMain\(/func gobcoInnerTestMain(/' "$mainholder"
				cat >"$tdir/gobco_exit_test.go" <<EOF
package $driverpkg

import (
	"os"
	"testing"

	gobcosubject "$importpath"
)

// gobcoExit persists the instrumented package's counters before the
// process goes away. GobcoFinish returns the code it was handed.
func gobcoExit(code int) { os.Exit(gobcosubject.GobcoFinish(code)) }

// TestMain covers the other way out: a TestMain that RETURNS, on any
// path, never reaches an os.Exit and would otherwise lose every hit
// since the last tick. The deferred flush costs nothing on the exit
// path, where it does not run at all.
func TestMain(m *testing.M) {
	defer gobcosubject.GobcoFinish(0)
	gobcoInnerTestMain(m)
}
EOF
			fi
		fi

		stats=$work/stats-$slug-$(echo "$t" | tr / _).json
		if ! (cd "$module" && GOBCO_STATS="$stats" go test -count=1 ${skiptests:+-skip "$skiptests"} "$t" \
			>"$work/driver-$slug.log" 2>&1); then
			echo "FAIL $importpath: driver $t did not pass"
			sed -n '1,10p' "$work/driver-$slug.log"
			failed=1
			continue 2
		fi

		# A driver that passed but reported nothing — or reported a
		# PARTIAL condition set — contributed silently less than it
		# appears to, and the merge below could not tell: every other
		# driver's report already carries the complete list, so the
		# denominator would still balance. Each report must therefore
		# carry every condition exactly once, with usable counts.
		if [ "$flush" = 1 ] && ! jq -e --argjson n "$conditions" '
			type == "array" and length == $n
			and ([.[] | select((.Start | type) == "string"
			                   and (.TrueCount | type) == "number"
			                   and (.FalseCount | type) == "number")] | length) == $n
			and ((map(.Start) | unique | length) == $n)
		' "$stats" >/dev/null 2>&1; then
			echo "FAIL $importpath: driver $t passed but reported no usable coverage for all $conditions conditions"
			failed=1
			continue 2
		fi
	done

	if [ "$flush" = 0 ]; then
		printf '%-32s no conditions  (%s drivers)\n' "$importpath" "$drivers"
		continue
	fi

	reports=$(find "$work" -maxdepth 1 -name "stats-$slug-*.json" | wc -l | tr -d ' ')
	if [ "$reports" != "$drivers" ]; then
		echo "FAIL $importpath: $reports coverage reports from $drivers drivers"
		failed=1
		continue
	fi

	# Sum every driver's counts per condition; an arm is covered when
	# some driver evaluated it at least once.
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
	printf '%-32s %s/%s arms  (%s drivers)\n' "$importpath" "$covered" "$total" "$drivers"
	echo "$report" | jq -r '.[] | select(.t == 0 or .f == 0)
		| "    \(.Start): \(.Code) never \(if .t == 0 and .f == 0 then "evaluated" elif .t == 0 then "true" else "false" end)"'
	[ "$covered" = "$total" ] || failed=1
done

exit "$failed"
