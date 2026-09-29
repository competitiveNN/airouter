#!/usr/bin/env bash
# Self-test for scripts/dist-freshness-check.sh.
#
# WHY THIS EXISTS
#
# The bundle and patch in dist/ are the only record of the unpushed commits once
# this checkout is gone. A missing one is obvious. A STALE one is not: it still
# exists, still verifies, still carries a plausible set of commits -- it just
# describes an older HEAD, and a handoff built from it would silently omit
# everything committed since. That is the failure this check exists to make
# loud, and a check that has never been seen to fail is not known to be able to.
#
# It also pins the skip contract that dist-freshness-check.sh relies on:
# "nothing unpushed" is CLEAN, not stale. A checkout with no unpushed commits
# needs no artifacts, and a check that reported that as a failure would push
# the next person towards committing the artifacts -- the exact self-referential
# loop scripts/export-unpushed.sh was written to avoid.
#
# The "unpushed" set here is a FIXTURE, not this repository: the check is run
# against throwaway repositories so that its own idea of HEAD, of the artifacts,
# and of what counts as unpushed can all be set deliberately and then broken.
#
# Usage: scripts/dist-freshness-selftest.sh [--keep]
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
# Exit 2 = the harness could not run.
set -uo pipefail

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO/scripts/dist-freshness-check.sh"
[ -r "$CHECK" ] || { echo "freshness-selftest: cannot read $CHECK" >&2; exit 2; }
command -v git >/dev/null 2>&1 || { echo "freshness-selftest: git not found" >&2; exit 2; }

TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/dist-freshness-selftest.XXXXXX") || exit 2
cleanup() {
  [ "$KEEP" -eq 1 ] && { echo "freshness-selftest: kept $TMPROOT"; return; }
  rm -rf "$TMPROOT"
}
trap cleanup EXIT

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

g() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }

# Assert on the exit status AND on expected message text. Asserting only the
# status is how a case passes because the harness is broken rather than because
# the subject behaved.
assert_run() {
  local label="$1" want_status="$2" want_msg="$3" out="$4" got="$5"
  local problems=""
  [ "$got" -ne "$want_status" ] && problems="exit $got, want $want_status"
  if [ -n "$want_msg" ] && ! printf '%s' "$out" | grep -qF -- "$want_msg"; then
    problems="${problems:+$problems; }missing message: $want_msg"
  fi
  if [ -n "$problems" ]; then
    bad "$label ($problems)"
    printf '%s\n' "$out" | sed 's/^/         | /' | head -6
  else
    ok "$label"
  fi
}

# A fixture repository with a real upstream ref and a real dist/ layout.
#
# `upstream <dir>` marks the current HEAD as origin/master. Everything committed
# after that is "unpushed" as far as the check is concerned.
new_repo() {
  local name="$1"
  local dir="$TMPROOT/$name"
  mkdir -p "$dir/scripts" "$dir/dist"
  cp "$CHECK" "$dir/scripts/dist-freshness-check.sh"
  cp "$REPO/scripts/export-unpushed.sh" "$dir/scripts/export-unpushed.sh"
  printf 'base\n' > "$dir/README.md"
  g "$dir" init -q -b master
  g "$dir" add -A
  g "$dir" commit -qm "base commit"
  g "$dir" update-ref refs/remotes/origin/master HEAD
  echo "$dir"
}

# Make the repo look like it has `n` unpushed commits.
add_unpushed() {
  local dir="$1" n="$2"
  local i=0
  while [ "$i" -lt "$n" ]; do
    printf 'change %s\n' "$i" >> "$dir/README.md"
    g "$dir" add -A
    g "$dir" commit -qm "unpushed commit $i"
    i=$((i + 1))
  done
}

# Write plausible artifacts that genuinely cover the current unpushed set. Uses
# the real export script so the fixtures are produced the same way production
# artifacts are, rather than by a hand-rolled stand-in that might diverge from
# the format the check parses.
write_artifacts() {
  local dir="$1"
  ( cd "$dir" && UPSTREAM=origin/master EXPORT_REF=testexport \
      bash scripts/export-unpushed.sh >/dev/null 2>&1 )
}

run_check() {
  local dir="$1"; shift
  local out got
  # UPSTREAM is explicitly cleared. These fixtures pin their own upstream at
  # refs/remotes/origin/master, so an UPSTREAM inherited from the environment
  # names a ref that does not exist in the fixture, and every case then fails
  # with "base does not exist" -- 12 failures that look like the check is broken
  # and are actually this harness leaking its caller's environment.
  #
  # It is not a hypothetical: scripts/recovery-check.sh runs this self-test in
  # a recovered tree, and it is routinely invoked as `UPSTREAM=base
  # bash scripts/recovery-check.sh`, so `base` propagated straight in here.
  out=$(cd "$dir" && UPSTREAM=origin/master bash scripts/dist-freshness-check.sh "$@" 2>&1)
  got=$?
  printf '%s' "$out"
  return "$got"
}

echo "freshness-selftest: case 1 -- a fresh checkout passes"
R=$(new_repo fresh)
add_unpushed "$R" 2
write_artifacts "$R"
out=$(run_check "$R" --verbose); got=$?
assert_run "fresh artifacts pass" 0 "artifacts match HEAD" "$out" "$got"

echo "freshness-selftest: case 2 -- a new commit makes them stale"
add_unpushed "$R" 1
out=$(run_check "$R"); got=$?
assert_run "stale after a new commit fails" 1 "are stale" "$out" "$got"
assert_run "the stale tip is named" 1 "but HEAD is" "$out" "$got"
assert_run "the patch count mismatch is named" 1 "but 3 are unpushed" "$out" "$got"
assert_run "the fix is named" 1 "export-unpushed.sh" "$out" "$got"

echo "freshness-selftest: case 3 -- regenerating clears it"
write_artifacts "$R"
out=$(run_check "$R"); got=$?
assert_run "regenerated artifacts pass" 0 "artifacts match HEAD" "$out" "$got"

echo "freshness-selftest: case 4 -- missing artifacts are reported, not assumed fine"
R2=$(new_repo missing)
add_unpushed "$R2" 1
out=$(run_check "$R2"); got=$?
assert_run "missing artifacts fail" 1 "is missing" "$out" "$got"
# And the half-present case: a valid bundle with a patch carrying no commit
# headers at all -- the shape a truncated or mis-generated patch has.
write_artifacts "$R2"
add_unpushed "$R2" 1
printf 'this is not a patch\n' > "$R2/dist/airouter-unpushed.patch"
out=$(run_check "$R2"); got=$?
assert_run "a patch with no headers is reported" 1 "readable commit headers" "$out" "$got"

echo "freshness-selftest: case 5 -- nothing unpushed is CLEAN, not stale"
R3=$(new_repo clean)
out=$(run_check "$R3"); got=$?
assert_run "no unpushed commits passes" 0 "nothing to carry" "$out" "$got"
# Even with artifacts absent, which is the normal state of a clean checkout.
if [ -e "$R3/dist/airouter-unpushed.bundle" ]; then
  bad "clean checkout has no artifacts to begin with"
else
  ok "clean checkout has no artifacts to begin with"
fi

echo "freshness-selftest: case 6 -- a check that cannot run must not report success"
R4=$(new_repo nourl)
add_unpushed "$R4" 1
write_artifacts "$R4"
out=$(cd "$R4" && UPSTREAM=refs/heads/does-not-exist \
      bash scripts/dist-freshness-check.sh 2>&1); got=$?
assert_run "an unusable upstream exits 2" 2 "does not exist" "$out" "$got"
# Outside any repository the check must exit 2, not 0. The script derives its
# root from its own location, so the fixture has to be a copy in a directory
# with no repository above it -- running the in-repo copy would just cd back
# into a repository and pass, testing nothing.
ORPHAN="$TMPROOT/not-a-repo/scripts"
mkdir -p "$ORPHAN"
cp "$CHECK" "$ORPHAN/dist-freshness-check.sh"
out=$(cd "$TMPROOT/not-a-repo" && bash scripts/dist-freshness-check.sh 2>&1); got=$?
assert_run "outside a repository exits 2" 2 "not a git repository" "$out" "$got"

echo "freshness-selftest: case 7 -- the bundle tip is read, not assumed"
# A bundle built from a DIFFERENT tip than the current one must be caught even
# when the patch happens to agree, which is the case a tip-only read would miss
# if it trusted the filename.
R5=$(new_repo tiplie)
add_unpushed "$R5" 2
write_artifacts "$R5"
# Move the branch back one commit so the artifacts describe a descendant.
g "$R5" reset -q --hard HEAD~1
out=$(run_check "$R5"); got=$?
assert_run "a bundle ahead of HEAD fails" 1 "are stale" "$out" "$got"

echo "freshness-selftest: case 8 -- a wrong tip ALONE is caught"
# Cases 2 and 7 both fail on more than the tip: the patch end and the patch
# commit count also disagree there, so they cannot tell you whether the tip
# comparison works at all. Disabling the tip check left all 13 assertions green
# for exactly that reason. This case holds the patch and the commit count
# correct and makes ONLY the bundle's tip wrong, so nothing else can mask it.
R6=$(new_repo tiponly)
add_unpushed "$R6" 3
write_artifacts "$R6"
# Rebuild the bundle from an EARLIER unpushed commit, leaving the patch alone.
OLD_TIP=$(g "$R6" rev-parse HEAD~1)
g "$R6" update-ref refs/heads/tiponly "$OLD_TIP"
( cd "$R6" && git bundle create dist/airouter-unpushed.bundle \
    refs/heads/tiponly --not origin/master >/dev/null 2>&1 )
g "$R6" update-ref -d refs/heads/tiponly
out=$(run_check "$R6"); got=$?
assert_run "a wrong bundle tip fails on its own" 1 "tip is" "$out" "$got"
assert_run "the wrong tip is named" 1 "but HEAD is" "$out" "$got"
# The patch was left correct, so the failure must be the tip and nothing else.
if printf '%s' "$out" | grep -qF 'airouter-unpushed.patch'; then
  bad "only the bundle tip is wrong (the patch was reported too)"
  printf '%s\n' "$out" | sed 's/^/         | /' | head -4
else
  ok "only the bundle tip is wrong (the patch was reported too)"
fi

echo ""
echo "----------------------------------------"
printf 'freshness-selftest: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
