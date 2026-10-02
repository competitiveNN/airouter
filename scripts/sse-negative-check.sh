#!/usr/bin/env bash
# Prove the SSE release-ordering contract is actually enforced, not just
# currently satisfied.
#
# Why: TestStreamSSE_WireLevelDeltaOrdering passes on correct code — which is
# also what a test that asserts nothing would do. The only way to know the
# guard has teeth is to break the thing it guards and confirm the test
# notices. That check was done once by hand; this makes it repeatable, so the
# next person to touch Proxy.streamSSE's hold-and-release machinery can re-prove
# it in one command.
#
# Method: apply a minimal, surgical mutation that restores the historical bug
# (the releasing event written before its own buffer), run the two ordering
# tests, require that they FAIL, then restore the original file and require
# that they PASS again. Any other outcome is a failure.
#
# Nothing is left behind: proxy.go is restored from a temp copy via a trap, so
# an interrupt or an early exit cannot leave the mutation in your tree.
#
# Usage: scripts/sse-negative-check.sh
#        SKE=1 scripts/sse-negative-check.sh   # skip the confirm-still-passes run

set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 2

TESTS='TestStreamSSE_BuffersPreReleaseEventsUntilReleasePoint|TestStreamSSE_WireLevelDeltaOrdering'

restore() {
  if [ -n "${BACKUP:-}" ] && [ -f "$BACKUP" ]; then
    cp "$BACKUP" proxy.go
  fi
}
# PRE must outlive restore() — it is the reference the restore is checked
# against — so the temp files are swept by their own trap, not by restore().
cleanup() {
  restore
  [ -n "${BACKUP:-}" ] && rm -f "$BACKUP"
  [ -n "${PRE:-}" ] && rm -f "$PRE"
  return 0
}
trap cleanup EXIT INT TERM

BACKUP=$(mktemp)
cp proxy.go "$BACKUP"
# PRE is the file as it was BEFORE this run touched anything. It is the only
# honest reference for "restore worked": the run must leave the working tree
# exactly as it found it, whatever state that was.
PRE=$(mktemp)
cp proxy.go "$PRE"

echo "sse-negative-check: baseline (unmutated) must PASS"
if ! go test -run "$TESTS" ./... >/dev/null 2>&1; then
  echo "  FAIL: the ordering tests do not pass on unmodified code." >&2
  echo "  Something is already broken; fix that before trusting this check." >&2
  restore
  exit 1
fi
echo "  ok"

# Prose for both outcomes, so the pre-check and the mutation cannot disagree.
refactored() {
  echo "  SKIP: the mutation site is no longer present in proxy.go." >&2
  echo "  Proxy.streamSSE has been refactored since this script was written." >&2
  echo "  The fix (flush the buffer before forwarding the releasing event) may" >&2
  echo "  have moved or changed shape — confirm the ordering contract by hand" >&2
  echo "  and update the mutation in this script to match." >&2
}

# The mutation is applied by the python block below, which is the single source
# of truth for the site text. There is deliberately no separate grep pre-check:
# two copies of the site string can drift apart, and when they do the pre-check
# reports "refactored" for a site that is still present (or vice versa).

echo "sse-negative-check: mutating proxy.go to reintroduce the ordering bug"
# Capture python's exit code directly. Using `if ! cmd` would lose it, because
# $? then reflects the negation, not the command.
set +e
python3 - <<'PY'
import sys
src = open('proxy.go').read()
old = "			released = true\n			if err := flushBuffered(); err != nil {\n				return err\n			}\n		}"
new = "			released = true\n		}"
if old not in src:
    sys.exit(3)   # 3 = site gone, not a write failure
open('proxy.go', 'w').write(src.replace(old, new, 1))
PY
mutate_code=$?
set -e

# Distinguish "the code moved" (exit 2, a warning) from "the mutation could
# not be written" (exit 1, a real problem). Getting this backwards would tell
# a future maintainer their guard is broken when the code merely refactored.
case "$mutate_code" in
  0) : ;;
  3) refactored; restore; exit 2 ;;
  *) echo "  FAIL: could not apply the mutation (python exit $mutate_code)." >&2; restore; exit 1 ;;
esac

# Verify the mutation actually changed something. Comparing against the backup
# is the real test; a substring grep would also match the pre-mutation text,
# since the "broken" form is a prefix of the "fixed" form.
if cmp -s proxy.go "$BACKUP"; then
  echo "  FAIL: the mutation did not change proxy.go." >&2
  restore
  exit 1
fi
echo "  mutation applied"

echo "sse-negative-check: mutated code must FAIL the ordering tests"
if go test -run "$TESTS" ./... >/dev/null 2>&1; then
  echo "  FAIL: the ordering tests still pass with the bug reintroduced." >&2
  echo "  The guard is toothless — it does not actually protect the contract." >&2
  restore
  exit 1
fi
echo "  ok (tests correctly failed)"

restore

if [ "${SKE:-0}" = "1" ]; then
  echo "sse-negative-check: PASS (skipping the confirm-restored run)"
  exit 0
fi

echo "sse-negative-check: restored code must PASS again"
if ! go test -run "$TESTS" ./... >/dev/null 2>&1; then
  echo "  FAIL: tests do not pass after restoring proxy.go." >&2
  echo "  git diff proxy.go to inspect what changed." >&2
  exit 1
fi

# Compare against PRE, not HEAD. This used to be `git diff --quiet proxy.go`,
# which asks a different question: it reports whether proxy.go matches the last
# COMMIT, not whether the restore put it back. Those agree only on a clean tree.
# With legitimate uncommitted work in proxy.go -- exactly the file this script
# mutates -- the old check failed every run for work it had not touched, and the
# obvious "fix" of committing first would have destroyed that work. `cmp` against
# PRE is what the stage actually means: byte-identical to where it started.
if ! cmp -s proxy.go "$PRE"; then
  echo "  FAIL: proxy.go was not restored to its pre-run contents." >&2
  git --no-pager diff --stat proxy.go >&2
  exit 1
fi
echo "  ok (and proxy.go is byte-identical to its pre-run contents)"

echo
echo "sse-negative-check: PASS — the ordering guard demonstrably has teeth."
