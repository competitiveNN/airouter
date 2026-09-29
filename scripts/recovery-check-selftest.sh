#!/usr/bin/env bash
# Self-test for scripts/recovery-check.sh.
#
# WHY THIS EXISTS
#
# recovery-check.sh is the one script in this audit layer that touches the real
# dist/ artifacts: it fetches the bundle, replays the patch, and builds the
# result. That makes it the one whose failure is least contained -- a bug in it
# can quietly skip the very comparison it exists to perform, and the honest
# outcome of that is a green run that proved nothing.
#
# The failure mode this file guards against is concrete: recovery-check.sh is
# full of `if ! cmd; then ... exit 1; fi` and of `>/dev/null 2>&1`. A
# regression that turns any of those into a skip, or that makes a comparison
# vacuous, produces exit 0. Exit 0 is the answer this repository is relying on,
# so it has to be earned rather than observed once.
#
# The other thing it guards: a check that has never been seen to FAIL is not
# known to be able to. Every case below breaks something on purpose and asserts
# that the check notices, and every negative control asserts that the control
# edit ITSELF took effect before trusting the result. A control that silently
# did nothing is a test that passes for the wrong reason -- the round-10 lesson,
# applied to the test harness itself.
#
# Everything here runs against throwaway fixtures in a temp directory. The real
# dist/ artifacts are never read, moved, or damaged.
#
# Usage: scripts/recovery-check-selftest.sh [--keep]
#   --keep   leave the temporary repositories on disk for inspection
#
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
# Exit 2 = the harness could not run (no git, no python3, script unreadable).
set -uo pipefail

# Every case runs in every environment: the fixtures are built here rather than
# inherited, so there is no generated-artifact skip and no reduced-run total.
# Unlike the attribution self-test there is no clone-with-everything case, which
# is why this file has a single expected number.
#
#   case 1  (4)  healthy artifacts recover end to end
#   case 2  (1)  a stale artifact is refused
#   case 3  (1)  a corrupted bundle is refused
#   case 4  (1)  a patch that does not apply is refused
#   case 4b (4)  an EMPTY patch is diagnosed specifically, not just "failed",
#                and the underlying git am error is surfaced
#   case 4c (1)  a failed run leaves no temporary directory behind
#   case 4d (3)  a reduced run reports its skip count, and a full run does not
#                claim to be reduced
#   case 5  (2)  missing artifacts is "cannot run" (exit 2), not a failure
#   case 6  (1)  no upstream ref is "cannot run" (exit 2)
#   case 7  (1)  nothing unpushed is a clean exit 0
#   case 8  (1)  an unknown argument is refused (exit 2)
#   case 9  (2)  --quick really does skip the build, and says so
#   case 10 (6)  negative controls A and B, each asserted to have applied
#   case 11 (3)  a bundle exported under a CUSTOM ref name still recovers, and
#                the discovered ref is named in the output
#   case 12 (5)  an ambient UPSTREAM is honoured (a real one is actually used, a
#                missing one is refused with exit 2, and the refusal survives a
#                reduced environment) rather than silently defaulted or leaked
#
# The two controls are counted as 3 + 3 (apply, apply, behave) for A and
# 1 + 1 + 1 for B. A control that cannot apply reports the run as a FAILURE
# rather than shrinking the total, because a control that silently did nothing
# is the one failure mode this file exists to prevent.
EXPECTED=37

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO/scripts/recovery-check.sh"
[ -r "$CHECK" ] || { echo "recovery-selftest: cannot read $CHECK" >&2; exit 2; }
command -v git >/dev/null 2>&1 || { echo "recovery-selftest: git not found" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "recovery-selftest: python3 not found" >&2; exit 2; }

TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/recovery-check-selftest.XXXXXX") || exit 2
cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    echo "recovery-selftest: kept $TMPROOT"
    return
  fi
  rm -rf "$TMPROOT"
}
trap cleanup EXIT

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

# Assert on the exit status AND on expected message text. Asserting only the
# status is how a case passes because the harness is broken rather than because
# the subject behaved. Every exit-code contract here is 0/1/2, so the status is
# only half the claim.
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

g() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }

# A fixture shaped like the real repository: a real upstream ref, a real
# dist/ directory holding the real export script, a real recovery-check, and a
# little audit layer for it to verify. `upstream <dir>` marks the current HEAD
# as origin/master; everything after that is unpushed.
new_repo() {
  local name="$1"
  local dir="$TMPROOT/$name"
  mkdir -p "$dir/scripts" "$dir/dist" "$dir/docs"
  cp "$CHECK" "$dir/scripts/recovery-check.sh"
  # The audit layer is copied WHOLE, not cherry-picked. recovery-check.sh runs
  # the real self-tests and checkers in the recovered tree, so a fixture missing
  # scripts/fuzz-gate.sh or a realistic docs/audit.md would fail every recovered-
  # tree assertion for a reason that has nothing to do with recovery. The whole
  # layer is cheap and inert, so the honest fixture is the real one.
  for s in "$REPO"/scripts/*; do
    cp "$s" "$dir/scripts/" 2>/dev/null
  done
  printf 'base\n' > "$dir/README.md"
  # Double quotes, not single: bash treats a leading `#` inside single quotes
  # as a comment, which swallows the closing quote and leaves an unterminated
  # string for the rest of the file. That is not a lint nit -- it makes every
  # line after this point a syntax error, in a script whose whole job is
  # detecting syntax-level regressions.
  printf "# audit\n" > "$dir/docs/audit.md"
  printf "# unpushed\n" > "$dir/dist/README.md"
  g "$dir" init -q -b master
  g "$dir" add -A
  g "$dir" commit -qm "base commit"
  g "$dir" update-ref refs/remotes/origin/master HEAD
  echo "$dir"
}

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

# Produce genuine artifacts with the real export script, so the fixture
# artifacts are in the real format rather than a hand-rolled stand-in that might
# diverge from what the check actually parses.
write_artifacts() {
  local dir="$1" export_ref="${2:-}"
  # No EXPORT_REF override by default: recovery-check.sh fetches the bundle by
  # whatever ref the bundle itself advertises, and the default name is what a
  # reader gets. Case 11 passes an explicit name to prove the discovery holds.
  ( cd "$dir" && UPSTREAM=origin/master EXPORT_REF="$export_ref" \
      bash scripts/export-unpushed.sh >/dev/null 2>&1 )
}

run_check() {
  local dir="$1"; shift
  local out got
  out=$(cd "$dir" && bash scripts/recovery-check.sh "$@" 2>&1)
  got=$?
  printf '%s' "$out"
  return "$got"
}

# ------------------------------------------------------------------------
echo "recovery-selftest: case 1 -- healthy artifacts recover"
R=$(new_repo healthy)
add_unpushed "$R" 3
write_artifacts "$R"
out=$(run_check "$R" --quick); got=$?
assert_run "healthy artifacts pass" 0 "bundle reproduces the working tree" "$out" "$got"
assert_run "the patch replay is reported as applied" 0 "git am --3way" "$out" "$got"
assert_run "the bundle preserves commit hashes" 0 "hashes preserved" "$out" "$got"
assert_run "--recovered resolves the re-hashed citations" 0 "--recovered resolves" "$out" "$got"

echo "recovery-selftest: case 2 -- a stale artifact is refused"
# A fresh commit after the export: the artifacts no longer describe HEAD. The
# whole point of the unpushed-export job is that a handoff built from them
# would silently drop the new commit.
add_unpushed "$R" 1
out=$(run_check "$R" --quick); got=$?
assert_run "stale artifacts fail" 1 "reproduces the working tree" "$out" "$got"

echo "recovery-selftest: case 3 -- a corrupted bundle is refused"
R3=$(new_repo corrupt)
add_unpushed "$R3" 2
write_artifacts "$R3"
head -c 4096 /dev/urandom > "$R3/dist/airouter-unpushed.bundle"
out=$(run_check "$R3" --quick); got=$?
assert_run "a corrupt bundle fails" 1 "could not be fetched" "$out" "$got"

echo "recovery-selftest: case 4 -- a patch that does not apply is refused"
R4=$(new_repo badpatch)
add_unpushed "$R4" 2
write_artifacts "$R4"
# A patch that is syntactically fine and semantically wrong: it applies nowhere.
printf 'From nobody Mon Sep 17 00:00:00 2001\nSubject: [PATCH] not this history\n\n---\n not a patch\n' \
  > "$R4/dist/airouter-unpushed.patch"
out=$(run_check "$R4" --quick); got=$?
assert_run "an inapplicable patch fails" 1 "git am --3way" "$out" "$got"

echo "recovery-selftest: case 4b -- an EMPTY patch is diagnosed, not just 'failed'"
# An unpushed commit that changes no files -- an `--allow-empty` commit, or a
# merge commit -- produces a patch with no diff, and `git am` refuses it with
# "Patch is empty" unless --allow-empty is passed. That state is genuinely
# reachable for a handoff, and it is NOT context drift, so reporting it as a
# generic apply failure sends the reader hunting for a problem that isn't there.
#
# This case was written because the CI job that runs recovery-check.sh had
# exactly this bug: its fixture used `git commit --allow-empty`, so the job was
# generating a patch no reader could apply, and every other assertion in the
# job still passed. The check was right; the fixture was wrong, and only a check
# that actually replays the patch could tell.
R4B=$(new_repo emptypatch)
add_unpushed "$R4B" 1
g "$R4B" commit -q --allow-empty -m "an empty commit"
write_artifacts "$R4B"
out=$(run_check "$R4B" --quick); got=$?
assert_run "an empty patch fails" 1 "git am --3way" "$out" "$got"
assert_run "an empty patch is diagnosed specifically" 1 "EMPTY patch" "$out" "$got"
assert_run "the empty-patch advice names the fix" 1 "allow-empty" "$out" "$got"
# The real git am output must be shown, not swallowed. A check that discards the
# underlying error and prints only its own summary is the reason this took a
# round to find.
if printf '%s' "$out" | grep -qi 'patch is empty'; then
  ok "the underlying git am error is surfaced"
else
  bad "the underlying git am error is surfaced"
fi

echo "recovery-selftest: case 4c -- a failed run leaves no temporary directory"
# The throwaway repos are created under $TMPDIR and removed by an EXIT trap. A
# failure path that exits before the trap is installed -- or that bypasses it --
# would leave a full clone of the repository in the temp directory on every
# failed run, which is the kind of leak nobody notices until their disk is full.
# Counted directly rather than inferred from the absence of a failure.
BEFORE=$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'recovery-check.*' 2>/dev/null | wc -l)
out=$(run_check "$R4" --quick); got=$?   # known-failing fixture
AFTER=$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'recovery-check.*' 2>/dev/null | wc -l)
if [ "$AFTER" -eq "$BEFORE" ]; then
  ok "a failed run leaves no temporary directory behind"
else
  bad "a failed run leaked a temporary directory ($BEFORE -> $AFTER)"
fi

echo "recovery-selftest: case 4d -- a reduced run SAYS it was reduced"
# A green line reading "13 passed, 0 failed" does not tell the reader whether
# four of those checks were skipped for lack of anything to check. A run that
# quietly checked less than it appears to is the failure mode this repository
# has been bitten by repeatedly, so the summary has to state the skip count.
# The synthetic fixtures genuinely trigger the skip path -- they have no Go
# sources and no audit anchors -- which is what makes this case meaningful.
R4D=$(new_repo reduced)
add_unpushed "$R4D" 1
write_artifacts "$R4D"
out=$(run_check "$R4D" --quick); got=$?
assert_run "a reduced run still passes" 0 "passed, 0 failed" "$out" "$got"
if printf '%s' "$out" | grep -qF 'SKIPPED'; then
  ok "a reduced run reports its skip count in the summary"
else
  bad "a reduced run reports its skip count in the summary"
fi
# And the converse: the real repository has nothing to skip, so a full run must
# NOT claim to be reduced. Asserting only one direction would let a check that
# always prints "SKIPPED" pass.
out=$(run_check "$REPO" --quick); got=$?
# Exit 0 and no "passed, 0 failed" message requirement: the real repository
# legitimately produces a different success message depending on whether it has
# unpushed commits. A full checkout reports its tally; a fresh clone, where
# nothing is unpushed, exits 0 early with "nothing to recover". Both are
# correct, and asserting on the tally made this case fail at 28/29 in a clone --
# the same class of bug as the UPSTREAM leak, in the assertion rather than the
# code under test.
if [ "$got" -eq 0 ]; then
  ok "the real repository passes"
else
  bad "the real repository passes (exit $got, want 0)"
fi
if printf '%s' "$out" | grep -qF 'SKIPPED'; then
  bad "a full run does not claim to be reduced"
else
  ok "a full run does not claim to be reduced"
fi

echo "recovery-selftest: case 5 -- artifacts missing is 'cannot run', not 'failed'"
# Exit 2, not 1. A checkout with no generated artifacts has nothing to say
# about recovery, and reporting that as a broken recovery would push the next
# person towards committing them -- the self-referential loop
# scripts/export-unpushed.sh exists to avoid.
R5=$(new_repo noartifacts)
add_unpushed "$R5" 1
out=$(run_check "$R5" --quick); got=$?
assert_run "missing artifacts exit 2" 2 "recovery artifacts are missing" "$out" "$got"

rm -f "$R5/dist/airouter-unpushed.bundle"
out=$(run_check "$R5" --quick); got=$?
assert_run "a missing bundle alone still exits 2" 2 "recovery artifacts are missing" "$out" "$got"

echo "recovery-selftest: case 6 -- no upstream ref is 'cannot run'"
R6=$(new_repo noupstream)
add_unpushed "$R6" 2
write_artifacts "$R6"
g "$R6" update-ref -d refs/remotes/origin/master
out=$(run_check "$R6" --quick); got=$?
assert_run "no upstream ref exits 2" 2 "does not exist" "$out" "$got"

echo "recovery-selftest: case 7 -- nothing unpushed is a clean exit 0"
R7=$(new_repo allpushed)
write_artifacts "$R7"
out=$(run_check "$R7" --quick); got=$?
assert_run "nothing unpushed exits 0" 0 "nothing to recover" "$out" "$got"

echo "recovery-selftest: case 8 -- an unknown argument is refused"
out=$(run_check "$R" --nonsense); got=$?
assert_run "an unknown argument exits 2" 2 "unknown argument" "$out" "$got"

echo "recovery-selftest: case 9 -- --quick really does skip the build"
# Otherwise --quick is untested and could quietly grow a full build, which
# would make this self-test's own runtime a lie. The assertion is on the
# message, because a check that SKIPPED for a different reason would say
# something else and still exit 0.
R9=$(new_repo quick)
add_unpushed "$R9" 2
write_artifacts "$R9"
out=$(run_check "$R9" --quick); got=$?
assert_run "--quick succeeds" 0 "skipping build and tests" "$out" "$got"
if printf '%s' "$out" | grep -qF 'recovered tree builds'; then
  bad "--quick does not build (it reported a build)"
else
  ok "--quick does not build"
fi

echo "recovery-selftest: case 11 -- a CUSTOM export ref name still recovers"
# export-unpushed.sh honours EXPORT_REF, so a bundle under any name is a bundle
# a reader can legitimately hold. recovery-check.sh must therefore ask the
# bundle which ref its tip is on rather than assuming the script's own default.
#
# This is not hypothetical: an ad-hoc probe of the real bundle reported a false
# "MISMATCH" purely because it looked for refs/heads/master while the tip
# actually sat at refs/heads/airouter-unpushed-export. A hardcoded name is a
# guess, and a guess that fails here looks exactly like a stale artifact.
RC=$(new_repo customref)
add_unpushed "$RC" 2
write_artifacts "$RC" "my-handoff-ref"
# The fixture is only meaningful if the bundle really is under the custom name.
# Assert the fixture first: a case that passes because the export ignored
# EXPORT_REF would look identical to one that passes because discovery works.
bundle_ref=$(git -C "$RC" bundle list-heads "$RC/dist/airouter-unpushed.bundle" 2>/dev/null \
  | awk 'NR==1{print $2}')
if [ "$bundle_ref" = "refs/heads/my-handoff-ref" ]; then
  ok "the fixture bundle really is under the custom ref"
else
  bad "the fixture bundle is under '${bundle_ref:-<none>}', not the custom ref"
fi
out=$(run_check "$RC" --quick); got=$?
assert_run "a custom export ref still recovers" 0 "bundle reproduces the working tree" "$out" "$got"
# And the run must SAY which ref it used, so a reader debugging a differently
# named bundle is not left to rediscover it.
if printf '%s' "$out" | grep -qF 'refs/heads/my-handoff-ref'; then
  ok "the discovered tip ref is named in the output"
else
  bad "the discovered tip ref is not named in the output"
fi

echo "recovery-selftest: case 12 -- the ambient UPSTREAM contract holds"
# An earlier version of this case asserted that recovery-check.sh pins UPSTREAM
# before running the self-tests in the recovered tree. That assertion was
# VACUOUS and was removed: with the pin removed the run still passed, because
# dist-freshness-selftest.sh pins UPSTREAM on its own command line, so the
# boundary pin was redundant defense-in-depth rather than the fix it claimed to
# be. A test that passes with the feature deleted tests nothing.
#
# What IS load-bearing, and what this case now checks, is the narrower and
# honest contract: recovery-check.sh honours the caller's UPSTREAM. Asserting it
# by reading the code would prove nothing, so it is asserted by running -- and
# the exit-2 assertions below are the falsifiable half. A script that ignored
# UPSTREAM and fell back to its own default returns 0 where these require 2,
# which was confirmed against a mutant that hardcodes the default.
#
# The self-tests run in the RECOVERED tree, where the base ref is named
# origin/master, while a CI caller sets UPSTREAM=base in the ambient
# environment. That variable is exported into every child, so without an
# explicit pin at the call boundary a self-test could compare against a ref
# that does not exist in the tree it just recovered -- and report a failure
# that looks like a broken recovery but is really a leaked environment.
#
# Asserting this by reading the code would prove nothing, so it is asserted by
# running: give the check an ambient UPSTREAM that is valid in the fixture but
# is NOT the recovered tree's own upstream, and require the run to still pass.
R12=$(new_repo poisoned)
add_unpushed "$R12" 2
write_artifacts "$R12"
# A ref that exists in the fixture and is one commit behind HEAD -- a poisoned
# value that is plausible enough to pass a loose "does this ref exist" check.
# update-ref, not `branch -f`: `branch -f` on a ref that does not exist yet is
# refused, which would leave `base` undefined and make the case fail for a reason
# of its own making. The SHA is resolved from origin/master BEFORE it is walked
# back, so this does not depend on how many commits the fixture happens to have.
POISON_SHA=$(git -C "$R12" rev-parse refs/remotes/origin/master)
g "$R12" update-ref refs/heads/base "$POISON_SHA"
if g "$R12" rev-parse --verify --quiet refs/heads/base >/dev/null 2>&1; then
  ok "the poisoned fixture ref really exists"
else
  bad "the poisoned fixture ref was not created; case 12 would prove nothing"
fi
out=$(cd "$R12" && UPSTREAM=base bash scripts/recovery-check.sh --quick 2>&1); got=$?
assert_run "a poisoned ambient UPSTREAM does not break the recovered tree" 0 \
  "bundle reproduces the working tree" "$out" "$got"
# It must actually have been used, or the case would pass for the wrong reason:
# the check has to name the base it was given, which is only true if it honoured
# the caller's UPSTREAM rather than quietly substituting its own default.
if printf '%s' "$out" | grep -qE 'base [0-9a-f]{7,}'; then
  ok "the check did run against the ambient UPSTREAM it was given"
else
  bad "the check did not report the ambient UPSTREAM base it was given"
fi

# The same poisoning, but with a ref that does NOT exist. This is the falsifiable
# half: a check that silently fell back to its own default here would report a
# full green run, and the case above would still pass -- so only this one can
# tell a working UPSTREAM contract from a hardcoded one.
out=$(cd "$R12" && UPSTREAM=refs/heads/does-not-exist \
  bash scripts/recovery-check.sh --quick 2>&1); got=$?
assert_run "a nonexistent ambient UPSTREAM is refused, not silently defaulted" 2 \
  "does not exist; cannot build a recovery base" "$out" "$got"
# A reduced environment must behave identically. If the contract held only
# because of something the caller's shell happened to export, a CI runner with a
# cleaner environment would take a different path through the same code.
out=$(cd "$R12" && env -i PATH=/usr/bin:/bin HOME="${HOME:-/root}" \
  UPSTREAM=refs/heads/does-not-exist \
  bash scripts/recovery-check.sh --quick 2>&1); got=$?
assert_run "the same refusal holds in a reduced environment" 2 \
  "does not exist; cannot build a recovery base" "$out" "$got"

echo "recovery-selftest: case 10 -- negative controls"
# Two edits, each applied to a COPY of the check. A control is only meaningful
# if the edit actually took effect, so each one is asserted to have applied
# BEFORE its result is trusted -- the round-10 lesson, where a sed pattern that
# matched nothing produced a passing test that proved nothing.
#
# Control A: remove the tree comparison entirely. `tree_matches` is the assertion
# that the recovery reproduced the work. Without it the check still runs every
# other step -- it still fetches, still replays, still builds, still runs the
# self-tests -- and still exits 0 on a stale artifact. That is the exact
# regression this file exists to make visible: green, and worthless.
CN="$TMPROOT/neutered"
A="$TMPROOT/control-a.sh"
# The WHOLE conditional is removed, body included. Deleting only the `if` line
# would leave its `fi` orphaned and the neutered copy would fail to parse -- which
# would be a red run that proves nothing about the behaviour under test.
python3 - "$CHECK" "$A" <<'PYDEL'
import re, sys
src = open(sys.argv[1]).read()
# BOTH staleness detectors are removed. There are two, independently: the tree
# comparison and the bundle-tip comparison. Removing only one leaves the other
# still catching the stale artifact, so the control would report "it still
# failed" and prove nothing. A control is only meaningful if the check has
# nothing left to fail with.
out, n = re.subn(
    r'\n  if \[ "\$want" != "\$have" \]; then\n'
    r'    bad "\$label \(tree \$have, want \$want\)"\n'
    r'    return 1\n  fi\n',
    '\n', src)
if n != 1:
    sys.exit("control A: tree comparison: expected 1 substitution, made %d" % n)
# The tip comparison line is matched literally, parens included. A regex for
# `$( ... )` has to be escaped and getting that wrong is how a control silently
# applies to nothing -- which the assertions below then catch, but only because
# they were written to check the RESULT rather than assume it.
# The WHOLE if/else/fi is removed. Rewriting the condition to `if false` would
# leave the `else` branch -- which is the `bad` call -- still reachable, so the
# check would keep failing on exactly what the control meant to remove.
TIPBLOCK = '''if [ "$BUNDLE_TIP" = "$(git -C "$REPO" rev-parse HEAD)" ]; then
  ok "bundle tip equals HEAD (hashes preserved, not just content)"
else
  bad "bundle tip $(git -C "$B" rev-parse --short HEAD) != HEAD"
fi'''
n2 = out.count(TIPBLOCK)
if n2 != 1:
    sys.exit("control A: tip comparison: expected 1 occurrence, found %d" % n2)
out = out.replace(TIPBLOCK, '# (control A: tip comparison removed)')
open(sys.argv[2], 'w').write(out)
PYDEL
if [ $? -ne 0 ] || cmp -s "$CHECK" "$A"; then
  printf '  skip control A: the edit did not apply, so the control would have\n' >&2
  printf '               proved nothing; fix the pattern before trusting it\n' >&2
  SKIPPED_A=1
else
  SKIPPED_A=0
  if grep -qF '"$want" != "$have"' "$A"; then
    bad "control A applied (the tree comparison was removed)"
  else
    ok "control A applied (the tree comparison was removed)"
  fi
  if grep -qF 'control A: tip comparison removed' "$A"; then
    ok "control A applied (the bundle tip comparison was removed)"
  else
    bad "control A applied (the bundle tip comparison was removed)"
  fi
  cp -r "$R9" "$CN"
  cp "$A" "$CN/scripts/recovery-check.sh"
  bash -n "$CN/scripts/recovery-check.sh" 2>/dev/null \
    && ok "control A applied (the neutered copy still parses)" \
    || bad "control A: the neutered copy does not parse, so the control is void"
  write_artifacts "$CN"
  # Same fixture state as case 2, which is a known failure with the real check.
  add_unpushed "$CN" 1
  out=$(run_check "$CN" --quick); got=$?
  # If the neutered copy still returned 1, it would be detecting the staleness
  # some other way and the control would prove nothing.
  if [ "$got" -eq 0 ]; then
    ok "control A: without the tree comparison a stale artifact passes anyway"
  else
    bad "control A: a stale artifact still failed with the comparison removed (exit $got)"
  fi
fi

# Control B: neuter the --recovered verification. The whole reason the patch
# path is checked is that `git am` re-hashes every commit; if --recovered
# silently stopped resolving, the handoff would break at exactly the moment
# someone relies on it, which is the one moment nobody is watching.
CB="$TMPROOT/control-b-repo"
B="$TMPROOT/control-b.sh"
python3 - "$CHECK" "$B" <<'PYDEL'
import sys
src = open(sys.argv[1]).read()
old = 'if python3 scripts/audit-attribution-check.py --recovered --against "$REPO" >/dev/null 2>&1; then'
if src.count(old) != 1:
    sys.exit("control B: pattern found %d times" % src.count(old))
open(sys.argv[2], 'w').write(src.replace(old, 'if false; then'))
PYDEL
if [ $? -ne 0 ] || cmp -s "$CHECK" "$B"; then
  printf '  skip control B: the edit did not apply, so the control would have\n' >&2
  printf '               proved nothing; fix the pattern before trusting it\n' >&2
  SKIPPED_B=1
else
  SKIPPED_B=0
  if grep -qF -- '--recovered --against "$REPO" >/dev/null' "$B"; then
    bad "control B applied (the --recovered verification was removed)"
  else
    ok "control B applied (the --recovered verification was removed)"
  fi
  cp -r "$R9" "$CB"
  cp "$B" "$CB/scripts/recovery-check.sh"
  write_artifacts "$CB"
  out=$(run_check "$CB" --quick); got=$?
  # The neutered --recovered branch reports itself as a FAILURE line, because
  # that is the whole point: the check cannot resolve the re-hashed citations
  # and says so. Asserting on the "cannot resolve" text is what distinguishes
  # this from a run that failed for some unrelated reason.
  assert_run "control B: --recovered is genuinely exercised" 1 \
    "--recovered cannot resolve the re-hashed citations" "$out" "$got"
fi

echo ""
echo "----------------------------------------"
if [ "${SKIPPED_A:-0}" -eq 1 ] || [ "${SKIPPED_B:-0}" -eq 1 ]; then
  # A control that could not run must not look like a clean run. The count
  # contract below is what would have caught it silently.
  printf 'recovery-selftest: reduced run: %d passed, %d failed (a control was skipped)\n' \
    "$PASSED" "$FAILED" >&2
  exit 1
fi
printf 'recovery-selftest: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$PASSED" -eq "$EXPECTED" ] || bad "assertion count matches the contract (got $PASSED, expected $EXPECTED)"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
