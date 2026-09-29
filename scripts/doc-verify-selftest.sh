#!/usr/bin/env bash
# Prove scripts/doc-verify.sh can fail, and that it fails for the right reason.
#
# WHY THIS EXISTS
#
# doc-verify.sh exists because dist/README.md once told the reader to run a
# command git rejects, and every other check in this repository reads code. The
# document was corrected, but a correction verified only by grepping one's own
# edit is the same kind of check that let the drift survive in the first place.
#
# A check that has never been observed failing is a claim. When doc-verify.sh
# was first written, its own verdict was produced by running it once and seeing
# green -- which is exactly the evidence that proves nothing. This file is the
# missing half: it breaks the document in the specific ways that have actually
# happened, asserts that doc-verify.sh goes red, and asserts that it goes red
# for a NAMED reason rather than for some incidental breakage.
#
# Every case mutates a COPY. dist/README.md is restored afterwards and its
# fingerprint is checked, so a case that fails partway cannot leave the real
# handoff document modified -- which would be a genuinely dangerous way for
# this test to go wrong.
#
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
# Exit 2 = the harness could not run.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO/scripts/doc-verify.sh"
DOC="$REPO/dist/README.md"

#   case 1 (3)  the real document passes, and the harness left it untouched
#   case 2 (3)  a 'HEAD:' refspec is caught, applied, and named
#   case 3 (3)  a non-existent ref name is caught by actually running it
#   case 4 (2)  an unexercisable document is caught
#   case 5 (6)  the "additional claims" can fail: a corrupted bundle turns both
#                the documented recovery check and the freshness check red,
#                and the final "restored byte-for-byte" check closes every case
#   case 6 (6)  the merge and checkout paths can fail: dropping --ff-only,
#                deleting the merge instruction, and checking out a ref that no
#                documented fetch creates are each caught and each named
EXPECTED=23

command -v git >/dev/null 2>&1 || { echo "doc-verify-selftest: git not found" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "doc-verify-selftest: python3 not found" >&2; exit 2; }
[ -r "$CHECK" ] || { echo "doc-verify-selftest: cannot read $CHECK" >&2; exit 2; }
[ -r "$DOC" ] || { echo "doc-verify-selftest: cannot read $DOC" >&2; exit 2; }

TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/doc-verify-selftest.XXXXXX") || exit 2
ORIG_DOC="$TMPROOT/README.md.orig"
cp "$DOC" "$ORIG_DOC" || exit 2

# Restore the real document no matter how this script exits. A test harness that
# can leave the handoff instructions broken is worse than no test harness.
restore() {
  cp "$ORIG_DOC" "$DOC" 2>/dev/null
  rm -rf "$TMPROOT"
}
trap restore EXIT

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

fingerprint() { git -C "$REPO" hash-object "$DOC" 2>/dev/null || echo unknown; }
DOC_FP=$(fingerprint)

run_check() {
  local out got
  out=$(bash "$CHECK" 2>&1); got=$?
  printf '%s' "$out"
  return "$got"
}

# Apply an edit to the real document and assert the change landed. A mutation
# that matches nothing produces a green check that proves nothing -- the
# round-10 lesson, repeated here because the same mistake was available again.
mutate() {
  python3 - "$DOC" "$1" "$2" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
n = s.count(old)
# The refspec appears in BOTH fetch examples -- the fresh-clone one and the
# existing-clone one -- and a document that documents the same command twice is
# the correct thing to have. So every occurrence is replaced, and the case
# still fails if the pattern is missing entirely (a silent no-op mutation is
# the round-10 trap) or if it somehow does not appear where it should.
if n < 1:
    sys.exit("pattern not found; the mutation would be a no-op")
open(path, 'w').write(s.replace(old, new))
PY
}

# ------------------------------------------------------------------------
echo "doc-verify-selftest: case 1 -- the real document passes"
out=$(run_check); got=$?
if [ "$got" -eq 0 ]; then
  ok "the real handoff document passes doc-verify"
else
  bad "the real handoff document FAILS doc-verify (exit $got)"
  printf '%s\n' "$out" | tail -5 | sed 's/^/         | /'
fi
if printf '%s' "$out" | grep -q "no sh block instructs a 'HEAD:' fetch"; then
  ok "the check reports finding no bad HEAD fetch"
else
  bad "the check did not report the HEAD-fetch assertion"
fi
# The harness must not have altered the document merely by inspecting it.
if [ "$(fingerprint)" = "$DOC_FP" ]; then
  ok "the harness left the document byte-identical"
else
  bad "the harness MODIFIED dist/README.md; it is a read-only check"
fi

echo "doc-verify-selftest: case 2 -- a 'HEAD:' refspec is caught and named"
# The exact regression Round 15 fixed, restored deliberately.
if mutate "'refs/heads/airouter-unpushed-export:refs/heads/frombundle'" \
          "'HEAD:refs/heads/frombundle'"; then
  ok "the case-2 mutation applied"
else
  bad "the case-2 mutation did NOT apply; the case would prove nothing"
  cp "$ORIG_DOC" "$DOC"
fi
if [ "$(fingerprint)" != "$DOC_FP" ]; then
  out=$(run_check); got=$?
  if [ "$got" -ne 0 ]; then
    ok "a 'HEAD:' refspec makes doc-verify fail"
  else
    bad "doc-verify returned 0 on a 'HEAD:' refspec it must reject"
  fi
  # It must name the problem. A check that goes red for an incidental reason --
  # a broken fixture, a missing tool -- is not evidence about this defect.
  if printf '%s' "$out" | grep -q "instructs a 'HEAD:' fetch"; then
    ok "and it named the HEAD-fetch problem specifically"
  else
    bad "it failed, but did not name the HEAD-fetch problem"
  fi
else
  bad "the case-2 mutation left the document unchanged"
fi
cp "$ORIG_DOC" "$DOC"

echo "doc-verify-selftest: case 3 -- a bogus ref name is caught by running it"
# Not the Round-15 regression, and not detectable by reading the text: the
# refspec is a plausible ref that simply does not exist in the bundle. Only
# executing it can catch this, which is the reason doc-verify runs the
# document's commands instead of grepping them.
if mutate "refs/heads/airouter-unpushed-export:refs/heads/frombundle" \
          "refs/heads/no-such-ref-name:refs/heads/frombundle"; then
  ok "the case-3 mutation applied"
else
  bad "the case-3 mutation did NOT apply; the case would prove nothing"
  cp "$ORIG_DOC" "$DOC"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ]; then
  ok "a ref that does not exist in the bundle is caught"
else
  bad "doc-verify returned 0 on a refspec that cannot be fetched"
fi
# The explicit HEAD-check must NOT be what fired, or case 3 proves nothing new
# beyond case 2: this defect has to be caught by actually running the command.
if printf '%s' "$out" | grep -q "documented refspec FAILS"; then
  ok "and it was caught by executing the refspec, not by the text check"
else
  bad "it was not caught by execution; the text check alone cannot see this"
fi
cp "$ORIG_DOC" "$DOC"

echo "doc-verify-selftest: case 4 -- a document with no command is caught"
# Strip the sh blocks that contain a fetch. A handoff document that no longer
# tells the reader how to apply anything is broken in a way nothing else here
# notices, and a checker that only inspects commands it finds is vacuous on an
# empty document.
if python3 - "$DOC" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
out = re.sub(r"```sh\n(?:.*?)```", "```\n(replaced by the self-test)\n```", s, flags=re.S)
open(p, 'w').write(out)
PY
then
  ok "the case-4 mutation applied"
else
  bad "the case-4 mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ]; then
  ok "a document with no executable fetch command is caught"
else
  bad "doc-verify returned 0 on a document with no commands in it"
fi
cp "$ORIG_DOC" "$DOC"

# The four "additional documented claims" run the document's other instructions
# -- the freshness check, `git bundle list-heads`, `git am --3way`, and
# recovery-check.sh. Each was added because a documented claim that nothing
# executes is a claim, not a check. That is only worth anything if they can
# fail, so case 5 breaks the artifacts the way a real corruption would and
# requires the checks to go red. Without it those four assertions are four more
# untested green lines -- the same shape of unverified claim this file exists to
# prevent.
echo "doc-verify-selftest: case 5 -- the additional claims can fail"
# Build the same fixture doc-verify builds, so the corruption lands on real
# generated artifacts rather than on a stand-in.
FX="$TMPROOT/claims-fixture"
mkdir -p "$FX/scripts" "$FX/dist" "$FX/docs"
for s in "$REPO"/scripts/*.sh "$REPO"/scripts/*.py "$REPO"/scripts/*.txt; do
  cp "$s" "$FX/scripts/" 2>/dev/null
done
printf 'base\n' > "$FX/README.md"
printf '# audit\n' > "$FX/docs/audit.md"
printf '# unpushed handoff\n' > "$FX/dist/README.md"
fg() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }
fg "$FX" init -q -b master
fg "$FX" config user.email t@t
fg "$FX" config user.name t
fg "$FX" add -A
fg "$FX" commit -qm "base commit"
fg "$FX" update-ref refs/remotes/origin/master HEAD
i=0
while [ "$i" -lt 2 ]; do
  printf 'content %s\n' "$i" > "$FX/change-$i.txt"
  fg "$FX" add -A
  fg "$FX" commit -qm "unpushed $i"
  i=$((i + 1))
done
if ( cd "$FX" && UPSTREAM=origin/master bash scripts/export-unpushed.sh >/dev/null 2>&1 ); then
  ok "case-5 fixture generated real artifacts"
else
  bad "case-5 fixture could not generate artifacts; the case would prove nothing"
fi

# The positive half: on intact artifacts the documented recovery check passes.
# Asserted first, so a later red is attributable to the corruption and not to a
# fixture that was never working.
if ( cd "$FX" && UPSTREAM=origin/master bash scripts/recovery-check.sh --quick ) >/dev/null 2>&1; then
  ok "intact artifacts pass the documented recovery check"
else
  bad "intact artifacts FAILED the recovery check; the fixture is not sound"
fi

# Now corrupt the bundle exactly as a damaged export would be, and require the
# same command to go red. This is what makes the doc-verify assertions
# load-bearing: if recovery-check could not detect a corrupt bundle, then
# doc-verify's "the documented recovery check passes" would be true of a
# handoff that does not work.
printf 'this is not a git bundle' > "$FX/dist/airouter-unpushed.bundle"
if ( cd "$FX" && UPSTREAM=origin/master bash scripts/recovery-check.sh --quick ) >/dev/null 2>&1; then
  bad "a corrupted bundle PASSED the recovery check; the claim proves nothing"
else
  ok "a corrupted bundle is caught, so the green claim above means something"
fi

# And the freshness check, which doc-verify also runs as a documented claim.
if ( cd "$FX" && UPSTREAM=origin/master bash scripts/dist-freshness-check.sh ) >/dev/null 2>&1; then
  bad "a corrupted bundle PASSED the freshness check; the claim proves nothing"
else
  ok "a corrupted bundle is caught by the documented freshness check too"
fi

# None of that touched the real document.
if [ "$(fingerprint)" = "$DOC_FP" ]; then
  ok "case 5 left the document untouched"
else
  bad "case 5 MODIFIED dist/README.md"
fi

# ------------------------------------------------------------------------
# Case 6: the two bundle-application paths that Round 18 added.
#
# "Or into an existing clone" ends in `git merge --ff-only`, which is the only
# place the document's promise that "nothing needs rebasing -- the history is
# linear on origin/master" is executable at all, and `git checkout frombundle`
# is the last step of the preferred path. Both were green on the first run,
# which is precisely the evidence that proves nothing. Each is broken here by
# editing the DOCUMENT, because the checks extract the merge flags, the merge
# ref and the checkout ref from the document: if a mutation to the instruction
# left the check green, the check would be verifying a command nobody reads.
echo "doc-verify-selftest: case 6 -- the merge and checkout paths can fail"
if mutate 'git merge --ff-only frombundle' 'git merge frombundle'; then
  ok "the case-6a mutation applied"
else
  bad "the case-6a mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ] && printf '%s' "$out" | grep -q 'not --ff-only'; then
  ok "dropping --ff-only is caught and named, not silently absorbed"
else
  bad "doc-verify accepted a document whose merge cannot detect a rebased history (exit $got)"
  printf '%s\n' "$out" | tail -4 | sed 's/^/         | /'
fi
cp "$ORIG_DOC" "$DOC"

# The other direction: the instruction disappears entirely. A checker that
# reports "0 failed" because it found nothing to check has stopped checking.
if mutate 'git merge --ff-only frombundle
' ''; then
  ok "the case-6b mutation applied"
else
  bad "the case-6b mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ] && printf '%s' "$out" | grep -q 'documents no merge command'; then
  ok "a document that stopped documenting the merge is caught"
else
  bad "doc-verify returned $got with the merge instruction deleted"
  printf '%s\n' "$out" | tail -4 | sed 's/^/         | /'
fi
cp "$ORIG_DOC" "$DOC"

# And the ref names are coupled to the fetch: checking out a ref the documented
# fetch never creates is a document that cannot work, and grepping for one
# spelling of "frombundle" would have missed it.
if mutate 'git checkout frombundle' 'git checkout recovered'; then
  ok "the case-6c mutation applied"
else
  bad "the case-6c mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ] && printf '%s' "$out" | grep -q 'no documented fetch creates that ref'; then
  ok "a checkout of a ref no documented fetch creates is caught"
else
  bad "doc-verify returned $got when the checkout ref did not match the fetched ref"
  printf '%s\n' "$out" | tail -4 | sed 's/^/         | /'
fi
cp "$ORIG_DOC" "$DOC"

# The document must be exactly as it was found. If it is not, the cases above
# have left the handoff instructions in a state nobody chose.
if [ "$(fingerprint)" = "$DOC_FP" ]; then
  ok "every case restored the document byte-for-byte"
else
  bad "a case left dist/README.md MODIFIED; the handoff doc is corrupted"
fi

echo ""
echo "----------------------------------------"
printf 'doc-verify-selftest: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$PASSED" -eq "$EXPECTED" ] || bad "assertion count matches the contract (got $PASSED, expected $EXPECTED)"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
