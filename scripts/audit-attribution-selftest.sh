#!/usr/bin/env bash
# Self-test for scripts/audit-attribution-check.py.
#
# WHY THIS EXISTS
#
# The checker guards the one thing the rest of the audit layer cannot: whether
# the documentation's claims about COMMITS are true. audit-drift-check.py
# validates that a symbol still exists; it says nothing about whether the fix a
# finding describes was applied. That gap had a concrete cost -- a round wrote
# that audit-drift-check.py had been changed, verified it, then a `git reset
# --hard` during unrelated probing destroyed it, and every existing check kept
# passing because the anchors were all still resolvable. The finding was right
# about the code and wrong about the commit.
#
# A guard that important needs its own test, and it needs one more than the
# others because it has two pieces of logic that are easy to get subtly wrong in
# a way that only LOOKS correct:
#
#   * the self-reference exemption. dist/README.md lists the unpushed commits so
#     a handoff can be built from it, but writing the list creates a commit,
#     which makes the list wrong, which requires rewriting it. The exemption for
#     the commit that delivers the list is what makes the check converge. The
#     first version of that exemption was "the diff touches dist/README.md",
#     which is trivially gameable: attach a one-line README tweak to a real
#     code commit and the whole commit escapes the list. Case 4 is that exploit,
#     run against both the narrow rule and the old one.
#
#   * --recovered mode. `git am` re-hashes every commit, so in a recovered tree
#     every citation fails by construction. Rather than document "this is
#     expected to fail", the checker grew a mode that matches citations by
#     content via `git patch-id --stable`. A mode that relaxes matching is
#     exactly the kind of thing that turns into "accepts anything", so cases 5
#     and 6 require it to pass on an honest citation and still FAIL on a
#     fabricated one.
#
# WHY NEGATIVE CONTROLS USE COPIES
#
# Every negative control below copies the script into a scratch tree and
# modifies the COPY. Mutating the real script even briefly is how the round-10
# fix was lost in the first place; a test harness that repeats the accident is
# worse than no harness.
#
# USAGE
#   scripts/audit-attribution-selftest.sh [--keep]
#     --keep   leave the temporary repositories on disk for inspection
#
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
# Exit 2 = the harness could not run (no git, no python3, script unreadable).
set -uo pipefail

# The number of assertions a complete run makes. Pinning it is what makes a
# silent loss of coverage visible: this suite skips cases in a bare clone (the
# generated patch is absent, and there are no unpushed commits to check), so the
# passing total legitimately varies by environment. What must NOT vary is how
# many assertions run when the environment IS complete. A case that starts
# skipping for a new reason, or one that stops running, drops this count, and
# the run reports it rather than quietly looking fine.
#
# The two cases that vary:
#
#   case 1    (1 assertion) runs only when this checkout has unpushed commits.
#             A clone that already has them all skips it with a stated reason.
#   cases 5-6 (7 assertions) run only when the generated patch is present.
#             Cases 2, 2b, 3, 4 and 7 (10 assertions) need nothing but git.
#
#   full run  = 1 + 10 + 7 = 18
#   clone run = 10 + 0     = 11
#
# The two leading assertions in case 5 are the base-discovery controls ("the
# replay base is origin/master by SHA" and "the base does not already contain the
# work"); they are counted in the 7 above. The second used to be "the wrong base
# is rejected", which replayed the patch onto HEAD and required git am to refuse
# it -- and that stopped refusing once the unpushed set shrank to two commits,
# because git recognises an already-applied patch and skips it. The property is
# the base not containing the work, so that is what is asserted now. If a case is
# moved out of the environment-dependent branch, or one is added, this arithmetic
# is what goes stale, and the run reports it.
EXPECTED_FULL=18
EXPECTED_CLONE=11

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO/scripts/audit-attribution-check.py"
[ -r "$CHECK" ] || { echo "attribution-selftest: cannot read $CHECK" >&2; exit 2; }
command -v git >/dev/null 2>&1 || { echo "attribution-selftest: git not found" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "attribution-selftest: python3 not found" >&2; exit 2; }

TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/audit-attribution-selftest.XXXXXX") || exit 2
cleanup() {
  [ "$KEEP" -eq 1 ] && { echo "attribution-selftest: kept $TMPROOT"; return; }
  rm -rf "$TMPROOT"
}
trap cleanup EXIT

PASSED=0
FAILED=0
SKIPPED_RECOVERY=0
SKIPPED_CASE1=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

# Assert on the status AND on expected message text. Asserting only the status
# is how a case passes because the harness is broken rather than because the
# subject behaved.
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

assert_absent() {
  if printf '%s' "$2" | grep -qF -- "$3"; then
    bad "$1 (did not expect: $3)"
  else
    ok "$1"
  fi
}

git_q() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }

# Every fixture MUST have a refs/remotes/origin/master, because the checker
# looks for exactly that ref and SKIPS the whole unpushed-list check without
# one, exiting 0. A fixture missing it therefore "passes" while asserting
# nothing -- the exact vacuity this repository has been bitten by twice. The
# base commit is the upstream; everything committed after it is unpushed.
pin_upstream() {
  local dir="$1"
  git -C "$dir" update-ref refs/remotes/origin/master HEAD
}

# new_repo <name> -- a real git repository with a copy of the checker, an audit
# document and a dist/README.md. Unlike the drift self-test this MUST be a real
# repository: the checker reads history, refs and per-commit diffs, so there is
# nothing to stub.
#
# The layout mirrors the real one: origin/master points at a base commit, and
# later commits on top are what the unpushed-list check reasons about.
new_repo() {
  local name="$1"
  local dir="$TMPROOT/$name"
  mkdir -p "$dir/scripts" "$dir/docs" "$dir/dist"
  cp "$CHECK" "$dir/scripts/audit-attribution-check.py"
  printf 'base\n' > "$dir/README.md"
  git_q "$dir" init -q -b master
  git_q "$dir" add -A
  git_q "$dir" commit -qm "base commit"
  pin_upstream "$dir"
  echo "$dir"
}

# base_sha <dir>
base_sha() { git -C "$1" rev-parse --short HEAD; }

echo "attribution-selftest: case 1 -- the real repository passes"
# Only meaningful where the real repository actually has unpushed commits. In a
# clone that has everything, origin/master equals HEAD, the handoff list is
# legitimately stale, and the checker is RIGHT to report it. Asserting a pass
# there would be asserting that a correct report is a bug, so the case is
# skipped with that stated rather than turned into a false failure.
UNPUSHED_COUNT=$(git -C "$REPO" rev-list --count origin/master..HEAD 2>/dev/null || echo 0)
if [ "$UNPUSHED_COUNT" -eq 0 ]; then
  printf '  skip case 1: this checkout has no unpushed commits, so there is no\n'
  printf '           handoff list to be accurate about (a clone with everything)\n' >&2
  SKIPPED_CASE1=1
else
  out=$(cd "$REPO" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
  assert_run "real repository passes" 0 "all citations resolve" "$out" "$got"
fi

echo "attribution-selftest: case 2 -- a fabricated citation is reported"
R=$(new_repo fabricated)
B=$(base_sha "$R")
printf '# audit\n\nCited `deadbee`, which never existed.\n' > "$R/docs/audit.md"
printf '# unpushed\n' > "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "cite a fabricated hash"
out=$(cd "$R" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
assert_run "fabricated hash fails" 1 "deadbee" "$out" "$got"
assert_run "fabricated hash is named" 1 "which is not a commit" "$out" "$got"

echo "attribution-selftest: case 2b -- a DANGLING commit is not a valid citation"
# A commit that was reset away still sits in the object database, and
# `git cat-file -e` still resolves it. A citation naming one therefore passes in
# the working tree where it was written and fails in every clone -- green here,
# red exactly where the work is handed off. This case builds that state and
# requires the check to reject it, which reachability-from-HEAD does and
# cat-file alone does not.
RD=$(new_repo dangling)
B=$(base_sha "$RD")
printf '# audit\n' > "$RD/docs/audit.md"
printf '# unpushed\n' > "$RD/dist/README.md"
git_q "$RD" add -A && git_q "$RD" commit -qm "add the handoff documents"
# A commit on a throwaway branch, then abandoned: present in the object
# database, not reachable from HEAD.
git_q "$RD" checkout -q -b throwaway
printf 'gone\n' > "$RD/scratch.txt"
git_q "$RD" add -A && git_q "$RD" commit -qm "this commit will be abandoned"
DANGLING=$(base_sha "$RD")
git_q "$RD" checkout -q master
git_q "$RD" branch -q -D throwaway
if ! git_q "$RD" cat-file -e "$DANGLING^{commit}" 2>/dev/null; then
  printf '  skip case 2b: the abandoned commit was pruned before the check ran\n' >&2
else
  printf '# audit\n\nThis cites `%s`, which was abandoned.\n' "$DANGLING" > "$RD/docs/audit.md"
  git_q "$RD" add -A && git_q "$RD" commit -qm "cite an abandoned commit"
  out=$(cd "$RD" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
  assert_run "a dangling commit is rejected" 1 "$DANGLING" "$out" "$got"
  assert_run "the dangling hash is named" 1 "which is not a commit" "$out" "$got"
fi

echo "attribution-selftest: case 3 -- a real unpushed commit must be listed"
R=$(new_repo unpushed)
B=$(base_sha "$R")
printf '# audit\n' > "$R/docs/audit.md"
printf '# unpushed\n' > "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "add the handoff documents"
S=$(base_sha "$R")
# A genuine code commit that the list has not caught up with.
printf 'package main\n' > "$R/main.go"
git_q "$R" add -A && git_q "$R" commit -qm "a real code change"
C=$(base_sha "$R")
out=$(cd "$R" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
assert_run "unlisted code commit is reported" 1 "missing from dist/README.md" "$out" "$got"
assert_run "the missing commit is named" 1 "$C" "$out" "$got"

echo "attribution-selftest: case 4 -- the exemption cannot be gamed"
# The exploit: a commit carrying a real code change AND a one-line README tweak.
# Under the old "touches dist/README.md" rule it was exempt and escaped the
# list entirely. Under the narrow rule it is confined to nothing but the
# handoff artifacts, so it does not qualify, and must be reported.
R=$(new_repo gaming)
B=$(base_sha "$R")
printf '# audit\n' > "$R/docs/audit.md"
printf '# unpushed\n' > "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "add the handoff documents"
S=$(base_sha "$R")
printf 'package main\n' > "$R/main.go"
printf '\ntwo\n' >> "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "code change with a README tweak"
G=$(base_sha "$R")
# Deliberately do NOT list G.
out=$(cd "$R" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
assert_run "code commit with a README tweak is still required" 1 "missing from dist/README.md" "$out" "$got"
assert_run "the laundering commit is named" 1 "$G" "$out" "$got"

# And the negative control: a commit confined to dist/ IS exempt, which is what
# makes the whole check converge instead of looping forever.
R=$(new_repo delivery)
B=$(base_sha "$R")
printf '# audit\n' > "$R/docs/audit.md"
printf '# unpushed\n' > "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "add the handoff documents"
S=$(base_sha "$R")
printf '\n- `%s` add the handoff documents\n' "$S" > "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "deliver the list"
D=$(base_sha "$R")
out=$(cd "$R" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
# S is listed, D is the exempt delivery commit: the check must converge.
assert_run "a dist/-confined delivery commit is exempt" 0 "all citations resolve" "$out" "$got"

echo "attribution-selftest: case 5 -- recovered mode matches by content"
# Rebuild the real history as a git-am replay: same content, different hashes.
# --recovered must then pass, because the citations still refer to the same
# CHANGES even though this tree re-hashed every commit.
#
# The replay base is the commit the unpushed patch applies ONTO -- the true
# origin/master the bundle/patch were generated against, NOT this checkout's
# HEAD. A clone of the local path tracks the local master, which already
# contains every unpushed commit, so replaying onto it would apply the patch on
# top of commits that are already there. That reproduces nothing and the case
# would then "pass" for the wrong reason, so the base is taken explicitly.
DST="$TMPROOT/recovered-dst"
PATCH="$REPO/dist/airouter-unpushed.patch"
BASE_SHA=$(git -C "$REPO" rev-parse origin/master)
if [ ! -r "$PATCH" ]; then
  # dist/airouter-unpushed.patch is a GENERATED artifact and is gitignored, so
  # it is legitimately absent from a fresh clone or a CI checkout of a tag.
  # Skipping loudly is correct here; what is NOT correct would be to let the
  # case quietly pass, so it reports a skip rather than an ok, and the total
  # reflects that the case did not run. Regenerate with scripts/export-unpushed.sh.
  printf '  skip cases 5-6: %s is a generated artifact and is absent here\n' \
    "${PATCH#$REPO/}" >&2
  printf '               (regenerate with scripts/export-unpushed.sh to run them)\n' >&2
  SKIPPED_RECOVERY=1
else
  git init -q "$DST"
  git_q "$DST" remote add origin "$REPO"
  # Fetch the exact base commit by SHA, so the working branch does not depend on
  # which branch a local clone happens to track. A clone of this path tracks the
  # checkout's own master, which already contains every unpushed commit -- so
  # `git am` onto it would apply the patch on top of commits that are already
  # present, reproduce nothing, and let the case pass for the wrong reason.
  git_q "$DST" fetch -q origin "$BASE_SHA"
  git_q "$DST" reset -q --hard FETCH_HEAD

  # Assert the base is really the thing it claims to be, by SHA, and that it is
  # NOT this checkout's HEAD. Fetching `origin` (the local path) would track the
  # local master, which already contains every unpushed commit: the replay would
  # then apply on top of commits that are already present, reproduce nothing,
  # and every remaining assertion in this case would pass for the wrong reason.
  # This is the third time that trap has cost time in this repository.
  if [ "$(git_q "$DST" rev-parse HEAD)" = "$(git -C "$REPO" rev-parse HEAD)" ]; then
    bad "the replay base is not HEAD ($BASE_SHA)"
  else
    ok "the replay base is origin/master by SHA, not this checkout's HEAD"
  fi
  # A STRUCTURAL negative control on that base, replacing a behavioural one. The
  # old version replayed the patch onto HEAD and required `git am` to reject it.
  # That stopped happening when the unpushed set shrank to two commits: git am
  # recognises an already-applied patch and skips it, and the case went red
  # describing a property of git rather than of the handoff. The property this
  # case actually needs is "the base does not already contain the work", which is
  # what is asserted now -- in terms that do not depend on the size of the patch
  # or on how chatty the installed git happens to be.
  PTIP=$(git -C "$REPO" rev-parse HEAD)
  if git_q "$DST" merge-base --is-ancestor "$PTIP" HEAD 2>/dev/null; then
    bad "the replay base already contains the patch tip ($PTIP): replaying it reproduces nothing"
  else
    ok "the replay base does not already contain the work the patch carries"
  fi

  # Replay the unpushed commits with git am: content preserved, hashes all new.
  ( cd "$DST" && git -c user.email=t@t -c user.name=t am --3way -q "$PATCH" \
      >/dev/null 2>&1 )
  # The replay used the committed copy of the checker, which predates
  # --recovered. Install the current one so the mode under test is the one that
  # is actually exercised.
  cp "$CHECK" "$DST/scripts/audit-attribution-check.py"

  if ! git_q "$DST" rev-parse --verify -q HEAD >/dev/null; then
    bad "recovered tree built (git am replay failed -- cannot test case 5)"
  else
    # The recovered tree's document has to cite commits the patch carried, or
    # --recovered has nothing to match and the case proves nothing. Grepping the
    # real docs/audit.md for "the first backticked hash" used to supply that, and
    # it silently stopped supplying it the moment someone pushed: the cited
    # commits moved into the replay's own base, where they resolve, so the
    # premise -- citations the replay has re-hashed -- evaporated underneath the
    # case while the code stayed correct. Derive the citations from the
    # repository instead of hoping the document happens to contain one, and
    # require that every one of them is now unresolvable here.
    CITED_LIST=$(git -C "$REPO" rev-list origin/master..HEAD)
    if [ -z "$CITED_LIST" ]; then
      bad "origin/master..HEAD is empty, so the replay had nothing to reproduce"
    else
      for h in $CITED_LIST; do
        printf '\nThe self-test cites `%s` so recovered mode has something to match.\n' "$h" \
          >> "$DST/docs/audit.md"
      done
      STILL=0
      for h in $CITED_LIST; do
        if git_q "$DST" rev-parse --verify -q "$h^{commit}" >/dev/null; then
          STILL=$((STILL + 1))
        fi
      done
      if [ "$STILL" -ne 0 ]; then
        bad "replay re-hashed the commits ($STILL replayed commit(s) still resolve here)"
      else
        ok "replay re-hashed every commit the patch carried (sanity)"
      fi
    fi

    # Without --recovered, the citations cannot resolve by hash. That is the
    # condition case 5 exists to fix, so assert it is real rather than assumed.
    out=$(cd "$DST" && python3 scripts/audit-attribution-check.py 2>&1); got=$?
    assert_run "plain mode in a recovered tree reports the re-hashes" 1 "which is not a commit" "$out" "$got"

    out=$(cd "$DST" && python3 scripts/audit-attribution-check.py --recovered --against "$REPO" 2>&1); got=$?
    assert_run "recovered mode passes on an honest citation" 0 "all citations resolve" "$out" "$got"

    echo "attribution-selftest: case 6 -- recovered mode still rejects a fabrication"
    cp "$DST/docs/audit.md" "$TMPROOT/audit.saved"
    printf '\nInvented commit `cafebabe` never happened.\n' >> "$DST/docs/audit.md"
    out=$(cd "$DST" && python3 scripts/audit-attribution-check.py --recovered --against "$REPO" 2>&1); got=$?
    assert_run "recovered mode rejects a fabricated hash" 1 "cafebabe" "$out" "$got"
    cp "$TMPROOT/audit.saved" "$DST/docs/audit.md"
  fi
fi

echo "attribution-selftest: case 7 -- --recovered refuses to run without --against"
R=$(new_repo noagainst)
B=$(base_sha "$R")
printf '# audit\n' > "$R/docs/audit.md"
printf '# unpushed\n' > "$R/dist/README.md"
git_q "$R" add -A && git_q "$R" commit -qm "add the handoff documents"
out=$(cd "$R" && python3 scripts/audit-attribution-check.py --recovered 2>&1); got=$?
assert_run "--recovered without --against exits 2" 2 "requires --against" "$out" "$got"

out=$(cd "$R" && python3 scripts/audit-attribution-check.py --recovered --against "$TMPROOT/not-a-repo" 2>&1); got=$?
assert_run "--against a non-repository exits 2" 2 "is not a" "$out" "$got"

echo ""
echo "----------------------------------------"

# A run is "complete" when neither environment-dependent case was skipped. The
# two expected totals are the contract: a case that starts skipping for a new
# reason, or one that stops running, changes PASSED and is reported here rather
# than being absorbed into a smaller number that still looks green.
if [ "$SKIPPED_RECOVERY" -eq 1 ] || [ "$SKIPPED_CASE1" -eq 1 ]; then
  expected="$EXPECTED_CLONE"
  printf 'attribution-selftest: %d passed, %d failed (reduced run: expected %d)\n' \
    "$PASSED" "$FAILED" "$expected"
else
  expected="$EXPECTED_FULL"
  printf 'attribution-selftest: %d passed, %d failed (full run: expected %d)\n' \
    "$PASSED" "$FAILED" "$expected"
fi

if [ "$PASSED" -ne "$expected" ]; then
  bad "assertion count matches the contract (got $PASSED, expected $expected)"
fi

[ "$FAILED" -eq 0 ] || exit 1
exit 0
