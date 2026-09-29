#!/usr/bin/env bash
# Replay every Fuzz target's seed corpus, then fuzz each one for a bounded time.
#
# WHY DISCOVERY RATHER THAN A HARDCODED LIST
#
# A CI step that names one target is coverage for that target and silence for
# every other. Adding a second fuzzer looks like it is covered (the seeds run
# under `go test ./...`) while nothing actually explores it, and renaming or
# deleting a target leaves a step pointing at nothing. Both failures are
# invisible: the command still exits 0, or is not run at all.
#
# So the target list is derived from the source on every run. A new fuzzer is
# picked up without touching this script, and a renamed one is covered under its
# new name. If discovery finds nothing, that is an error rather than an empty
# loop, because a gate that quietly covers nothing is worse than no gate.
#
# Usage: scripts/fuzz-gate.sh [seconds-per-target]
#        FROZEN=1 scripts/fuzz-gate.sh     # seeds only, no fuzzing
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 2

# Validate the duration BEFORE it reaches `go test -fuzztime`, which takes a
# silently-invalid value and reports "invalid duration" as if it were a fuzz
# failure. Two distinct mistakes used to land on that same message:
#
#   * FROZEN=1 was documented as the way to skip fuzzing, but it is an
#     environment variable, so `FROZEN=1 ./fuzz-gate.sh` (the near-universal
#     form) binds it to $1. It then became the duration, "FROZEN" was passed
#     to -fuzztime, and the script reported "found a counterexample" and exited
#     1 — pointing the reader at testdata/fuzz for a crasher that does not
#     exist. A flag that lies about what it did is worse than a missing one.
#   * A typo ("20x", "30s") likewise became a fake counterexample rather than
#     a usage error.
#
# So a non-integer argument is a usage error here, and FROZEN is read only
# from the environment, never from the positional slot.
SECONDS_PER_TARGET="${1:-20}"
if ! [[ "$SECONDS_PER_TARGET" =~ ^[0-9]+$ ]]; then
  echo "::error::seconds-per-target must be a non-negative integer, got '${SECONDS_PER_TARGET}'."
  echo "::error::For a bare env var use: FROZEN=1 scripts/fuzz-gate.sh   (FROZEN=1 with a"
  echo "::error::space, not '=1 FROZEN=1'). Seeds only, no fuzzing."
  exit 2
fi
if [ "${FROZEN:-0}" = "1" ]; then
  if [ "$#" -gt 0 ]; then
    echo "::error::FROZEN=1 and an explicit duration are mutually exclusive: the env var"
    echo "::error::means seeds only, and an argument is a request to fuzz. Pick one."
    exit 2
  fi
  SECONDS_PER_TARGET=0
fi

# Discovery walks every package directory, not just the repository root. A
# root-only `ls *_test.go` finds the two targets that happen to live there and
# silently misses every target added under a subdirectory — the same
# coverage-that-looks-like-coverage failure as a hardcoded list, except it also
# survives someone auditing the list. `git ls-files` supplies the paths, so
# vendor and build output are excluded, and each path is passed to sed as one
# quoted argument: an unquoted expansion would split on whitespace in a
# filename and turn one unreadable file into a silently shorter target list.
mapfile -t TARGETS < <(
  git ls-files -z -- '*_test.go' 2>/dev/null |
    while IFS= read -r -d '' f; do
      sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\).*/\1/p' "$f"
    done | sort -u
)

if [ "${#TARGETS[@]}" -eq 0 ]; then
  echo "::error::no Fuzz targets found; the discovery pattern no longer matches, so this"
  echo "::error::gate is covering nothing. Fix the discovery command rather than removing"
  echo "::error::the step. Note this also fires when git ls-files finds no *_test.go"
  echo "::error::(wrong worktree, or the files are untracked — commit them)."
  exit 1
fi

# Cross-check discovery against the repository's own view of the test corpus.
# If a *_test.go file is not tracked by git, `git ls-files` skips it and its
# fuzz targets vanish from this list without any signal — reintroducing exactly
# the silent gap the recursive walk above exists to close. Listing every
# *_test.go actually on disk and diffing the two sets turns that into a loud
# failure.
mapfile -t UNTRACKED_TESTFILES < <(
  find . -name '*_test.go' -not -path './.git/*' -not -path './vendor/*' -print |
    sed 's|^\./||' | sort
)
mapfile -t TRACKED_TESTFILES < <(git ls-files -- '*_test.go' 2>/dev/null | sort)
if [ "${#UNTRACKED_TESTFILES[@]}" -ne "${#TRACKED_TESTFILES[@]}" ]; then
  echo "::error::test files exist that git does not track; discovery over git ls-files"
  echo "::error::would skip them and their fuzz targets with no signal. Commit them, or"
  echo "::error::add them to .gitignore deliberately:"
  comm -23 <(printf '%s\n' "${UNTRACKED_TESTFILES[@]}") <(printf '%s\n' "${TRACKED_TESTFILES[@]}")
  exit 1
fi

echo "fuzz targets: ${TARGETS[*]}"

for t in "${TARGETS[@]}"; do
  # Seeds first. These are the committed corpus — a crasher found on any
  # machine is replayed here forever, which is what makes the gate worth having.
  echo "--- replaying seed corpus: $t"
  # PIPESTATUS, not the pipeline's exit code. `go test ... | tail -3` reports
  # tail's status, so a panicking or failing corpus still exits 0 and the gate
  # passes — verified by breaking a seed and watching this exact bug. Output is
  # captured first and printed after, which also keeps the full log on failure
  # instead of only the last three lines.
  # The exit status is taken from the assignment itself, which carries the
  # command's status, so a failing corpus cannot be masked by the `$?` that
  # follows a bare command.
  seed_out=$(go test -run "^${t}\$" -v ./... 2>&1)
  if [ $? -ne 0 ]; then
    echo "$seed_out" | tail -20
    echo "::error::seed corpus failed for $t. If a crasher was found, Go has written"
    echo "::error::it to testdata/fuzz/${t}/ — commit it."
    exit 1
  fi
  echo "$seed_out" | grep -E '^(ok|PASS|--- PASS: [^/]+$)' | tail -2
done

fuzz_phase() {
  for t in "${TARGETS[@]}"; do
    echo "--- fuzzing ${t} for ${SECONDS_PER_TARGET}s"
    if ! go test -run '^$' -fuzz "^${t}\$" -fuzztime "${SECONDS_PER_TARGET}s" ./...; then
      echo "::error::$t found a counterexample. Go has written it to"
      echo "::error::testdata/fuzz/${t}/ — commit it so it replays on every later run."
      return 1
    fi
  done
}

# A crasher that is not committed is a regression nobody can reproduce. Go
# writes it to the working tree automatically; this is the only place that
# notices.
#
# UNTRACKED, not "present". Testing for existence inverts the rule and makes the
# gate contradict its own error message: that message tells you to commit the
# crasher, and a committed crasher then trips an existence test on every run
# forever. Committing the regression test the gate just asked for would turn CI
# permanently red with no way to make it green. Git already knows the difference
# between "here" and "committed", so ask git.
untracked_crashers() {
  # --exclude-standard is required: .gitignore is expected to cover the bulk
  # generated corpus, and this must report only the minimized crashers that Go
  # drops into testdata/fuzz and that a human is being asked to commit.
  git ls-files --others --exclude-standard -- testdata/fuzz 2>/dev/null
}

check_uncommitted_crashers() {
  local untracked
  untracked=$(untracked_crashers)
  if [ -n "$untracked" ]; then
    echo "::error::a fuzz crasher is present in the working tree but is not"
    echo "::error::committed. Commit it — Go replays testdata/fuzz on every"
    echo "::error::subsequent run, which is what makes it a regression test."
    printf '%s\n' "$untracked"
    return 1
  fi
  # Informational only, and deliberately not a conditional. `[ "$n" -gt 0 ] && echo`
  # as the last statement of the function makes the function return the
  # predicate's status, so a healthy tree with no corpus (n=0, the normal case
  # before the first crasher exists) makes this function return 1 — and both
  # call sites treat non-zero as "a crasher was lost", turning the gate
  # permanently red with an error message about a file that does not exist.
  if [ -d testdata/fuzz ]; then
    local n
    n=$(find testdata/fuzz -type f 2>/dev/null | wc -l | tr -d ' ')
    echo "testdata/fuzz: ${n} corpus file(s), all tracked"
  fi
  return 0
}

# FROZEN=1 skips only the fuzzing phase. The crasher check still runs: a
# previous run that found a counterexample and dropped it in the tree has
# already lost it, and replaying the committed seeds passes cleanly over the
# top of that, so exiting early here would report PASS on a tree holding an
# unreproducible regression.
if [ "$SECONDS_PER_TARGET" -le 0 ]; then
  echo "FROZEN=1: seed corpora only, skipping the fuzzing phase"
  check_uncommitted_crashers || exit 1
  echo "fuzz-gate: PASS (${#TARGETS[@]} target(s), seeds only)"
  exit 0
fi

fuzz_phase || exit 1

check_uncommitted_crashers || exit 1

echo "fuzz-gate: PASS (${#TARGETS[@]} target(s), ${SECONDS_PER_TARGET}s each)"
