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
cp "$DOC" "$D/dist/README.md"
printf 'base\n' > "$D/README.md"
printf "# audit\n" > "$D/docs/audit.md"
g() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }
g "$D" init -q -b master
g "$D" config user.email t@t
g "$D" config user.name t
g "$D" add -A
g "$D" commit -qm "base commit"
g "$D" update-ref refs/remotes/origin/master HEAD
i=0
while [ "$i" -lt 2 ]; do
  printf 'change %s\n' "$i" >> "$D/README.md"
  g "$D" add -A
  g "$D" commit -qm "unpushed $i"
  i=$((i + 1))
done
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

echo ""
echo "----------------------------------------"
printf 'doc-verify: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
