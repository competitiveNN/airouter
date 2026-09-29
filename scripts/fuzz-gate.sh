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
#
# Each target is recorded together with the package that owns it, as
# "<dir><TAB><Target>". That pairing is load-bearing, not decoration: `go test
# -fuzz` refuses to run against multiple packages at once, so the target list and
# the command that consumes it have to agree on a single package per invocation.
# Discovery that recursed while the fuzz phase still said `./...` was not merely
# untested at scale -- it was guaranteed to start failing the moment a second
# package existed, with Go's "cannot use -fuzz flag with multiple packages". A
# flat target list makes that failure mode unrepresentable, because there is no
# longer a way to invoke a target without also naming its package.
mapfile -t TARGET_PAIRS < <(
  git ls-files -z -- '*_test.go' 2>/dev/null |
    while IFS= read -r -d '' f; do
      # A tracked file can be missing from the working tree (deleted, or a sparse
      # checkout). The cross-check below turns that into a clean error, but it
      # runs after this loop, so sed's own "No such file" complaint would reach
      # the log first and look like the finding. Skip, and let the set diff name
      # it properly.
      if [ ! -f "$f" ]; then
        continue
      fi
      # A leading ./ is what makes this a directory Go can resolve. Bare
      # `dirname` output is a package path ("probesub"), and `go test probesub`
      # reads that as a stdlib import -- it fails with "package probesub is not
      # in std" before the fuzzer ever starts, which the fuzz loop then reports
      # as a counterexample. The failure gets attributed to the code instead of
      # to the gate, which is worse than not running: it sends someone to debug a
      # fuzz target that is fine. The repository root is spelled "." for the same
      # reason; "." also resolves as a filesystem path.
      case "$f" in
        */*) dir="./${f%/*}" ;;
        *)   dir="." ;;
      esac
      sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\).*/\1/p' "$f" |
        while IFS= read -r t; do printf '%s\t%s\n' "$dir" "$t"; done
    done | sort -u
)

TARGETS=()
for pair in "${TARGET_PAIRS[@]}"; do
  TARGETS+=("${pair#*$'\t'}")
done

if [ "${#TARGET_PAIRS[@]}" -eq 0 ]; then
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
#
# The comparison is a SET DIFF, not a length comparison. Counting matched only
# because the script happens to run from the repository root, and it keeps
# matching whenever one untracked file is balanced by one deleted-but-tracked
# file -- so the check could pass on a tree carrying exactly the gap it exists to
# detect. `comm` in both directions reports the file git does not know about and
# the file git tracks that is not on disk. The second direction matters on its
# own: a deleted test file keeps its fuzz target in the list, and the fuzz phase
# then fails on a package that no longer builds, far from the actual cause.
mapfile -t UNTRACKED_TESTFILES < <(
  find . -name '*_test.go' -not -path './.git/*' -not -path './vendor/*' -print |
    sed 's|^\./||' | sort -u
)
mapfile -t TRACKED_TESTFILES < <(git ls-files -- '*_test.go' 2>/dev/null | sort -u)
on_disk_not_in_git=$(comm -23 \
  <(printf '%s\n' "${UNTRACKED_TESTFILES[@]}") \
  <(printf '%s\n' "${TRACKED_TESTFILES[@]}"))
in_git_not_on_disk=$(comm -13 \
  <(printf '%s\n' "${UNTRACKED_TESTFILES[@]}") \
  <(printf '%s\n' "${TRACKED_TESTFILES[@]}"))
if [ -n "$on_disk_not_in_git" ] || [ -n "$in_git_not_on_disk" ]; then
  if [ -n "$on_disk_not_in_git" ]; then
    echo "::error::test files exist that git does not track; discovery over git ls-files"
    echo "::error::would skip them and their fuzz targets with no signal. Commit them, or"
    echo "::error::add them to .gitignore deliberately:"
    printf '%s\n' "$on_disk_not_in_git"
  fi
  if [ -n "$in_git_not_on_disk" ]; then
    echo "::error::test files are tracked by git but absent from the working tree; their"
    echo "::error::fuzz targets would still be discovered and would fail to build. Restore"
    echo "::error::them (git checkout), or commit the deletion:"
    printf '%s\n' "$in_git_not_on_disk"
  fi
  exit 1
fi

echo "fuzz targets: ${TARGETS[*]}"

for pair in "${TARGET_PAIRS[@]}"; do
  dir="${pair%%$'\t'*}"
  t="${pair#*$'\t'}"
  # Seeds first. These are the committed corpus — a crasher found on any
  # machine is replayed here forever, which is what makes the gate worth having.
  # `./...` is correct HERE and only here: -run is not -fuzz, so Go happily runs
  # it across every package, and the point of the seed replay is to cover each
  # target everywhere it exists.
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
    echo "::error::it to ${dir}/testdata/fuzz/${t}/ — commit it."
    exit 1
  fi
  echo "$seed_out" | grep -E '^(ok|PASS|--- PASS: [^/]+$)' | tail -2
done

# The fuzz phase is the one that must name a single package. `go test -fuzz`
# exits 1 with "cannot use -fuzz flag with multiple packages" the moment
# `./...` resolves to more than one, so a gate written this way is not merely
# slow on a repo that grows a subpackage — it is a guaranteed red CI the day the
# second package lands, and the natural-looking fix (keep ./..., it worked
# before) is the thing that keeps it broken. Fuzzing one package at a time is
# also what Go documents, and it keeps the per-target time budget honest: under
# ./... the 20s would be shared across every package rather than spent on the
# target being reported.
#
# Verified against a deliberately added second package: the previous ./...
# invocation failed with "cannot use -fuzz flag with multiple packages" and
# reported a counterexample that did not exist.
fuzz_phase() {
  for pair in "${TARGET_PAIRS[@]}"; do
    dir="${pair%%$'\t'*}"
    t="${pair#*$'\t'}"
    echo "--- fuzzing ${t} in ${dir} for ${SECONDS_PER_TARGET}s"
    if ! go test -run '^$' -fuzz "^${t}\$" -fuzztime "${SECONDS_PER_TARGET}s" "$dir"; then
      echo "::error::$t failed in ${dir}. If Go reported a counterexample it has"
      echo "::error::written it to ${dir}/testdata/fuzz/${t}/ — commit it so it replays on"
      echo "::error::every later run. If the output above is a build or setup failure"
      echo "::error::rather than a fuzz report, the gate is broken, not the code."
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
  #
  # Scoped to the packages that actually own a fuzz target, not to the bare
  # `testdata/fuzz` path. Go writes a crasher beside the package that produced
  # it, so a target in ./probesub lands in ./probesub/testdata/fuzz -- which a
  # root-only check never sees. That is the exact case the gate was extended to
  # cover, so a check that misses it would report PASS on a tree holding an
  # uncommitted regression in the one place regressions can now occur.
  local dir
  for pair in "${TARGET_PAIRS[@]}"; do
    dir="${pair%%$'\t'*}"
    git ls-files --others --exclude-standard -- "${dir}/testdata/fuzz" 2>/dev/null
  done | sort -u
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
  local n=0 dir
  for pair in "${TARGET_PAIRS[@]}"; do
    dir="${pair%%$'\t'*}"
    [ -d "${dir}/testdata/fuzz" ] || continue
    n=$(( n + $(find "${dir}/testdata/fuzz" -type f 2>/dev/null | wc -l) ))
  done
  [ "$n" -gt 0 ] && echo "fuzz corpus: ${n} file(s) across the fuzzing packages, all tracked"
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
