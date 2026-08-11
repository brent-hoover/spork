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

# Everything below runs against a copy, never the real tree.
cp -a . "$work/mod"
rm -rf "$work/mod/scripts"
for hook in "$work"/mod/*/*/export_test.go "$work"/mod/*/export_test.go; do
	[ -f "$hook" ] || continue
	mv "$hook" "${hook%export_test.go}export_gobco.go"
done
cleanup() {
	while read -r r; do [ -n "$r" ] && rm -rf "$r"; done <"$roots"
	rm -rf "$work"
}
trap cleanup EXIT
cd "$work/mod"

# Dependency closure per test binary, so each instrumented package is
# driven only by the test packages that actually link it.
targets=()
while read -r importpath; do
	targets+=("$importpath")
	go list -deps -test "$importpath" >"$work/deps-$(echo "$importpath" | tr / _)" 2>/dev/null || : >"$work/deps-$(echo "$importpath" | tr / _)"
done < <(go list ./...)

if [ "$#" -gt 0 ]; then
	subjects=("$@")
else
	subjects=()
	while read -r dir; do subjects+=("./${dir#"$PWD"/}"); done < <(go list -f '{{.Dir}}' ./... | grep -v "^$PWD$")
fi

failed=0
for subject in "${subjects[@]}"; do
	importpath=$(go list -f '{{.ImportPath}}' "$subject")
	pkgname=$(go list -f '{{.Name}}' "$subject")
	rel=${subject#./}
	slug=$(echo "$importpath" | tr / _)

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
	if [ "$expected" = "0" ]; then
		printf '%-28s no conditions\n' "$importpath"
		continue
	fi

	# Persist counters from test binaries that have their own TestMain.
	# The ticker alone is not enough: a driver's TestMain ends in
	# os.Exit, which kills the goroutine without a final flush and loses
	# every hit since the last tick (measured: 6320 hits against
	# -immediately's 6368 on identity). So os.Exit is also rerouted
	# through gobco's own GobcoFinish, which persists synchronously.
	# The ticker stays as the backstop for a driver that exits some
	# other way, and the two together are checked against -immediately.
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

	drivers=0
	for t in "${targets[@]}"; do
		grep -qxF "$importpath" "$work/deps-$(echo "$t" | tr / _)" || continue
		drivers=$((drivers + 1))
		tdir=$(cd "$module" && go list -f '{{.Dir}}' "$t")

		# Reroute os.Exit in this driver's test files, then define the
		# replacement. Written after the rewrite so its own os.Exit
		# survives.
		# The subject's own test binary needs no rewrite: gobco injects a
		# TestMain there that already persists, and importing the subject
		# from inside itself would not compile.
		exitfile=$tdir/gobco_exit_test.go
		if [ "$t" != "$importpath" ] && [ ! -f "$exitfile" ]; then
			holder=$(grep -l 'os\.Exit(' "$tdir"/*_test.go 2>/dev/null | head -1 || true)
			if [ -n "$holder" ]; then
				driverpkg=$(sed -n 's/^package \([A-Za-z0-9_]*\).*/\1/p' "$holder" | head -1)
				grep -l 'os\.Exit(' "$tdir"/*_test.go | while read -r f; do
					perl -pi -e 's/os\.Exit\(/gobcoExit(/g' "$f"
				done
				cat >"$exitfile" <<EOF
package $driverpkg

import (
	"os"

	gobcosubject "$importpath"
)

// gobcoExit persists the instrumented package's counters before the
// process goes away. GobcoFinish returns the code it was handed.
func gobcoExit(code int) { os.Exit(gobcosubject.GobcoFinish(code)) }
EOF
			fi
		fi

		stats=$work/stats-$slug-$(echo "$t" | tr / _).json
		if ! (cd "$module" && GOBCO_STATS="$stats" go test -count=1 "$t" \
			>"$work/driver-$slug.log" 2>&1); then
			echo "FAIL $importpath: driver $t did not pass"
			sed -n '1,10p' "$work/driver-$slug.log"
			failed=1
			continue 2
		fi
	done

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
	printf '%-28s %s/%s arms  (%s drivers)\n' "$importpath" "$covered" "$total" "$drivers"
	echo "$report" | jq -r '.[] | select(.t == 0 or .f == 0)
		| "    \(.Start): \(.Code) never \(if .t == 0 and .f == 0 then "evaluated" elif .t == 0 then "true" else "false" end)"'
	[ "$covered" = "$total" ] || failed=1
done

exit "$failed"
