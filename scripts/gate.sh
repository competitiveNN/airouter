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
  #
  # KNOWN LIMIT: this compares the tree at the START with the tree at the END.
  # It cannot see a write that happens DURING the run and is then reverted, and
  # that is not hypothetical. On 2026-09-30 five consecutive stages failed —
  # go test, go test -race, config pytest, mutation-check, config rules check —
  # all of them reading config.yaml, while config.yaml was rewritten at 19:04 and
  # restored at 19:05 to content byte-identical to config.yaml.bak. The
  # fingerprint matched at both ends, so the run was reported as ordinary
  # failures rather than INVALID RUN, and every stage passed on a re-run.
  #
  # The writer was NOT identified. `scripts/sync-models.sh` is the only thing
  # that creates config.yaml.bak, and a backup surviving means it exited without
  # reaching its `rm -f`; but the timer was inactive and no sync process was
  # running when the evidence was collected. Do not repeat the guess I did: two
  # other maki sessions were live on this machine, and I attributed the write to
  # one of them without checking its cwd. Both were elsewhere (`.config/maki`
  # and a different project). Check `readlink /proc/<pid>/cwd` before blaming a
  # process.
  #
  # The flock below serializes GATE runs against each other. It does not stop any
  # other writer. If stages fail in a cluster that all read one file, suspect a
  # concurrent writer before suspecting the code, and check mtimes before
  # re-running: a file whose mtime is older than the run was not touched by it.
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

# Snapshot the mtimes of tracked paths, for DIAGNOSIS only. Deliberately NOT an
# invalidation rule: measured on 2026-09-30, a clean run of the `config pytest`
# stage alone advances the mtime of three tracked sources
# (fetch-free-models.py, regenerate_config.py, scripts/test_regenerate_config.py)
# with byte-identical content, and a full run advances five (those plus
# dist/README.md and proxy.go). A "mtime advanced => INVALID RUN" check would
# therefore fire on every single run, which is worse than no check at all.
#
# What it buys instead: when the run IS already void on content, this names which
# paths were rewritten, which is the difference between a triage that starts at
# the right file and one that starts by guessing. Written to $TMPDIR, never into
# the repository, for the reason the routing log is out of tree.
MTIME_SNAP="${TMPDIR:-/tmp}/airouter-gate-mtimes.$$"
tracked_mtimes() {
  git -C "$REPO" ls-files -z 2>/dev/null \
    | xargs -0 -r stat -c '%Y %n' -- 2>/dev/null | sort
}
tracked_mtimes >"$MTIME_SNAP" 2>/dev/null || :

PASSED=0
FAILED=0
SKIPPED=0
SKIPPED_NAMES=""
FAILED_NAMES=""
# Every stage is registered by one of exactly three helpers, and each of them
# increments exactly one of the three counters above. The total is asserted at
# the end, because a fourth helper added later without a counter bump -- or one
# whose counter is bumped in only one of its branches -- makes stages vanish
# from the accounting while still running. The results would read complete and
# be short a stage, which is the same shape as a check that silently stopped
# running.
REGISTERED=$(grep -cE '^(run |run_live |fmt_ok )"' "$0")

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

# run_live <name> <command...> — a stage that needs the real provider
# credentials and network access, so it cannot run everywhere.
#
# Three states, not two, and the third is the point. A live stage that degraded
# to "pass" on a machine without credentials would be indistinguishable from one
# that actually verified the route, and that is precisely how a check stops
# being run: it looks green in every run and covers nothing. So:
#
#   exit 0  -> PASS, counted.
#   exit 77 -> SKIP, NOT counted as a pass, printed in its own colour-free
#              line and named again in the summary. A green gate on a machine
#              that skipped the live stage says so out loud.
#   other   -> FAIL, counted.
#
# A skip does not fail the run, because a checkout without provider keys is a
# legitimate state to be in. It is recorded rather than hidden.
run_live() {
  local name="$1"; shift
  local out got
  out=$("$@" 2>&1); got=$?
  if [ "$got" -eq 0 ]; then
    PASSED=$((PASSED + 1))
    printf '  PASS  %s\n' "$name"
  elif [ "$got" -eq 77 ]; then
    SKIPPED=$((SKIPPED + 1))
    SKIPPED_NAMES="$SKIPPED_NAMES $name"
    printf '  SKIP  %s (no credentials on this machine)\n' "$name"
    printf '%s\n' "$out" | tail -2 | sed 's/^/          | /'
  else
    FAILED=$((FAILED + 1))
    FAILED_NAMES="$FAILED_NAMES $name"
    printf '  FAIL  %s\n' "$name"
    printf '%s\n' "$out" | tail -5 | sed 's/^/          | /'
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
# ---- WHY THERE ARE TWO ROUTING STAGES. DO NOT COLLAPSE THEM. ----
#
# `opencode-routing-selftest` and `live opencode routing` are not a duplicate
# pair and their similar sizes are coincidence. They have different upstreams,
# different credentials and different claims:
#
#   selftest — 127.0.0.1 via scripts/fake-opencode-upstream.py, dummy key.
#               Claims the CHECK's own guards hold: fixture validation, the
#               port pre-flight, the retry predicate and budget, the
#               provider pinning, and that each of those can fail.
#   live     — https://opencode.ai/zen/v1, the real key from .envrc.
#               Claims ROUTING works: four profiles, each adding a session on
#               the expected provider and model, streamed and non-streamed.
#
# Neither can stand in for the other. The fake cannot prove anything about
# opencode.ai, and the live stage exits 77 (SKIP) without a credential — so on
# a keyless machine or in CI it covers nothing, and a neutered live check would
# be found only by someone holding a key. That is why the hermetic one exists and
# why it is the one that runs the fault injection.
#
# If you are tempted to merge them into "one routing stage", note that the
# merged version would either need a key to run at all, or would stop testing
# the live provider. Neither is the same check.
#
# The selftest proves the LIVE check's guards can fail: it is hermetic (it stands
# the real gateway up against a fake upstream on loopback, with a dummy key), so
# it runs everywhere.
run "opencode-routing-selftest"  bash "$REPO/scripts/opencode-routing-selftest.sh"
run "shell-lint"                 bash "$REPO/scripts/shell-lint.sh"
run "shell-lint-control"         bash "$REPO/scripts/shell-lint-control.sh"

# -------------------------------------------------------------- security ----
run "secret-scan"        bash "$REPO/scripts/secret-scan.sh"
run "sse-negative-check" bash "$REPO/scripts/sse-negative-check.sh"

# ----------------------------------------------------------------- audit ----
run "audit-drift-check"       python3 "$REPO/scripts/audit-drift-check.py"
run "audit-attribution-check" python3 "$REPO/scripts/audit-attribution-check.py"
run "dist-freshness-check"    bash "$REPO/scripts/dist-freshness-check.sh"
# scripts/run-config-tests.py rather than `python3 -m pytest`: the shell layer
# rewrites that output into a one-line summary, and a suite that failed to
# COLLECT came back as "No tests collected" with exit status 0. The runner
# asserts a collection floor, so "the suites passed" here means they ran.
run "config pytest"           python3 "$REPO/scripts/run-config-tests.py"

# The tests above are only worth their runtime if they can fail. This plants a
# defect in the sync chain, proves the suite goes red, restores the tree and
# fails if any planted defect went unnoticed — a hand-run version of this
# reported green while a mutation had silently patched nothing, so the counts
# and the byte-level restore are assertions, not output to eyeball.
# Preflight first, and as its own step: a tree left dirty by a sweep that was
# killed mid-mutation (SIGKILL/OOM — the paths a finally cannot cover) makes
# every later result untrustworthy, and the only thing that notices is this.
run "mutation preflight"     python3 "$REPO/scripts/mutation-check.py" --preflight
run "mutation-check"         python3 "$REPO/scripts/mutation-check.py"

# The distribution rules the nightly model sync is supposed to follow, checked
# against the fetched model list: key-group completeness, the auto-fallback
# terminator, vision/score sync, no invented or dead ids. validate-config.py
# covers the structural half (profiles, providers, api_key_env); this covers
# the half that needs the data. It degrades to a NOTICE, not a pass, when no
# model list is present.
run "config rules check"      python3 "$REPO/scripts/check-rules.py" --config "$REPO/config.yaml"

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

# -------------------------------------------------------------------- live ----
# NOT hermetic. Needs OPENCODE_API_KEY from .envrc and network access, and it
# makes four real completions against opencode.ai/zen. It is here anyway,
# because every other stage is a statement about the code and none of them is a
# statement about whether the gateway routes anywhere: the hermetic CI job
# (testdata/ci-config.yaml + scripts/fake-sse-upstream.py) asserts the SSE
# contract against a fake upstream, which cannot tell a working route from a
# dead one. This runs the real thing against a fixture whose every chain is
# opencode, and reads the routing back off /admin/sessions.
#
# It does not perturb the tree: the fixture is copied into a temp dir before
# startup (airouter writes cooldowns.json beside its config), and the binary it
# may build is gitignored. The fingerprint comparison below is what would catch
# it if that ever stopped being true.
#
# This is the LIVE half of the pair documented at `opencode-routing-selftest`
# above. Read that comment before changing either one: the two are not
# interchangeable, and the live half is the only stage here that can skip
# (exit 77) rather than pass or fail.
run_live "live opencode routing" bash "$REPO/scripts/opencode-routing-check.sh"

# --------------------------------------------------------------- summary ----
END_FP=$(tree_fingerprint)

echo ""
echo "----------------------------------------"
printf 'gate: %d passed, %d failed' "$PASSED" "$FAILED"
if [ "$SKIPPED" -ne 0 ]; then
  printf ', %d skipped' "$SKIPPED"
fi
printf ' (%d stages registered)\n' "$REGISTERED"

# Accounting before verdicts: a mismatch here means a stage ran and was not
# counted, so the pass/fail numbers below do not describe the whole run.
ACCOUNTED=$((PASSED + FAILED + SKIPPED))

# REGISTERED == 0 first, because it is the case the mismatch test cannot see:
# 0 + 0 + 0 == 0, so a zero total would sail through and the gate would report
# success having run nothing at all. The realistic way to get here is the grep
# above failing to match -- a renamed helper, or an edit that moved the stage
# registrations out of column 0 -- which silently disarms every stage while
# still printing a green summary. A gate that can pass by running nothing is
# worse than no gate, so refuse to report on an empty run.
if [ "$REGISTERED" -eq 0 ]; then
  echo ""
  echo "$P NO STAGES REGISTERED -- the stage-count pattern matched nothing, so"
  echo "$P every check below was skipped while the summary would read 0 passed"
  echo "$P and not failed. The pattern is"
  echo "$P     $(grep -nE '^(run |run_live |fmt_ok )"' "$0" | head -1 || true)"
  echo "$P an empty match; check that every stage is still registered through"
  echo "$P run / run_live / fmt_ok at the start of a line."
  exit 1
fi

if [ "$ACCOUNTED" -ne "$REGISTERED" ]; then
  echo ""
  echo "$P ACCOUNTING MISMATCH -- $REGISTERED stages are registered but only"
  echo "$P $ACCOUNTED were accounted for ($PASSED passed, $FAILED failed, $SKIPPED"
  echo "$P skipped). A stage ran and was not counted, so the results above are"
  echo "$P incomplete and this run is not a pass. Check that every stage is"
  echo "$P registered through run / run_live / fmt_ok and that each of them"
  echo "$P increments its counter on every path."
  exit 1
fi

if [ "$SKIPPED" -ne 0 ]; then
  echo "$P the live stage did NOT run on this machine:$SKIPPED_NAMES"
  echo "$P a green run above therefore says nothing about live provider routing."
fi

if [ "$END_FP" != "$START_FP" ]; then
  echo ""
  echo "$P INVALID RUN -- the working tree changed while these checks were"
  echo "$P in progress. The results above describe a repository state that no"
  echo "$P longer exists, so they are neither a pass nor a failure. This is"
  echo "$P what concurrent mutation looks like from the gate's side; run it"
  echo "$P again with nothing else editing the tree."
  # Name the paths whose mtime advanced during the run. Diagnostic only, and
  # deliberately narrow: the gate's own `config pytest` stage advances three
  # tracked sources with identical content, so this cannot be an invalidation
  # rule. Here it just shortens the search.
  if [ -s "$MTIME_SNAP" ]; then
    MOVED=$(tracked_mtimes 2>/dev/null | diff "$MTIME_SNAP" - 2>/dev/null | grep '^>' | awk '{print $2}')
    if [ -n "$MOVED" ]; then
      echo "$P paths rewritten during the run (mtime advanced; content may or may"
      echo "$P not have changed):"
      printf '%s\n' "$MOVED" | head -15 | sed 's/^/          | /'
    fi
  fi
  rm -f "$MTIME_SNAP" 2>/dev/null
  exit 3
fi
rm -f "$MTIME_SNAP" 2>/dev/null

if [ "$FAILED" -ne 0 ]; then
  echo "$P failed:$FAILED_NAMES"
  exit 1
fi
exit 0
