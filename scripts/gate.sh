#!/usr/bin/env bash
# Run every check in this repository, serially, under a lock.
#
# WHY THIS EXISTS
#
# Verification in this repository used to be ad-hoc: a shell one-liner in a
# background nohup job, retyped each time, with the results appended to a log.
# That arrangement produced three FALSE failures in one session -- the
# attribution self-test, the recovery self-test, and the attribution check all
# reported red while the code was fine.
#
# The cause was not a flaky test. It was a gate and a mutation probe running at
# the same time: the probe was deliberately reverting a fix to confirm a new
# assertion could fail, the gate read the tree mid-revert, and reported the
# deliberate damage as a regression. Nothing was wrong with the code and
# everything was wrong with the signal.
#
# Two things follow, and this script exists to enforce both:
#
#   1. Only one verification run may be in flight at a time. `flock` takes an
#      exclusive lock, so a second run waits instead of interleaving.
#   2. Nothing may modify the tree while a run is in flight. That cannot be
#      locked away from an external editor, so it is stated plainly here and
#      the run records the tree's hash at the start and checks it at the end. A
#      tree that changed underneath a run is reported as INVALID, not as a
#      failure -- the results describe a repository state that no longer exists,
#      and treating them as a pass or a fail is equally wrong.
#
# Exit 0 = every check passed and the tree did not change during the run.
# Exit 1 = at least one check failed.
# Exit 2 = the gate could not run (missing tool, lock unavailable).
# Exit 3 = the tree changed while the run was in progress; results are void.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="$REPO/scripts/gate.sh"
P="gate:"
LOCK="${TMPDIR:-/tmp}/airouter-gate.lock"

# Go is not always on PATH by default here; /usr/bin/go has been the wrong one.
if ! command -v go >/dev/null 2>&1 && [ -x /usr/lib/golang/bin/go ]; then
  PATH="/usr/lib/golang/bin:$PATH"
  export PATH
fi

for t in git python3 go flock; do
  command -v "$t" >/dev/null 2>&1 || { echo "$P $t not found" >&2; exit 2; }
done

tree_fingerprint() {
  # HEAD plus the full worktree state. `git status` covers staged, unstaged and
  # untracked; hashing it catches a revert-and-restore cycle only if the content
  # differs, which is the case that matters.
  { git -C "$REPO" rev-parse HEAD; git -C "$REPO" status --porcelain -uall; } \
    | git hash-object --stdin 2>/dev/null || echo unknown
}

# Take the lock. The lock is on the file descriptor, so it is released on exit
# even if this script is killed; nothing has to clean up after a crash.
exec 9>"$LOCK" || { echo "$P cannot open $LOCK" >&2; exit 2; }
if ! flock -n 9; then
  echo "$P another verification run holds the lock; waiting..."
  flock 9 || { echo "$P could not acquire $LOCK" >&2; exit 2; }
fi

START_FP=$(tree_fingerprint)
[ "$START_FP" = "unknown" ] && { echo "$P cannot fingerprint the tree" >&2; exit 2; }

PASSED=0
FAILED=0
FAILED_NAMES=""

# run <name> <command...>
run() {
  local name="$1"; shift
  local out got
  out=$("$@" 2>&1); got=$?
  if [ "$got" -eq 0 ]; then
    PASSED=$((PASSED + 1))
    printf '  PASS  %s\n' "$name"
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES $name"
    printf '  FAIL  %s\n' "$name"
    # Show enough of the failure to act on, and no more: a self-test prints its
    # own per-assertion detail, and dumping 40 lines of it here buries the
    # other 21 results.
    printf '%s\n' "$out" | tail -3 | sed 's/^/          | /'
  fi
}

fmt_ok() {
  # A check whose success is the ABSENCE of output, not a zero status.
  local name="$1"; shift
  local out
  out=$("$@" 2>&1)
  if [ -z "$out" ]; then
    PASSED=$((PASSED + 1))
    printf '  PASS  %s\n' "$name"
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES $name"
    printf '  FAIL  %s (expected no output)\n' "$name"
    printf '%s\n' "$out" | head -5 | sed 's/^/          | /'
  fi
}

echo "$P repository $REPO"
echo "$P tree $(git -C "$REPO" rev-parse --short HEAD) ($(git -C "$REPO" rev-list --count origin/master..HEAD 2>/dev/null || echo '?') unpushed)"
echo ""

# ------------------------------------------------------------------ code ----
run "go build ./..."          go build ./...
run "go vet ./..."           go vet ./...
fmt_ok "gofmt -l . is clean"  gofmt -l .

# `go test` without -race and with it, because the race detector changes the
# timing and a suite that only ever runs one way has not been run both ways.
run "go test -count=1"       go test -count=1 ./...
run "go test -race -count=1" go test -race -count=1 ./...

# ------------------------------------------------------------ self-tests ----
run "fuzz-gate-selftest"         bash "$REPO/scripts/fuzz-gate-selftest.sh"
run "audit-drift-selftest"       bash "$REPO/scripts/audit-drift-selftest.sh"
run "audit-attribution-selftest" bash "$REPO/scripts/audit-attribution-selftest.sh"
run "dist-freshness-selftest"    bash "$REPO/scripts/dist-freshness-selftest.sh"
run "recovery-check-selftest"    bash "$REPO/scripts/recovery-check-selftest.sh"
run "doc-verify-selftest"        bash "$REPO/scripts/doc-verify-selftest.sh"
run "test-suite-selftest"        bash "$REPO/scripts/test-suite-selftest.sh"
run "shell-lint"                 bash "$REPO/scripts/shell-lint.sh"
run "shell-lint-control"         bash "$REPO/scripts/shell-lint-control.sh"

# -------------------------------------------------------------- security ----
run "secret-scan"        bash "$REPO/scripts/secret-scan.sh"
run "sse-negative-check" bash "$REPO/scripts/sse-negative-check.sh"

# ----------------------------------------------------------------- audit ----
run "audit-drift-check"       python3 "$REPO/scripts/audit-drift-check.py"
run "audit-attribution-check" python3 "$REPO/scripts/audit-attribution-check.py"
run "dist-freshness-check"    bash "$REPO/scripts/dist-freshness-check.sh"
run "config pytest"           python3 -m pytest -q "$REPO/scripts/test_regenerate_config.py"

# The Go suite itself, measured rather than run: `go test ./...` is green when a
# third of the tests have gone missing, and was green once when a test file was
# truncated. This is the only place the SUITE has a contract, so it is the only
# place losing a test is a failure rather than a smaller success.
run "test-suite-check" python3 "$REPO/scripts/test-suite-check.py"

# The handoff document, executed rather than grepped; see the file for why that
# distinction is the whole point of it.
run "doc-verify" bash "$REPO/scripts/doc-verify.sh"

# The expensive one, last: it builds and tests a recovered tree, which is the
# claim the whole handoff rests on and the one no other check makes.
run "recovery-check (full)" bash "$REPO/scripts/recovery-check.sh"

# --------------------------------------------------------------- summary ----
END_FP=$(tree_fingerprint)

echo ""
echo "----------------------------------------"
printf 'gate: %d passed, %d failed\n' "$PASSED" "$FAILED"

if [ "$END_FP" != "$START_FP" ]; then
  echo ""
  echo "$P INVALID RUN -- the working tree changed while these checks were"
  echo "$P in progress. The results above describe a repository state that no"
  echo "$P longer exists, so they are neither a pass nor a failure. This is"
  echo "$P what concurrent mutation looks like from the gate's side; run it"
  echo "$P again with nothing else editing the tree."
  exit 3
fi

if [ "$FAILED" -ne 0 ]; then
  echo "$P failed:$FAILED_NAMES"
  exit 1
fi
exit 0
