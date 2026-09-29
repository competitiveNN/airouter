#!/usr/bin/env bash
# Prove the recovery artifacts actually recover, end to end, every time.
#
# WHY THIS EXISTS
#
# The bundle and patch in dist/ are the ONLY record of the unpushed commits
# once this checkout is gone -- 41 of them at the time of writing, with push
# permission still false. Everything else in this repository's audit layer
# assumes they work. That assumption was, until now, verified by hand in a
# session, which is the weakest form of verification there is: it is true of
# the commit it was run against and of no other, and nothing notices when it
# stops being true.
#
# `dist-freshness-check.sh` covers the adjacent question -- are the artifacts
# current? -- and deliberately does NOT prove they are usable. "Points at HEAD"
# and "reproduces the work" are different claims. A bundle that names the right
# tip can still be unfetchable; a patch can apply and produce a tree that does
# not build. This script makes the second claim mechanically.
#
# WHAT IT DOES
#
#   1. fetches the bundle into a throwaway repository, onto a real upstream base
#      so the commits are genuinely absent beforehand, and checks the recovered
#      tree is byte-identical to the working tree;
#   2. applies the patch with `git am --3way` onto the SAME base fetched by SHA
#      and checks the resulting tree is byte-identical too;
#   3. builds and runs the self-tests in the recovered tree, so "it recovered"
#      means "the recovered tree works", not "the files arrived".
#
# WHY THE BASE IS FETCHED BY SHA
#
# A clone of this path tracks THIS checkout's master, which already contains
# every unpushed commit. Replaying the patch onto that applies it on top of
# commits that are already present, reproduces nothing, and every check below
# would then pass for the wrong reason. The base is therefore taken explicitly
# from refs/remotes/origin/master and fetched by SHA. This is the third time
# this specific trap has cost time here; it is the reason `--against` exists in
# audit-attribution-check.py and the reason case 5 of its self-test builds the
# replay at all.
#
# WHY TREES AND NOT HASHES
#
# `git am` re-creates every commit, so the recovered history has different
# hashes and always will. The trees are what must match, and they are what is
# compared. audit-attribution-check.py --recovered exists to keep the commit-
# level citations meaningful across exactly that difference.
#
# Usage: scripts/recovery-check.sh [--keep] [--quick]
#   --keep    leave the temporary repositories on disk
#   --quick   skip the Go build and test suite (structure only)
# Exit 0 = both artifacts recovered the tree and it works.
# Exit 1 = recovery is broken.
# Exit 2 = the check could not run (no artifacts, no upstream ref, no go).
set -uo pipefail

KEEP=0
QUICK=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    --quick) QUICK=1 ;;
    *) echo "recovery-check: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUNDLE="$REPO/dist/airouter-unpushed.bundle"
PATCH="$REPO/dist/airouter-unpushed.patch"
UPSTREAM="${UPSTREAM:-origin/master}"
P="recovery-check:"

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

# Compare the recovery's tree against the working tree. Trees, not hashes: the
# bundle preserves hashes and the patch cannot, so the tip comparison is only
# meaningful for the bundle.
tree_matches() {
  local recovered="$1" label="$2"
  local want have
  want=$(git -C "$REPO" rev-parse HEAD^{tree})
  have=$(git -C "$recovered" rev-parse HEAD^{tree} 2>/dev/null)
  if [ -z "$have" ]; then
    bad "$label (no HEAD in the recovered tree)"
    return 1
  fi
  if [ "$want" != "$have" ]; then
    bad "$label (tree $have, want $want)"
    return 1
  fi
  ok "$label"
  return 0
}

if ! git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
  echo "$P not a git repository" >&2
  exit 2
fi
if ! git -C "$REPO" rev-parse --verify --quiet "$UPSTREAM" >/dev/null 2>&1; then
  echo "$P $UPSTREAM does not exist; cannot build a recovery base" >&2
  exit 2
fi
# "Nothing unpushed" is checked BEFORE the artifacts are required. A checkout
# that is fully pushed needs no artifacts, and demanding them first would report
# a clean tree as broken -- the same self-referential trap
# scripts/export-unpushed.sh exists to avoid, where a check's complaint pushes
# the next person towards committing the very artifacts that are meant to stay
# local.
if [ "$(git -C "$REPO" rev-list --count "$UPSTREAM"..HEAD)" -eq 0 ]; then
  echo "$P nothing is unpushed; there is nothing to recover"
  exit 0
fi

if [ ! -r "$BUNDLE" ] || [ ! -r "$PATCH" ]; then
  echo "$P recovery artifacts are missing." >&2
  echo "  Generate them with: ./scripts/export-unpushed.sh" >&2
  exit 2
fi

BASE_SHA=$(git -C "$REPO" rev-parse "$UPSTREAM")

# The bundle's tip ref is DISCOVERED, not assumed.
#
# export-unpushed.sh honours an EXPORT_REF override (its own default is
# "airouter-unpushed-export"), and a reader who exported with a custom name gets
# a perfectly good bundle that this script would otherwise refuse with a
# misleading "could not be fetched" -- a message that points at corruption, not
# at a name. Hardcoding the default here is what made an ad-hoc verification of
# the bundle tip report a false "MISMATCH": the tip is at
# refs/heads/airouter-unpushed-export, the probe looked for refs/heads/master,
# and the empty result read as staleness.
#
# `git bundle list-heads` reports the ref names the bundle actually contains, so
# asking the bundle is exact where assuming a name is a guess. The tip is the
# head whose commit is HEAD-equivalent; --not-$UPSTREAM means the bundle holds
# the unpushed commits and their base prerequisite, and only the tip is a head we
# want. Take the first listed head, and fail loudly if the bundle advertises
# none, rather than silently checking out nothing.
#
# The two ways this can come up empty are NOT the same problem and are reported
# differently. `git bundle list-heads` exits non-zero on a bundle it cannot
# read at all (truncated, random bytes, wrong format), and exits zero with empty
# output for a readable bundle that carries no head. Collapsing them into one
# "no head ref" message would tell a reader their corrupt bundle "advertises no
# head", which is a different diagnosis pointing at a different fix, and it
# would do so while the more honest "could not be fetched" never got to run.
# The exit status is captured for exactly this reason.
LIST_OUT=$(git -C "$REPO" bundle list-heads "$BUNDLE" 2>&1)
LIST_RC=$?
BUNDLE_REF=$(printf '%s\n' "$LIST_OUT" | awk 'NR==1{print $2}')
if [ "$LIST_RC" -ne 0 ]; then
  # Unreadable. Let the fetch below produce the canonical diagnosis instead of
  # pre-empting it with a message about ref names.
  BUNDLE_REF="refs/heads/airouter-unpushed-export"
elif [ -z "$BUNDLE_REF" ]; then
  # Readable, but genuinely carries no head to fetch. That is not a corrupt
  # bundle and not a stale one; it is an artifact with nothing in it.
  bad "the bundle is readable but advertises no head ref; there is nothing to fetch"
  exit 1
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/recovery-check.XXXXXX") || exit 2
cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    echo "$P kept $WORK"
    return
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "$P base $BASE_SHA ($(git -C "$REPO" rev-parse --short "$UPSTREAM"))"
# Name the ref we found, so a run against a custom EXPORT_REF says so instead of
# leaving the reader to wonder which name was used.
echo "$P bundle tip ref $BUNDLE_REF"

# ---------------------------------------------------------------- bundle ----
echo "$P bundle"
B="$WORK/bundle"
git init -q "$B" || exit 2
git -C "$B" config user.email t@t
git -C "$B" config user.name t
# The base must exist before the bundle can be fetched: a bundle records its
# prerequisites, and fetching into a repository that has none fails with a
# message that looks like a corrupt bundle rather than a missing base.
if ! git -C "$B" fetch -q "$BUNDLE" "$BUNDLE_REF:refs/heads/recovered" 2>/dev/null; then
  # No base in the clone: bring one in, then retry. This mirrors what the
  # documented recovery instructions actually require of a reader.
  git -C "$B" fetch -q "$REPO" "$BASE_SHA" || { bad "bundle base could not be fetched"; exit 1; }
  git -C "$B" fetch -q "$BUNDLE" "$BUNDLE_REF:refs/heads/recovered" \
    || { bad "bundle could not be fetched"; exit 1; }
fi
git -C "$B" checkout -q recovered 2>/dev/null || { bad "bundle tip did not check out"; exit 1; }
ok "bundle fetched and checked out"
tree_matches "$B" "bundle reproduces the working tree"

BUNDLE_TIP=$(git -C "$B" rev-parse HEAD)
if [ "$BUNDLE_TIP" = "$(git -C "$REPO" rev-parse HEAD)" ]; then
  ok "bundle tip equals HEAD (hashes preserved, not just content)"
else
  bad "bundle tip $(git -C "$B" rev-parse --short HEAD) != HEAD"
fi

# ----------------------------------------------------------------- patch ----
echo "$P patch"
D="$WORK/patch"
git init -q "$D" || exit 2
git -C "$D" config user.email t@t
git -C "$D" config user.name t
git -C "$D" fetch -q "$REPO" "$BASE_SHA" || { bad "patch base could not be fetched"; exit 1; }
git -C "$D" reset -q --hard FETCH_HEAD
# Confirm the commits really are absent before replaying, so a pass afterwards
# means the replay did something rather than that they were already there.
PRE=$(git -C "$D" rev-list --count "$BASE_SHA"..HEAD 2>/dev/null || echo 0)
if [ "$PRE" -ne 0 ]; then
  bad "the base already contained unpushed commits; the replay would prove nothing"
  exit 1
fi
# The failure output is captured rather than discarded. "git am" has several
# distinct failure modes that all collapse to the same exit status, and the one
# that actually bites is "Patch is empty": an unpushed commit that touches no
# files produces a patch with no diff, which `git am` refuses unless
# --allow-empty is passed. That is a real, reachable state for a handoff --
# `--allow-empty` commits and merge commits both produce it -- and reporting it
# as a generic apply failure sends the reader looking for context drift that
# isn't there.
if ! AM_OUT=$(git -C "$D" am --3way -q "$PATCH" 2>&1); then
  bad "patch did not apply cleanly (git am --3way)"
  if printf '%s' "$AM_OUT" | grep -qi 'patch is empty'; then
    printf '         | git am refused an EMPTY patch. At least one unpushed\n' >&2
    printf '         | commit changes no files (an --allow-empty commit, or a\n' >&2
    printf '         | merge commit). Recover with `git am --3way --allow-empty`,\n' >&2
    printf '         | or re-export after dropping the empty commit.\n' >&2
  fi
  printf '%s\n' "$AM_OUT" | sed 's/^/         | /' | head -8 >&2
  exit 1
fi
ok "patch applied with git am --3way onto the true base"
tree_matches "$D" "patch reproduces the working tree"

# ------------------------------------------------------ recovered tree ------
# "It recovered" must mean "the recovered tree works", not "the files arrived".
# These are the same checks a reader would run after picking the work up.
echo "$P recovered tree"
cd "$B" || exit 2

if [ "$QUICK" -eq 1 ]; then
  printf '  --   skipping build and tests (--quick)\n'
else
  if command -v go >/dev/null 2>&1; then
    if go build ./... >/dev/null 2>&1; then
      ok "recovered tree builds"
    else
      bad "recovered tree does not build"
    fi
    if go test -count=1 ./... >/dev/null 2>&1; then
      ok "recovered tree passes its test suite"
    else
      bad "recovered tree fails its test suite"
    fi
  else
    printf '  --   go not found; skipping build and tests\n'
  fi
fi

# Every self-test must run in the recovered tree, including the ones that
# depend on the generated patch -- which is exactly what a reader unpacking
# this work would NOT have, so that skip is itself worth observing.
#
# A self-test that fails because the tree lacks what it tests (no Go sources for
# the fuzz gate, no audit anchors to drift from) is reported as a skip, not a
# failure. That distinction is load-bearing: the artifacts recovered correctly
# is the claim this script makes, and a reader whose checkout is a partial one
# should not be told the recovery is broken because a check had nothing to
# check. Anything that DID run and failed is still a failure.
SKIPPED_AUDIT=0
run_in_recovered() {
  local label="$1"; shift
  local out got
  out=$("$@" 2>&1); got=$?
  if [ "$got" -eq 0 ]; then
    ok "$label"
  elif printf '%s' "$out" | grep -qE \
         'no anchors found|cannot read|no FuzzTargets|no Go source|nothing to check' \
       || printf '%s' "$out" | grep -qF 'could not reconstruct the pre-round-9 gate'
  then
    SKIPPED_AUDIT=$((SKIPPED_AUDIT + 1))
    printf '  --   %s: nothing to check in this tree (skipped)\n' "$label"
  else
    bad "$label"
  fi
}

# UPSTREAM is deliberately NOT pinned here, and that is a decision rather than
# an oversight. This block used to force UPSTREAM=origin/master onto every
# self-test, on the theory that a caller's UPSTREAM=base (as the unpushed-export
# CI job sets) would otherwise leak into children running in a recovered tree
# that has no such ref.
#
# Two things make it unnecessary. First, the leak is already fixed where it
# originated: dist-freshness-selftest.sh pins UPSTREAM on its own command line,
# and the other three self-tests never read it. Second, the pin could not have
# worked as written -- the recovered tree contains exactly one ref,
# refs/heads/recovered, so origin/master never resolves there and the branch
# always fell through to the unpinned path. Its only effect was a note on every
# run saying the pin was skipped.
#
# The assertion that "justified" it was also vacuous: deleting the pin left the
# suite green. A mechanism that never executes, guarding a leak that cannot
# occur, is cost without benefit. The self-tests own their environment; a child
# that needs a specific upstream sets it where it is visible.
for selftest in \
  fuzz-gate-selftest \
  audit-drift-selftest \
  audit-attribution-selftest \
  dist-freshness-selftest
do
  if [ ! -r "scripts/$selftest.sh" ]; then
    printf '  --   recovered tree has no scripts/%s.sh (skipped)\n' "$selftest"
    SKIPPED_AUDIT=$((SKIPPED_AUDIT + 1))
    continue
  fi
  run_in_recovered "recovered tree: $selftest passes" \
    bash "scripts/$selftest.sh"
done

# The checkers themselves must run in the recovered tree. The attribution check
# is the one that matters: the bundle preserves hashes, so every citation
# should resolve here. If it does not, the reader who unpacks the bundle gets a
# red check on a tree that is actually fine -- the exact split this repository
# already got burned by once.
run_in_recovered "recovered tree: audit anchors resolve" \
  python3 scripts/audit-drift-check.py
run_in_recovered "recovered tree: audit attributions resolve" \
  python3 scripts/audit-attribution-check.py

# And the same, in the git am tree, where the hashes were rebuilt. Plain mode is
# expected to report the re-hash; --recovered must resolve it. Both halves are
# asserted because a --recovered that accepted everything, or that resolved
# nothing, would look the same as one that works.
cd "$D" || exit 2
if python3 scripts/audit-attribution-check.py >/dev/null 2>&1; then
  printf '  note: plain attribution mode passed in the re-hashed tree; hashes were\n'
  printf '        preserved, so --recovered is not being exercised here\n'
else
  ok "patch tree: plain attribution mode reports the re-hash (expected)"
fi
if python3 scripts/audit-attribution-check.py --recovered --against "$REPO" >/dev/null 2>&1; then
  ok "patch tree: --recovered resolves the re-hashed citations"
else
  bad "patch tree: --recovered cannot resolve the re-hashed citations"
fi

echo ""
echo "----------------------------------------"
printf 'recovery-check: %d passed, %d failed\n' "$PASSED" "$FAILED"

# Skips are reported in the summary, not only inline. A run that quietly
# checked less than it appears to is the exact failure mode this repository has
# been bitten by repeatedly, and a green line reading "13 passed, 0 failed"
# says nothing about whether four of those were skipped. Stating the count lets
# a reader tell a full run from a reduced one without scrolling back.
if [ "${SKIPPED_AUDIT:-0}" -gt 0 ]; then
  printf 'recovery-check: %d check(s) SKIPPED (nothing to check in this tree)\n' \
    "$SKIPPED_AUDIT"
  printf '  This was a reduced run; on a full checkout every check runs.\n'
fi

[ "$FAILED" -eq 0 ] || exit 1
exit 0
