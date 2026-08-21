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
#     That rewriting is done by gobco-hook.go, on the SYNTAX TREE. Doing
#     it with grep and perl was wrong in five ways at once — an aliased
#     import on one line or in a block, a dot import, an exit reached
#     through a function value, syscall.Exit — and every one of them
#     fails SILENTLY, because the ticker has already written a report
#     carrying the complete condition list, so a stale result passes
#     every check below. What cannot be rewritten is refused instead.
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
# Resolved before the staging cd, because it lives beside this script and
# not inside whichever module is being measured.
hookprog=$(cd "$(dirname "$0")" && pwd)/gobco-hook.go
if [ ! -f "$hookprog" ]; then
	echo "branch-coverage: $hookprog is missing; drivers cannot be hooked" >&2
	exit 2
fi

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

# awk, not a grep pipeline: a skip file holding nothing but the comments
# it is documented to allow makes grep exit 1, and under `set -e` that
# would abort the gate instead of skipping nothing.
skiptests=
if [ -f branch-coverage-skip ]; then
	skiptests=$(awk '{sub(/#.*/, ""); gsub(/[[:space:]]/, "")} $0 != "" {printf "%s%s", sep, $0; sep="|"}' branch-coverage-skip)
fi

# Everything below runs against a copy, never the real tree.
cp -a "$module_root" "$work/mod"
while read -r hook; do
	mv "$hook" "${hook%export_test.go}export_gobco.go"
done < <(find "$work/mod" -name export_test.go -type f)
cd "$work/mod"

# Every `go list` below is materialised and its status checked BEFORE its
# output is consumed. Reading one through process substitution hides its
# failure from `set -e`, and a package that vanishes from either list is a
# package the gate silently stops measuring — which is the whole failure
# class this script exists to refuse.
# list <dest> <dir> <go list args...> — the directory is explicit because
# once a subject is instrumented the questions are about the STAGED copy,
# not the tree the gate was invoked from.
list() {
	local dest=$1 dir=$2
	shift 2
	if ! (cd "$dir" && go list "$@") >"$dest" 2>"$work/list.err"; then
		echo "branch-coverage: go list $* in $dir failed — the module does not load, so nothing can be measured" >&2
		sed -n '1,10p' "$work/list.err" >&2
		exit 2
	fi
}

# deps writes the import paths a test binary links, one per line, from
# `go list -json` — which is parsed as JSON and never split on a delimiter.
# There is no separator that a directory cannot contain (review 2062 is
# right that unit separator and newline are both legal in a Unix path), so
# the answer is not a better separator but no separator at all.
#
# Two different things share the "X [Y.test]" shape. `pkg [other.test]` is a
# REAL package rebuilt for another package's test binary, and the bare path
# is the only form it takes when the subject transitively depends on the
# package under test — matching only the bare path lost such drivers
# entirely. But `pkg_test [pkg.test]` MAY be the SYNTHETIC external test
# package, which nobody can import, and treating it as real invents a
# dependency on a package of that name (kriya review 2033).
#
# They are told apart by DIRECTORY: the synthetic package lives in the
# tested package's own directory, a real sibling in its own. Deciding on
# the name alone drops a genuine dependency (review 2054).
# The first argument selects whether TEST dependencies count. They do for a
# driver, whose whole point is what its test binary links. They must NOT for
# the subject's own closure, which exists to answer "would a helper importing
# the subject cycle?" — a question about PRODUCTION imports, since an
# imported package never brings its own tests. Folding both into one -test
# call falsely rejected a valid reciprocal arrangement where the subject's
# tests import the driver (reviews 2066/2067).
deps() {
	local mode=$1 dest=$2 dir=$3
	shift 3
	# Two explicit branches rather than an array of flags: stock macOS
	# ships Bash 3.2, where expanding an EMPTY array under `set -u` is an
	# unbound-variable error and would abort the gate outright (review
	# 2070).
	local st=0
	if [ "$mode" = withtests ]; then
		(cd "$dir" && go list -deps -test -json "$@") >"$work/deps.json" 2>"$work/list.err" || st=$?
	else
		(cd "$dir" && go list -deps -json "$@") >"$work/deps.json" 2>"$work/list.err" || st=$?
	fi
	if [ "$st" != 0 ]; then
		echo "branch-coverage: go list -deps $* in $dir failed — the module does not load, so nothing can be measured" >&2
		sed -n '1,10p' "$work/list.err" >&2
		exit 2
	fi
	jq -rs '
		(map(select(.ForTest == null) | {key: .ImportPath, value: .Dir}) | from_entries) as $dirs
		| map(
			((.ImportPath | sub(" \\[.*\\]$"; "")) as $base
			| select((.ForTest // "") == "" or $base != (.ForTest + "_test") or .Dir != ($dirs[.ForTest] // "\u0000"))
			| $base)
		)
		| unique | .[]
	' "$work/deps.json" >"$dest"
}


# Dependency closure per test binary, so each instrumented package is
# driven only by the test packages that actually link it. A package with
# no test files of its own builds no test binary and is not a driver.
list "$work/drivers" "$PWD" -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...
targets=()
while read -r importpath; do
	[ -n "$importpath" ] || continue
	targets+=("$importpath")
	deps withtests "$work/deps-$(echo "$importpath" | tr / _)" "$PWD" "$importpath"
done <"$work/drivers"

# A package with no production .go files has no conditions to
# instrument; gobco's generated file would become the directory's only
# non-test source and it would stop type-checking as one package.
if [ "$#" -gt 0 ]; then
	subjects=("$@")
else
	subjects=()
	list "$work/subjects" "$PWD" -f '{{if .GoFiles}}{{.Dir}}{{end}}' ./...
	while read -r dir; do
		[ -n "$dir" ] || continue
		# The module root is a package like any other. Excluding it —
		# harmless in a module whose root holds no .go files, as it was
		# when this script served one app — would silently skip an
		# entire package, failing tests and all, in any module laid out
		# the other way.
		sub=${dir#"$PWD"}
		sub=${sub#/}
		if [ -z "$sub" ]; then subjects+=("."); else subjects+=("./$sub"); fi
	done <"$work/subjects"
fi

failed=0
# Module-wide totals, so a FLOOR can be judged on the whole rather than
# package by package. A per-package 100% rule and a module floor answer
# different questions: the first asks "is every arm reached", the second
# "has coverage gone backwards".
sum_covered=0
sum_total=0
measured=0
# ${arr[@]+...} throughout: stock macOS Bash 3.2 treats an EMPTY array as
# unset under `set -u`, so a module with no packages, no test binaries, or a
# driver with no active test files would abort the gate rather than report
# on it (review 2070).
# Nothing to measure is not success. Everything else here refuses to report
# a number it did not earn; reporting NO number and exiting 0 is the same
# fault with the volume turned down (review 2075's fixture found this).
if [ "${#subjects[@]}" = 0 ]; then
	echo "branch-coverage: no package in $module_root has production code to instrument" >&2
	exit 2
fi

for subject in ${subjects[@]+"${subjects[@]}"}; do
	importpath=$(go list -f '{{.ImportPath}}' "$subject")
	pkgname=$(go list -f '{{.Name}}' "$subject")
	rel=${subject#./}
	[ "$rel" = "." ] && rel=
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
		cat >"$module${rel:+/$rel}/gobco_flush.go" <<EOF
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

	# The subject's own dependency closure, for the cycle check below.
	deps productiononly "$work/subjdeps" "$module" "$importpath"

	drivers=0
	for t in ${targets[@]+"${targets[@]}"}; do
		grep -qxF "$importpath" "$work/deps-$(echo "$t" | tr / _)" || continue
		drivers=$((drivers + 1))
		tdir=$(cd "$module" && go list -f '{{.Dir}}' "$t")
		# The driver's PRODUCTION package name. "external test package"
		# means exactly <name>_test and nothing else: a production
		# package may itself be named something_test, and treating it as
		# external would skip the cycle check below and emit a helper
		# that cannot compile (review 2038).
		tname=$(cd "$module" && go list -f '{{.Name}}' "$t")

		# Teach this driver to persist synchronously however it ends. The
		# subject's own test binary needs no hook: gobco injects a
		# TestMain there that already persists, and importing the
		# subject from inside itself would not compile.
		if [ "$flush" = 1 ] && [ "$t" != "$importpath" ]; then
			# Only the files this build actually compiles. A *_test.go
			# glob also matches files excluded by build constraints, and
			# rewriting an inactive platform's TestMain would either
			# reference a gobcoInnerTestMain that is never compiled or
			# collide with the one that is.
			list "$work/testfiles" "$module" -f "{{range .TestGoFiles}}{{\$.Dir}}/{{.}}
{{end}}{{range .XTestGoFiles}}{{\$.Dir}}/{{.}}
{{end}}" "$t"
			active=()
			while read -r f; do [ -n "$f" ] && active+=("$f"); done <"$work/testfiles"
			if [ "${#active[@]}" = 0 ]; then
				echo "FAIL $importpath: driver $t compiles no test files"
				failed=1
				continue 2
			fi

			# The rewrite is done on the SYNTAX TREE, not with grep and
			# perl. Reviews 2013 and 2014 were right that a textual pass
			# cannot see an aliased import (`stdos "os"`, on one line or
			# in a block), a dot import, an exit reached through a
			# function value (`exit := os.Exit`), or syscall.Exit — and
			# each of those ends the process with no flush while the
			# ticker has already written a full-shaped report, so the
			# stale result passes every check made below. gobco-hook.go
			# rewrites every os.Exit call under whatever name os carries,
			# renames a TestMain so it can be wrapped, and REFUSES
			# anything it cannot prove safe. (log.Fatal is deliberately
			# allowed: it fires only on a path that fails the driver, and
			# a failed driver already fails the package.)
			if ! hook=$(go run "$hookprog" "$work/testfiles" 2>"$work/hook.err"); then
				echo "FAIL $importpath: driver $t cannot be hooked to flush its counters"
				sed -n '1,5p' "$work/hook.err"
				failed=1
				continue 2
			fi
			# One helper per PACKAGE, because a test directory can hold
			# both `foo` and `foo_test` and a helper emitted into one is
			# invisible to the other — so a rewritten exit over there
			# would not compile, and a wrapper placed on the wrong side
			# could not see gobcoInnerTestMain (review 2019/2020).
			# Where an injected TestMain should go, when no package
			# declares one. The EXTERNAL package is preferred, because the
			# injected helper imports the instrumented subject and the
			# subject may legitimately import the driver's production
			# package — an arrangement that works precisely because only
			# the external tests reach the subject. Injecting into the
			# internal package would close that into an import cycle
			# (reviews 2025/2026).
			anymain=no
			injectinto=
			while read -r pkgname hasmain _; do
				[ -n "$pkgname" ] || continue
				[ "$hasmain" = testmain ] && anymain=yes
				if [ "$pkgname" = "${tname}_test" ]; then
					# Preferred — unless a helper cannot live there,
					# because the external test package is built under
					# the subject's own import path. Then the internal
					# package is tried instead, which is legal whenever
					# the subject does not import the driver (review
					# 2058); if that cycles too, the emission loop below
					# refuses and says which way it failed.
					[ "$importpath" = "${t}_test" ] || injectinto=$pkgname
				elif [ -z "$injectinto" ] || [ "$injectinto" = "${tname}_test" ]; then
					[ -n "$injectinto" ] && [ "$importpath" != "${t}_test" ] || injectinto=$pkgname
				fi
			done <<<"$hook"

			if [ "$anymain" = no ] && [ -z "$injectinto" ]; then
				echo "FAIL $importpath: driver $t declares no TestMain and no test package of it can hold one — its counters would rest on the ticker alone"
				failed=1
				continue 2
			fi

			while read -r pkgname hasmain hasexits; do
				[ -n "$pkgname" ] || continue
				needexit=no
				needmain=no
				[ "$hasexits" = exits ] && needexit=yes
				if [ "$hasmain" = testmain ]; then
					needmain=wrap
				elif [ "$anymain" = no ] && [ "$pkgname" = "$injectinto" ]; then
					# Nobody declares TestMain, so one is supplied. Go's
					# generated main would otherwise exit without flushing.
					needmain=inject
					needexit=yes
				fi
				[ "$needexit" = no ] && [ "$needmain" = no ] && continue

				# A helper must import the subject to reach GobcoFinish.
				# For an INTERNAL test package that is impossible when the
				# subject imports the package under test: Go refuses the
				# cycle, and no flush can be installed there. Preferring
				# the external package covers only the injected case
				# (review 2032) — an internal TestMain, or an os.Exit in
				# an internal test file, still needs one. Refuse loudly
				# rather than measure this package without it.
				# Go builds a driver's external test package under the
				# import path <driver>_test. If the SUBJECT's path is
				# exactly that, the helper's `import "<subject>"` reads
				# as the package importing itself and Go refuses it as a
				# cycle — a real collision between a synthetic package
				# name and a real one, which no placement resolves. Say
				# so plainly rather than leave Go's message to explain a
				# situation this gate created (review 2054).
				if [ "$pkgname" = "${tname}_test" ] && [ "$importpath" = "${t}_test" ]; then
					echo "FAIL $importpath: driver $t's external test package is built as $importpath, the subject's own path — a flush helper there would import itself, so these counters cannot be persisted"
					failed=1
					continue 3
				fi

				if [ "$pkgname" != "${tname}_test" ]; then
					if grep -qxF "$t" "$work/subjdeps"; then
						echo "FAIL $importpath: driver $t needs a flush helper in its INTERNAL test package, but $importpath imports $t — the helper's import of the subject would be a cycle, so these counters cannot be persisted"
						failed=1
						continue 3
					fi
				fi

				helper=$tdir/gobco_${pkgname}_test.go
				{
					echo "package $pkgname"
					echo
					echo "import ("
					[ "$needexit" = yes ] && echo '	"os"'
					[ "$needmain" != no ] && echo '	"testing"'
					echo
					echo "	gobcosubject \"$importpath\""
					echo ")"
				} >"$helper"
				if [ "$needexit" = yes ]; then
					cat >>"$helper" <<EOF

// gobcoExit persists the instrumented package's counters before the
// process goes away. GobcoFinish returns the code it was handed.
func gobcoExit(code int) { os.Exit(gobcosubject.GobcoFinish(code)) }
EOF
				fi
				case $needmain in
				inject)
					cat >>"$helper" <<EOF

// TestMain persists on the way out of the generated main.
func TestMain(m *testing.M) { gobcoExit(m.Run()) }
EOF
					;;
				wrap)
					# Covers the other way out: a TestMain that RETURNS, on
					# any path, never reaches an exit and would otherwise
					# lose every hit since the last tick. The deferred flush
					# costs nothing on the exit path, where it does not run.
					cat >>"$helper" <<EOF

func TestMain(m *testing.M) {
	defer gobcosubject.GobcoFinish(0)
	gobcoInnerTestMain(m)
}
EOF
					;;
				esac
			done <<<"$hook"
			unset pkgname hasmain hasexits
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
		measured=$((measured + 1))
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
	measured=$((measured + 1))
	covered=$(echo "$report" | jq '[.[] | (if .t > 0 then 1 else 0 end) + (if .f > 0 then 1 else 0 end)] | add // 0')
	printf '%-32s %s/%s arms  (%s drivers)\n' "$importpath" "$covered" "$total" "$drivers"
	echo "$report" | jq -r '.[] | select(.t == 0 or .f == 0)
		| "    \(.Start): \(.Code) never \(if .t == 0 and .f == 0 then "evaluated" elif .t == 0 then "true" else "false" end)"'
	sum_covered=$((sum_covered + covered))
	sum_total=$((sum_total + total))
	# Under a floor, a package below 100% is INFORMATION, not a failure —
	# the arms are still listed above. Without one, every arm is required,
	# which is what the residue work needs.
	if [ -z "${BRANCH_COVERAGE_FLOOR:-}" ]; then
		[ "$covered" = "$total" ] || failed=1
	fi
done

# The same invariant as the discovery check above, but it has to hold for
# EXPLICIT arguments too: naming only test-only packages left the loop with
# nothing to do and still exited 0 (review 2079). Measuring nothing is not
# success, however the subjects were chosen.
#
# Guarded on $failed as well, because a package that FAILED was attempted —
# the gate did its job and said so, and that is an ordinary failure (1), not
# "there was nothing here" (2).
if [ "$measured" = 0 ] && [ "$failed" = 0 ]; then
	echo "branch-coverage: none of the named packages had anything to instrument" >&2
	exit 2
fi

# The total is always REPORTED, so the number is visible whether or not it
# is being enforced.
if [ "$sum_total" -gt 0 ]; then
	pct=$(echo "$sum_covered $sum_total" | awk '{printf "%.1f", 100 * $1 / $2}')
	printf '%-32s %s/%s arms = %s%%
' "TOTAL" "$sum_covered" "$sum_total" "$pct"
	if [ -n "${BRANCH_COVERAGE_FLOOR:-}" ]; then
		# A shortfall is a failure even if every package instrumented
		# cleanly, and an instrumentation failure stays a failure even if
		# the surviving packages clear the floor.
		if awk -v p="$pct" -v f="$BRANCH_COVERAGE_FLOOR" 'BEGIN { exit !(p + 0 < f + 0) }'; then
			echo "branch-coverage: ${pct}% is below the ${BRANCH_COVERAGE_FLOOR}% floor" >&2
			failed=1
		else
			echo "branch-coverage: ${pct}% clears the ${BRANCH_COVERAGE_FLOOR}% floor"
		fi
	fi
fi

exit "$failed"
