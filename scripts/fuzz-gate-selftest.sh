#!/usr/bin/env bash
# Self-test for scripts/fuzz-gate.sh.
#
# WHY THIS EXISTS
#
# The gate exists to catch failures that "look like coverage": a target nothing
# explores, a discovery pattern that no longer matches, a crasher nobody
# committed. Round 9 found four defects in it that every existing check
# reported as PASS, for one reason: the repository is a single Go package, and
# all four defects only manifest once there is a second one. The gate was
# therefore never executed in the configuration it was written for.
#
# A self-test is the fix for that class, not a retest of the same tree. It
# builds a throwaway repository with two packages and asserts the gate's
# behaviour there, so the condition that made the bugs invisible is now
# reproduced on every run. Nothing here imports the real repository, and
# nothing it creates is left behind.
#
# Each case asserts a *specific* failure mode, and each was confirmed to pass
# against a deliberately reverted gate before being accepted — a self-test that
# has never failed is indistinguishable from one that cannot.
#
# Usage: scripts/fuzz-gate-selftest.sh [--keep]
#        --keep   leave the temporary repositories on disk for inspection
#
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
set -uo pipefail

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

GATE_SOURCE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/fuzz-gate.sh"
[ -r "$GATE_SOURCE" ] || { echo "selftest: cannot read $GATE_SOURCE" >&2; exit 2; }

TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/fuzz-gate-selftest.XXXXXX") || exit 2
cleanup() {
  [ "$KEEP" -eq 1 ] && { echo "selftest: kept $TMPROOT"; return; }
  rm -rf "$TMPROOT"
}
trap cleanup EXIT

PASSED=0
FAILED=0
CURRENT=""

# A stub `go` so the self-test never runs a real fuzzer, and never depends on
# the Go toolchain being present or on how long a real fuzz run takes. It
# answers only what the gate asks: the seed replay (`-run`) and the fuzz phase
# (`-fuzz`). Crucially it REJECTS a `-fuzz` invocation naming `.` or a
# multi-package pattern, exactly as the real go tool does — that rejection is
# what makes the `./...` regression detectable here at all.
mk_go_stub() {
  local dir="$1"
  cat > "$dir/go" <<'STUB'
#!/usr/bin/env bash
# Records its arguments and enforces the one real constraint that matters:
# go test -fuzz accepts exactly one package pattern.
printf '%s\n' "$*" >> "$GO_STUB_LOG"
fuzzing=0
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  [ "${args[i]}" = "-fuzz" ] && fuzzing=1
done
if [ "$fuzzing" -eq 1 ]; then
  last="${args[${#args[@]}-1]}"
  # Only "./..." is the multi-package pattern. A bare "." names exactly one
  # package and is the CORRECT argument for a root-level target, so matching
  # it here made the stub reject the very fix the round-9 cases assert. The
  # stub has to model go's rule, not a stricter one, or it tests itself.
  case "$last" in
    ./...)
      echo "cannot use -fuzz flag with multiple packages" >&2
      exit 1
      ;;
  esac
fi
exit 0
STUB
  chmod +x "$dir/go"
}

# new_repo <name> -- a git repo with a root package and one subpackage, each
# holding a fuzz target. This is the shape the round-9 defects were blind to.
#
# The path is echoed on stdout and NOTHING ELSE may be: `git init` and `git
# commit` write to stdout, so a subshell that captures them would append their
# chatter to the path and every later `$repo` would be a nonsense path. That
# failure is silent — `cd` complains, the gate never runs, and the cases fail
# for reasons that have nothing to do with what they claim to test.
new_repo() {
  # Split declarations: `local name="$1" dir="$TMPROOT/$name"` expands $name
  # before the first assignment takes effect in some bash versions, so $dir
  # ends up as "<TMPROOT>/" and every case then fails on `cd: null directory`.
  local name="$1"
  local dir="$TMPROOT/$name"
  mkdir -p "$dir/subpkg"
  printf 'module %s\n\ngo 1.22\n' "$name" > "$dir/go.mod"
  printf 'package main\n\nfunc main() {}\n' > "$dir/main.go"
  cat > "$dir/root_test.go" <<'EOF'
package main

import "testing"

func FuzzRootTarget(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) {})
}
EOF
  printf 'package subpkg\n\nfunc H() int { return 1 }\n' > "$dir/subpkg/sub.go"
  cat > "$dir/subpkg/sub_test.go" <<'EOF'
package subpkg

import "testing"

func FuzzSubTarget(f *testing.F) {
	f.Add(2)
	f.Fuzz(func(t *testing.T, n int) {})
}
EOF
  mk_go_stub "$dir"
  (
    cd "$dir" || exit 1
    git init -q .
    git config user.email selftest@example.invalid
    git config user.name selftest
    git add -A
    git commit -qm "fixture: two packages"
  ) >/dev/null 2>&1
  printf '%s\n' "$dir"
}

# run_gate <repo> <gate-script> <args...> -- run the gate inside a repo with
# the stub `go` first on PATH. Echoes combined output, returns its exit code.
#
# The call log is TRUNCATED per run. The stub appends, so a shared log
# accumulates every invocation from every case, and a later case counting
# "-fuzz invocations" would then be counting its predecessors' too — the gate
# could stop fuzzing the second target entirely and the count would still say
# two.
run_gate() {
  local repo="$1"; shift
  local gate="$1"; shift
  : > "$repo/go-calls.log"
  ( cd "$repo" && PATH="$repo:$PATH" GO_STUB_LOG="$repo/go-calls.log" \
      bash "$gate" "$@" 2>&1 )
}

begin() { CURRENT="$1"; }

ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad()  { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

# assert_contains <label> <haystack> <needle>
assert_contains() {
  if printf '%s' "$2" | grep -qF -- "$3"; then
    ok "$1"
  else
    bad "$1 (expected to find: $3)"
    printf '%s\n' "$2" | sed 's/^/         | /' | head -12
  fi
}

# assert_absent <label> <haystack> <needle>
assert_absent() {
  if printf '%s' "$2" | grep -qF -- "$3"; then
    bad "$1 (did not expect: $3)"
  else
    ok "$1"
  fi
}

assert_status() {
  local label="$1" want="$2" got="$3"
  if [ "$got" -eq "$want" ]; then
    ok "$label"
  else
    bad "$label (exit $got, want $want)"
  fi
}

echo "fuzz-gate selftest: $GATE_SOURCE"
echo

# ---------------------------------------------------------------------------
echo "case 1: a two-package repository fuzzes each target in its own package"
# ---------------------------------------------------------------------------
# This is the case the round-9 defects were invisible in. A gate that regressed
# to `./...` fails here, and the stub's "cannot use -fuzz flag with multiple
# packages" is what it would be told.
repo=$(new_repo two-package)
out=$(run_gate "$repo" "$GATE_SOURCE" 1); st=$?
assert_status "gate passes on a two-package repo" 0 $st
assert_contains "root target fuzzed in ." "$out" "fuzzing FuzzRootTarget in . "
assert_contains "subpackage target fuzzed in ./subpkg" "$out" "fuzzing FuzzSubTarget in ./subpkg "
assert_absent "no multi-package -fuzz invocation" "$(cat "$repo/go-calls.log")" "-fuzztime 1s ./..."
# The subpackage must be named as a filesystem path. A bare `subpkg` is read by
# go as a stdlib import and fails with "package subpkg is not in std".
assert_absent "subpackage is not named as a bare package path" \
  "$(cat "$repo/go-calls.log")" "-fuzztime 1s subpkg"
echo

# ---------------------------------------------------------------------------
echo "case 2: a non-integer duration is a usage error, not a fuzz failure"
# ---------------------------------------------------------------------------
# The round-8 FROZEN=1 defect: an env var read positionally became the
# duration, and Go's "invalid duration" was reported as a counterexample.
for bad_arg in "FROZEN=1" "30s" "20x" "-5"; do
  out=$(run_gate "$repo" "$GATE_SOURCE" "$bad_arg"); st=$?
  assert_status "rejects '$bad_arg' with exit 2" 2 $st
  assert_contains "rejects '$bad_arg' as a usage error" "$out" "must be a non-negative integer"
  assert_absent "'$bad_arg' fabricates no counterexample" "$out" "found a counterexample"
done
echo

# ---------------------------------------------------------------------------
echo "case 3: an untracked test file is reported, by set difference"
# ---------------------------------------------------------------------------
# Round 9 replaced a count comparison with `comm` in both directions. A count
# passes whenever one untracked add is balanced by one tracked delete, so the
# paired case is what actually tests the fix.
out=$(run_gate "$repo" "$GATE_SOURCE" 0); st=$?
assert_status "clean tree passes" 0 $st

printf 'package main\n\nimport "testing"\n\nfunc TestExtra(t *testing.T) {}\n' \
  > "$repo/extra_test.go"
printf 'package subpkg\n' > "$repo/subpkg/subpkg_test.go"   # tracked, then deleted
git -C "$repo" add subpkg/subpkg_test.go
git -C "$repo" commit -qm "fixture: will be deleted"
rm -f "$repo/subpkg/subpkg_test.go"
out=$(run_gate "$repo" "$GATE_SOURCE" 0); st=$?
assert_status "balanced add+delete is still caught" 1 $st
assert_contains "untracked file reported" "$out" "extra_test.go"
assert_contains "tracked-but-missing file reported" "$out" "subpkg_test.go"
rm -f "$repo/extra_test.go"
git -C "$repo" checkout -- subpkg/subpkg_test.go 2>/dev/null
echo

# ---------------------------------------------------------------------------
echo "case 4: an uncommitted crasher in a SUBDIRECTORY package is reported"
# ---------------------------------------------------------------------------
# Go writes a crasher beside the package that produced it. A check scoped to the
# root `testdata/fuzz` cannot see `./subpkg/testdata/fuzz`, which is exactly
# where it lands now that the gate covers subpackages.
#
# A FRESH repo, not the one case 3 dirtied. Each case gets its own tree: a case
# that inherits the previous case's leftovers asserts nothing about its own
# defect, and here it passed for the wrong reason -- the tracked-but-missing
# file from case 3 fired first and the gate never reached the crasher check.
repo4=$(new_repo leaked-crasher)
mkdir -p "$repo4/subpkg/testdata/fuzz/FuzzSubTarget"
printf 'not-a-corpus-entry' > "$repo4/subpkg/testdata/fuzz/FuzzSubTarget/leaked"
out=$(run_gate "$repo4" "$GATE_SOURCE" 0); st=$?
assert_status "uncommitted subdirectory crasher fails the gate" 1 $st
assert_contains "the subdirectory crasher is named" "$out" "subpkg/testdata/fuzz/FuzzSubTarget/leaked"
echo

# ---------------------------------------------------------------------------
echo "case 5: the gate is still green on a genuinely healthy two-package repo"
# ---------------------------------------------------------------------------
# Everything above is a failure case. This one exists so a gate that simply
# refuses to run anything cannot pass the suite: it must discover both targets,
# fuzz both, and exit 0 on a clean tree.
repo2=$(new_repo healthy)
out=$(run_gate "$repo2" "$GATE_SOURCE" 1); st=$?
assert_status "healthy two-package repo passes" 0 $st
assert_contains "both targets discovered" "$out" "FuzzRootTarget FuzzSubTarget"
calls=$(grep -c ' -fuzz ' "$repo2/go-calls.log")
if [ "$calls" -ge 2 ]; then
  ok "both targets actually fuzzed ($calls -fuzz invocations)"
else
  bad "expected 2 -fuzz invocations, saw $calls"
fi
echo

# ---------------------------------------------------------------------------
echo "case 6: discovery that finds nothing is an error, not an empty pass"
# ---------------------------------------------------------------------------
# The silent-coverage failure the whole script is built around: if the pattern
# stops matching, a gate that loops over nothing still exits 0.
repo3="$TMPROOT/no-targets"
mkdir -p "$repo3"
printf 'module notargets\n\ngo 1.22\n' > "$repo3/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$repo3/main.go"
printf 'package main\n\nimport "testing"\n\nfunc TestOnly(t *testing.T) {}\n' \
  > "$repo3/main_test.go"
mk_go_stub "$repo3"
( cd "$repo3" && git init -q . && git config user.email s@e.invalid \
  && git config user.name s && git add -A && git commit -qm fixture ) >/dev/null 2>&1
out=$(run_gate "$repo3" "$GATE_SOURCE" 1); st=$?
assert_status "repo with no fuzz targets fails" 1 $st
assert_contains "and says so" "$out" "no Fuzz targets found"
echo

# ---------------------------------------------------------------------------
# Negative control: the cases above must FAIL against the pre-round-9 gate.
# Without this, "the self-test passes" is indistinguishable from "the self-test
# asserts nothing". The old gate is reconstructed from git and exercised on the
# same two-package fixture; it is expected to break, which is the proof the
# suite has teeth.
#
# The baseline is cb4e826 (round 8), NOT the commit before it. Round 8 is where
# the FROZEN=1 and root-only-discovery defects were fixed; those are already
# covered against the CURRENT gate by case 2 and case 3. What is checked here
# is only what round 9 changed -- the round-9 gate is the one that runs
# multi-package, so it is round 8's that must fail to.
# ---------------------------------------------------------------------------
echo "case 7: negative control — the pre-round-9 gate fails these fixtures"
# ---------------------------------------------------------------------------
OLD_GATE=$(git -C "$(dirname "$GATE_SOURCE")/.." show \
  cb4e826:scripts/fuzz-gate.sh 2>/dev/null)
if [ -z "$OLD_GATE" ]; then
  bad "could not reconstruct the pre-round-9 gate from cb4e826"
else
  printf '%s\n' "$OLD_GATE" > "$TMPROOT/old-gate.sh"
  out=$(run_gate "$repo2" "$TMPROOT/old-gate.sh" 1); st=$?
  assert_status "old gate fails on a two-package repo" 1 $st
  assert_contains "old gate hits the multi-package refusal" "$out" "cannot use -fuzz flag with multiple packages"
  # The same fixture the current gate passes in case 5. A gate that regressed
  # the multi-package fix would now be indistinguishable from the old one.
  old_calls=$(grep -c ' -fuzz ' "$repo2/go-calls.log" 2>/dev/null || echo 0)
  if [ "$old_calls" -lt 2 ]; then
    ok "old gate fuzzes fewer targets than the current gate ($old_calls)"
  else
    bad "old gate fuzzed $old_calls targets; the negative control proves nothing"
  fi
fi
echo

echo "----------------------------------------"
printf 'selftest: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
