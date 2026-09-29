#!/usr/bin/env bash
# The handoff document is the one artifact a human is guaranteed to follow, and
# the one nothing else verifies.
#
# WHY THIS EXISTS
#
# dist/README.md once told the reader to run
#   git fetch .../airouter-unpushed.bundle 'HEAD:refs/heads/frombundle'
# which fails with `fatal: couldn't find remote ref HEAD`. That was the exact
# defect Round 13 fixed in export-unpushed.sh: `git bundle create A..HEAD`
# records the tip under the ref HEAD, and git fetch rejects that name. The
# script was corrected and the document was not, so the one file written
# *specifically for the next person* still carried the failure mode the round
# was about. Every other check here validates code; none of them read the doc.
#
# A documentation check is usually a grep, and a grep is what let the drift
# survive in the first place. So this does not grep: it extracts the refspecs
# out of the document's own fenced sh blocks and RUNS each one against a
# freshly generated bundle. A command in the doc either works or it does not.
#
# Only fenced ```sh blocks are considered. The prose deliberately quotes the
# old broken command to explain why it is wrong, so scanning raw text would
# flag the explanation as though it were an instruction.
#
# Exit 0 = every documented command works. Exit 1 = at least one does not.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
P="doc-verify:"

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

command -v git >/dev/null 2>&1 || { echo "$P git not found" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "$P python3 not found" >&2; exit 2; }

DOC="$REPO/dist/README.md"
[ -r "$DOC" ] || { echo "$P cannot read $DOC" >&2; exit 2; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/doc-verify.XXXXXX") || exit 2
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# A fixture shaped like a real handoff: real scripts, a real upstream ref, and
# the REAL document under test, so a fix to the doc is what is being measured.
D="$WORK/repo"
mkdir -p "$D/scripts" "$D/dist" "$D/docs"
for s in "$REPO"/scripts/*.sh "$REPO"/scripts/*.py "$REPO"/scripts/*.txt; do
  cp "$s" "$D/scripts/" 2>/dev/null
done
printf 'base\n' > "$D/README.md"
printf "# audit\n" > "$D/docs/audit.md"
# A MINIMAL handoff document for the fixture, not a copy of the real one.
#
# The real dist/README.md cites ~13 real commits, and copying it into a fixture
# whose history does not contain them makes the attribution check report every
# one as a dangling citation. That failure is a property of the FIXTURE, not of
# recovery, and it arrives wearing the costume of a recovery bug.
#
# The real document is not what is being trusted here. It is read directly from
# $DOC below and its refspecs are executed against THIS fixture's bundle. The
# fixture only needs a document the fixture's own history can satisfy.
printf '# unpushed handoff\n' > "$D/dist/README.md"
g() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }
g "$D" init -q -b master
g "$D" config user.email t@t
g "$D" config user.name t
g "$D" add -A
g "$D" commit -qm "base commit"
g "$D" update-ref refs/remotes/origin/master HEAD
i=0
while [ "$i" -lt 2 ]; do
  # Each unpushed commit touches its OWN file. Appending every commit to one
  # README makes the patch un-replayable by construction: `git am` onto the
  # base then hits a genuine 3-way conflict on the same lines, and the check
  # reports a recovery failure that is a property of the FIXTURE, not of the
  # recovery. It cost two wrong fixes before that was diagnosed. A handoff
  # carries changes to distinct files; the fixture now does too.
  printf 'content %s\n' "$i" > "$D/change-$i.txt"
  g "$D" add -A
  g "$D" commit -qm "unpushed $i"
  i=$((i + 1))
done

# The fixture's dist/README.md must list the fixture's own unpushed commits, or
# the attribution check inside recovery-check.sh will -- correctly -- report them
# as missing from the handoff. The check is right and the fixture was wrong: a
# hand-off document with no commit list is not a realistic handoff, and the
# resulting failure read as a recovery defect when it was a fixture defect.
# The commit that lists them touches only dist/, so it is itself exempt.
printf '\nFixture unpushed commits:\n' >> "$D/dist/README.md"
g "$D" rev-list --reverse refs/remotes/origin/master..HEAD | while IFS= read -r sha; do
  gmsg=$(g "$D" log -1 --format=%s "$sha")
  printf -- '- `%s` %s\n' "$(printf '%s' "$sha" | cut -c1-7)" "$gmsg" >> "$D/dist/README.md"
done
g "$D" add -A
g "$D" commit -qm "list the fixture's unpushed commits"

if ! ( cd "$D" && UPSTREAM=origin/master bash scripts/export-unpushed.sh >/dev/null 2>&1 ); then
  echo "$P could not generate fixture artifacts" >&2
  exit 2
fi

# Pull refspecs out of the document itself. A hand-copied list would be a second
# thing to drift, which is the entire problem being fixed here.
python3 - "$DOC" > "$WORK/refspecs" <<'PY'
import re, sys
txt = open(sys.argv[1]).read()
blocks = re.findall(r"```sh\n(.*?)```", txt, re.S)
seen = []
for b in blocks:
    # Join shell line continuations, so a refspec written across two lines is
    # still recognised as the single command it is.
    joined = b.replace("\\\n", " ")
    for m in re.finditer(r"git fetch .*?'([^']+)'", joined):
        seen.append(m.group(1))
for s in dict.fromkeys(seen):
    print(s)
PY

count=$(wc -l < "$WORK/refspecs" | tr -d ' ')
echo "$P $count executable fetch refspec(s) found in $DOC"

if [ "$count" -gt 0 ]; then
  ok "the document contains at least one fetch command"
else
  bad "no fetch command found in any sh block; the document is unexercisable"
fi

# Run each one. A reader will run exactly this, so this is the same command.
while IFS= read -r spec; do
  [ -z "$spec" ] && continue
  rm -rf "$WORK/reader"
  git clone -q -b master "$D" "$WORK/reader" 2>/dev/null
  if ( cd "$WORK/reader" &&
       git fetch -q "$D/dist/airouter-unpushed.bundle" "$spec" >/dev/null 2>&1 ); then
    ok "documented refspec works: $spec"
  else
    bad "documented refspec FAILS: $spec"
  fi
done < "$WORK/refspecs"

# And the specific regression this file exists for: no executable block may
# fetch the 'HEAD' ref, which git rejects outright.
if python3 - "$DOC" <<'PY'
import re, sys
txt = open(sys.argv[1]).read()
offenders = [
    b for b in re.findall(r"```sh\n(.*?)```", txt, re.S)
    if re.search(r"git fetch .*?'HEAD:", b.replace("\\\n", " "))
]
if offenders:
    print("offending block:\n" + offenders[0], file=sys.stderr)
sys.exit(1 if offenders else 0)
PY
then
  ok "no sh block instructs a 'HEAD:' fetch (which git rejects)"
else
  bad "an sh block still instructs a 'HEAD:' fetch, which git rejects"
fi

# ------------------------------------------------- the other claims too ----
# Round 15's defect was a documented command that did not work. The fetch
# refspecs were the obvious instance, but the document makes several other
# executable claims, and each is one more instruction a reader will follow
# verbatim:
#
#   ./scripts/dist-freshness-check.sh --verbose   are they current?
#   ./scripts/recovery-check.sh                   do they actually recover?
#   git am --3way <patch>                         applying the patch
#   git bundle list-heads <bundle>                reading the recorded tip
#   git checkout frombundle                       applying the bundle to a clone
#   git merge --ff-only frombundle                applying it to an existing clone
#
# A claim in prose that nothing executes is the same failure as a stale
# refspec, just quieter. Each is run here against the fixture. The push and
# remote-URL blocks are deliberately NOT run: they act on a real remote, and a
# verification step that can push to GitHub is not a verification step.
printf ''

echo "$P additional documented claims"

# 1. The document says the freshness checker exits non-zero when artifacts are
#    stale. Verify the positive half here (they ARE current), because that is
#    the claim a reader relies on before relying on the artifacts at all.
if ( cd "$D" && UPSTREAM=origin/master bash scripts/dist-freshness-check.sh --verbose ) \
     >/dev/null 2>&1; then
  ok "the documented freshness check passes on current artifacts"
else
  bad "the documented freshness check FAILED on freshly generated artifacts"
fi

# 2. `git bundle list-heads` is documented as the way to read the tip, and as
#    agreeing with HEAD. Run it and compare, rather than trusting that the doc
#    describes the right command.
BUNDLE_TIP=$(git -C "$D" bundle list-heads "$D/dist/airouter-unpushed.bundle" 2>/dev/null \
  | awk 'NR==1{print $1}')
HEAD_TIP=$(git -C "$D" rev-parse HEAD)
if [ -n "$BUNDLE_TIP" ] && [ "$BUNDLE_TIP" = "$HEAD_TIP" ]; then
  ok "the documented 'git bundle list-heads' really does report HEAD"
else
  bad "bundle list-heads reported '$BUNDLE_TIP', HEAD is '$HEAD_TIP'"
fi

# 3. `git am --3way` on the patch, in a clone at the TRUE BASE. This is the
#    documented patch path and the one a reader without the bundle will use.
#
#    Two things the first version of this got wrong, both of which made the
#    assertion look like a product failure when it was neither:
#      - it cloned at `master`, which ALREADY CONTAINS the unpushed commits, so
#        there was nothing to apply;
#      - the throwaway clone had no committer identity, so `git am` died with
#        "Committer identity unknown" before it could even try.
#    A fixture that fails for its own reasons is worse than no fixture: it
#    teaches the reader to distrust a check that was working.
PR="$WORK/patchreader"
git clone -q "$D" "$PR" 2>/dev/null
# Put the reader on the base, which is what the handoff document actually tells
# a person: apply onto the pushed history, not onto a tree that already has the
# commits. Cloning at master would make this a tautology.
if git -C "$PR" rev-parse --verify --quiet refs/remotes/origin/master >/dev/null 2>&1; then
  git -C "$PR" checkout -q -B base refs/remotes/origin/master 2>/dev/null
else
  git -C "$PR" checkout -q -B base HEAD~2 2>/dev/null
fi
# `git am` re-creates every commit, so it needs a committer to be someone. The
# reader is a throwaway clone and the identities are already synthetic.
git -C "$PR" config user.email reader@example.invalid
git -C "$PR" config user.name reader
if ( cd "$PR" && git am --3way "$D/dist/airouter-unpushed.patch" ) >/dev/null 2>&1; then
  if [ "$(git -C "$PR" rev-parse HEAD^{tree})" = "$(git -C "$D" rev-parse HEAD^{tree})" ]; then
    ok "the documented 'git am --3way' reproduces the tree exactly"
  else
    bad "git am applied but the resulting tree differs from the working tree"
  fi
else
  bad "the documented 'git am --3way' failed on the generated patch"
fi

# 4. The document tells the reader recovery-check.sh answers "do they actually
#    recover?". Run it the documented way (no UPSTREAM override, from the
#    fixture) and require the answer to be yes. --quick skips the Go build,
#    which CI's `go` job already covers for identical content.
RC_OUT=$( cd "$D" && UPSTREAM=origin/master bash scripts/recovery-check.sh --quick 2>&1 )
RC_GOT=$?
if [ "$RC_GOT" -eq 0 ]; then
  ok "the documented recovery check passes on the generated artifacts"
else
  bad "the documented recovery check FAILED on freshly generated artifacts (exit $RC_GOT)"
  printf '%s\n' "$RC_OUT" | grep -E '^\s+FAIL' | head -4 | sed 's/^/         | /'
fi

# 5. "Or into an existing clone": fetch the documented refspec, then
#    `git merge --ff-only frombundle`.
#
#    The fetch half was already exercised, and the tree comparison below covers
#    "did it arrive intact", but `--ff-only` is asserting something none of the
#    other checks can see. It is the executable form of a PROSE claim several
#    lines away in "Pushing once write access exists": "Nothing needs rebasing
#    -- the history is linear on `origin/master`." That sentence is a promise
#    about the shape of the history, and a reader who follows it and turns out
#    to be wrong gets `fatal: Not possible to fast-forward` in the middle of a
#    handoff, with no instruction left to follow.
#
#    A plain `git merge` would accept a divergent history with a merge commit
#    and report success. --ff-only is what makes the check mean the sentence,
#    so it is what gets run -- taken from the document, not re-typed here.
# What the document itself says to merge and to check out. Extracted rather than
# re-typed: a hand-copied ref name here would be a second thing that can drift,
# and a check that keeps passing after the instruction it verifies was rewritten
# is the failure this file was written for.
DOC_CMDS=$(python3 - "$DOC" <<'PY'
import re, sys
txt = open(sys.argv[1]).read()
merge_flags = merge_ref = checkout_ref = ""
for b in re.findall(r"```sh\n(.*?)```", txt, re.S):
    joined = b.replace("\\\n", " ")
    if not merge_ref:
        m = re.search(r"(git\s+merge\s+[^\n;|&]+)", joined)
        if m:
            toks = m.group(1).split()
            flags = [t for t in toks[1:] if t.startswith("-")]
            refs = [t for t in toks[1:] if not t.startswith("-")]
            merge_flags, merge_ref = " ".join(flags), (refs[-1] if refs else "")
    if not checkout_ref:
        m = re.search(r"git\s+checkout\s+(-\S+\s+)*([A-Za-z0-9._/-]+)", joined)
        if m:
            checkout_ref = m.group(2)
print("\t".join([merge_flags, merge_ref, checkout_ref]))
PY
)
MERGE_FLAGS=$(printf '%s' "$DOC_CMDS" | cut -f1)
MERGE_REF=$(printf '%s' "$DOC_CMDS" | cut -f2)
CHECKOUT_REF=$(printf '%s' "$DOC_CMDS" | cut -f3)

# The fetch refspec that CREATES the short ref the document later merges or
# checks out. Matched by name, not by the literal `frombundle`: a document that
# consistently renames the ref stays correct, and a check that greps for one
# spelling would report that as a handoff failure.
spec_for() {
  local s
  while IFS= read -r s; do
    [ -z "$s" ] && continue
    case "${s##*:}" in
      "$1"|"refs/heads/$1") printf '%s\n' "$s"; return 0 ;;
    esac
  done < "$WORK/refspecs"
  return 1
}

# 5. "Or into an existing clone": fetch, then `git merge --ff-only frombundle`.
#
#    The fetch half was already exercised, and the tree comparison below covers
#    "did it arrive intact", but `--ff-only` asserts something none of the other
#    checks can see. It is the executable form of a PROSE claim several lines
#    away in "Pushing once write access exists": "Nothing needs rebasing -- the
#    history is linear on `origin/master`." That sentence is a promise about the
#    shape of the history, and a reader who follows it and turns out to be wrong
#    gets `fatal: Not possible to fast-forward` mid-handoff, with no instruction
#    left to follow.
#
#    A plain `git merge` would accept a divergent history by adding a merge
#    commit, which is precisely the outcome the sentence promises will not
#    happen. So --ff-only is asserted to still BE there before it is run.
if [ -z "$MERGE_REF" ]; then
  # The document stopped documenting a merge. That is not a pass; it is this
  # check silently losing its subject, which is how the fetch refspec drifted.
  bad "the existing-clone path documents no merge command, so it cannot be exercised"
elif ! printf '%s' "$MERGE_FLAGS" | grep -q -- '--ff-only'; then
  bad "the existing-clone path documents '${MERGE_FLAGS:-git merge}', not --ff-only; a plain merge cannot detect a rebased history"
elif ! FFSPEC=$(spec_for "$MERGE_REF"); then
  bad "the document merges '$MERGE_REF' but no documented fetch creates that ref"
else
  EX="$WORK/existing"
  git clone -q "$D" "$EX" 2>/dev/null
  # Stand the "existing clone" on the pushed base. A clone left on `master`
  # already contains the commits, so fast-forwarding would succeed by doing
  # nothing and the linearity claim would never be tested.
  git -C "$EX" checkout -q -B master refs/remotes/origin/master 2>/dev/null
  git -C "$EX" config user.email reader@example.invalid
  git -C "$EX" config user.name reader
  ( cd "$EX" && git fetch -q "$D/dist/airouter-unpushed.bundle" "$FFSPEC" ) >/dev/null 2>&1
  if FF_ERR=$( cd "$EX" && git merge $MERGE_FLAGS "$MERGE_REF" 2>&1 ); then
    if [ "$(git -C "$EX" rev-parse HEAD^{tree})" = "$(git -C "$D" rev-parse HEAD^{tree})" ]; then
      ok "the documented 'git merge $MERGE_FLAGS $MERGE_REF' fast-forwards to the exported tree"
    else
      bad "merge succeeded but the resulting tree differs from the working tree"
    fi
  else
    bad "the documented 'merge $MERGE_FLAGS $MERGE_REF' could not fast-forward -- the history is not linear on the upstream base"
    printf '%s\n' "$FF_ERR" | head -3 | sed 's/^/         | /'
  fi
fi

# 6. `git checkout frombundle` -- the last step of the preferred path. Its
#    sibling fetch is exercised above, but nothing until here proves that
#    checking the fetched ref out produces the tree that was exported.
if [ -z "$CHECKOUT_REF" ]; then
  bad "the preferred path documents no checkout command, so it cannot be exercised"
elif ! COSPEC=$(spec_for "$CHECKOUT_REF"); then
  bad "the document checks out '$CHECKOUT_REF' but no documented fetch creates that ref"
else
  CO="$WORK/checkout"
  git clone -q -b master "$D" "$CO" 2>/dev/null
  git -C "$CO" config user.email reader@example.invalid
  git -C "$CO" config user.name reader
  if ( cd "$CO" && git fetch -q "$D/dist/airouter-unpushed.bundle" "$COSPEC" ) >/dev/null 2>&1; then
    if ( cd "$CO" && git checkout -q "$CHECKOUT_REF" ) >/dev/null 2>&1; then
      if [ "$(git -C "$CO" rev-parse HEAD^{tree})" = "$(git -C "$D" rev-parse HEAD^{tree})" ]; then
        ok "the documented 'git checkout $CHECKOUT_REF' reproduces the tree exactly"
      else
        bad "checkout $CHECKOUT_REF succeeded but the tree differs from the working tree"
      fi
    else
      bad "the documented 'git checkout $CHECKOUT_REF' failed after a successful fetch"
    fi
  else
    bad "could not fetch the documented refspec for the checkout path"
  fi
fi

echo ""
echo "----------------------------------------"
printf 'doc-verify: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
