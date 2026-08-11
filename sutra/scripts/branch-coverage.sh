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
#     aborts the whole package. See api.SetBodyLimitForTest.
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
cleanup() {
	while read -r r; do [ -n "$r" ] && rm -rf "$r"; done <"$roots"
	rm -rf "$work"
}
trap cleanup EXIT

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

	# Persist counters from test binaries that have their own TestMain.
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
		(cd "$module" && GOBCO_STATS="$work/stats-$slug-$(echo "$t" | tr / _).json" \
			go test -count=1 "$t" >/dev/null 2>&1) || true
	done

	# Sum every driver's counts per condition; an arm is covered when
	# some driver evaluated it at least once.
	report=$(jq -s '
		add // []
		| group_by(.Start)
		| map({Start: .[0].Start, Code: .[0].Code,
		       t: map(.TrueCount) | add, f: map(.FalseCount) | add})
	' "$work"/stats-"$slug"-*.json 2>/dev/null || echo '[]')

	total=$(echo "$report" | jq 'length * 2')
	covered=$(echo "$report" | jq '[.[] | (if .t > 0 then 1 else 0 end) + (if .f > 0 then 1 else 0 end)] | add // 0')
	printf '%-28s %s/%s arms  (%s drivers)\n' "$importpath" "$covered" "$total" "$drivers"
	echo "$report" | jq -r '.[] | select(.t == 0 or .f == 0)
		| "    \(.Start): \(.Code) never \(if .t == 0 and .f == 0 then "evaluated" elif .t == 0 then "true" else "false" end)"'
	[ "$covered" = "$total" ] || failed=1
done

exit "$failed"
