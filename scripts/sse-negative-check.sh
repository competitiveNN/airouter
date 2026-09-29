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

# The mutation: drop the flushBuffered() call that the fix added, so
# `released = true` is set but the buffer is only drained later, after the
# releasing event has already been written.
MUTATION_ORIG='			released = true
			if err := flushBuffered(); err != nil {
				return err
			}
		}'
MUTATION_BROKEN='			released = true
		}'

restore() {
  if [ -n "${BACKUP:-}" ] && [ -f "$BACKUP" ]; then
    cp "$BACKUP" proxy.go
    rm -f "$BACKUP"
  fi
}
trap restore EXIT INT TERM

BACKUP=$(mktemp)
cp proxy.go "$BACKUP"

echo "sse-negative-check: baseline (unmutated) must PASS"
if ! go test -run "$TESTS" ./... >/dev/null 2>&1; then
  echo "  FAIL: the ordering tests do not pass on unmodified code." >&2
  echo "  Something is already broken; fix that before trusting this check." >&2
  restore
  exit 1
fi
echo "  ok"

if ! grep -qF "$MUTATION_ORIG" proxy.go; then
  echo "  SKIP: the mutation site is no longer present in proxy.go." >&2
  echo "  Proxy.streamSSE has been refactored since this script was written." >&2
  echo "  The fix (flush the buffer before forwarding the releasing event) may" >&2
  echo "  have moved or changed shape — confirm the ordering contract by hand." >&2
  restore
  exit 2
fi

echo "sse-negative-check: mutating proxy.go to reintroduce the ordering bug"
if ! python3 - <<'PY'
import sys
src = open('proxy.go').read()
old = "			released = true\n			if err := flushBuffered(); err != nil {\n				return err\n			}\n		}"
new = "			released = true\n		}"
if old not in src:
    sys.exit(1)
open('proxy.go', 'w').write(src.replace(old, new, 1))
PY
then
  echo "  FAIL: could not apply the mutation; proxy.go is unchanged." >&2
  restore
  exit 1
fi

if grep -qF "$MUTATION_BROKEN" proxy.go; then
  echo "  mutation applied"
else
  echo "  FAIL: could not apply the mutation; proxy.go is unchanged." >&2
  restore
  exit 1
fi

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

if ! git diff --quiet proxy.go; then
  echo "  FAIL: proxy.go differs from HEAD after restore." >&2
  git --no-pager diff --stat proxy.go >&2
  exit 1
fi
echo "  ok (and proxy.go is byte-identical to HEAD)"

echo
echo "sse-negative-check: PASS — the ordering guard demonstrably has teeth."
