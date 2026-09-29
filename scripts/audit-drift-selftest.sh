#!/usr/bin/env bash
# Self-test for scripts/audit-drift-check.py.
#
# WHY THIS EXISTS
#
# audit-drift-check.py had the same root-only-source bug the fuzz gate had: it
# collected sources with `ROOT.glob("*.go")`, which happens to cover every file
# while the repository is a single flat package and silently stops covering the
# day a file moves into a subdirectory. It reported the moved symbols as "no
# longer exists" and told the reader to add them to ALLOWLIST -- a wrong fix, and
# a permanent one, for symbols that were never deleted.
#
# The fix shipped in 56dfbdf. Nothing tested it, and a round later it was
# destroyed by a `git reset --hard` and reported as shipped anyway. A fix with no
# test is one refactor away from that, which is why the test exists rather than
# the commit message.
#
# WHY A SEPARATE SCRIPT
#
# fuzz-gate-selftest.sh stubs `go` to model a two-package Go module. This one
# needs no stub: ROOT is derived from __file__, so a fixture directory with a
# copy of the script and a docs/audit.md is a complete, self-contained
# repository from the check's point of view. Sharing one file would mean one
# harness carrying two unrelated fixture shapes, and the shared parts (assert
# helpers, temp dirs) are the parts that hide bugs when they drift.
#
# THE TWO FAILURE MODES THIS GUARDS, both of which have actually happened here:
#
#   * green for the wrong reason -- a case asserts an exit code and passes
#     because the harness under test is broken rather than because the subject
#     behaved. Every case below therefore asserts on EXPECTED MESSAGE TEXT as
#     well as on the status, so "it failed" for an unrelated reason is caught.
#   * a control that tests nothing -- a negative control is built by copying the
#     subject to a scratch file and modifying the COPY. Mutating the real script
#     even briefly is how the round-10 fix got lost.
#
# Usage: scripts/audit-drift-selftest.sh [--keep]
#        --keep   leave the temporary fixtures on disk for inspection
#
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
set -uo pipefail

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO/scripts/audit-drift-check.py"
[ -r "$CHECK" ] || { echo "drift-selftest: cannot read $CHECK" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "drift-selftest: python3 not found" >&2; exit 2; }

TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/audit-drift-selftest.XXXXXX") || exit 2
cleanup() {
  [ "$KEEP" -eq 1 ] && { echo "drift-selftest: kept $TMPROOT"; return; }
  rm -rf "$TMPROOT"
}
trap cleanup EXIT

PASSED=0
FAILED=0

ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

assert_status() {
  if [ "$3" -eq "$2" ]; then ok "$1"; else bad "$1 (exit $3, want $2)"; fi
}

# Both message and status. Asserting only the status is how "passed because the
# harness is broken" hides; asserting only the message is how a script that
# errors out early can look like a clean run.
assert_run() {
  local label="$1" want_status="$2" want_msg="$3" out="$4" got="$5"
  local problems=""
  [ "$got" -ne "$want_status" ] && problems="exit $got, want $want_status"
  if [ -n "$want_msg" ] && ! printf '%s' "$out" | grep -qF -- "$want_msg"; then
    problems="${problems:+$problems; }missing message: $want_msg"
  fi
  if [ -n "$problems" ]; then
    bad "$label ($problems)"
    printf '%s\n' "$out" | sed 's/^/         | /' | head -8
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

# new_fixture <name> -- a repository the check can run against unchanged: a
# copy of the real script, docs/audit.md, and two Go files, one at the root and
# one in a subpackage. `git init` is unnecessary: ROOT comes from __file__, and
# the check touches no git plumbing.
#
# The audit document deliberately anchors symbols that live in the SUBDIRECTORY
# file. That is the point -- a root-only glob cannot resolve them, and the
# current check must.
new_fixture() {
  local name="$1"
  local dir="$TMPROOT/$name"
  mkdir -p "$dir/scripts" "$dir/docs" "$dir/subpkg"
  cp "$CHECK" "$dir/scripts/audit-drift-check.py"
  cat > "$dir/docs/audit.md" <<'EOF'
# Fixture audit

## Findings

• `subpkgOnly` — declared only in the subpackage source.
• `rootOnly` — declared only in the root source.
EOF
  cat > "$dir/root.go" <<'EOF'
package main

type rootOnly struct{}

func rootOnly() int { return 1 }
EOF
  cat > "$dir/subpkg/sub.go" <<'EOF'
package subpkg

type subpkgOnly struct{}

func subpkgOnly() int { return 2 }
EOF
  printf '%s\n' "$dir"
}

# run_check <fixture-dir> [script-path] -- run a copy of the check, echo output,
# return its status.
run_check() {
  local dir="$1" script="${2:-$1/scripts/audit-drift-check.py}"
  out=$(cd "$dir" && python3 "$script" 2>&1)
  st=$?
  printf '%s' "$out"
  return $st
}

echo "audit-drift-check selftest: $CHECK"
echo

# ---------------------------------------------------------------------------
echo "case 1: a two-package layout resolves anchors declared in a subpackage"
# ---------------------------------------------------------------------------
# The regression this whole script exists for. Before 56dfbdf this FAILED,
# naming subpkgOnly as a deleted symbol.
repo1=$(new_fixture two-package)
out=$(run_check "$repo1"); st=$?
assert_run "subpackage anchors resolve" 0 "anchors checked, all resolve" "$out" "$st"
# The message must name both anchors' count, so an empty anchor set cannot
# masquerade as a pass.
assert_absent "no failure reported" "$out" "FAILED"
echo

# ---------------------------------------------------------------------------
echo "case 2: the pre-fix root-only glob fails that same layout"
# ---------------------------------------------------------------------------
# The negative control, and the one that proves case 1 means something. The
# broken version is produced by copying the script and replacing ONLY the
# source-collection expression -- the real script is never modified, which is
# how a verified fix got destroyed once already.
repo2=$(new_fixture control)
# The control copy goes INSIDE the fixture, not into $TMPROOT. ROOT is derived
# from __file__, so a copy sitting beside the fixture resolves ROOT to $TMPROOT
# and the check reports "audit.md not found" -- an exit 2 that looks like a
# failing control but tests nothing about the glob. The real script is still
# never modified; this is a copy, placed where the check expects to be run from.
broken="$repo2/scripts/broken-check.py"
python3 - "$repo2/scripts/audit-drift-check.py" "$broken" <<'PY'
import re
import sys

src, dst = sys.argv[1], sys.argv[2]
text = open(src).read()
# Replace the whole GO_SOURCES assignment, however it is spelled.
replaced, n = re.subn(
    r"GO_SOURCES = sorted\((?:.|\n)*?\n\)",
    'GO_SOURCES = list(ROOT.glob("*.go"))',
    text,
    count=1,
)
if n != 1:
    # Fall back to a bare one-line form so the control still works if the
    # expression is ever simplified. Refusing to build the control would
    # silently turn this case into "always green".
    replaced, n = re.subn(
        r"^GO_SOURCES = .*$",
        'GO_SOURCES = list(ROOT.glob("*.go"))',
        text,
        count=1,
        flags=re.M,
    )
if n != 1:
    sys.exit("control: could not locate the GO_SOURCES assignment")
open(dst, "w").write(replaced)
PY
if [ $? -ne 0 ]; then
  bad "could not build the root-only control"
else
  out=$(run_check "$repo2" "$broken"); st=$?
  assert_status "root-only glob fails on a two-package layout" 1 $st
  # Name the symbol, not just the status: a failure for any other reason
  # (missing file, bad python) would also exit 1.
  assert_run "and blames the moved symbol specifically" 1 "subpkgOnly" "$out" "$st"
fi
echo

# ---------------------------------------------------------------------------
echo "case 3: an anchor naming a symbol that does not exist still fails"
# ---------------------------------------------------------------------------
# The other direction. If this passed, the check would be vacuous -- reporting
# "all resolve" no matter what the audit document said.
repo3=$(new_fixture stale-anchor)
sed -i 's/`subpkgOnly`/`symbolThatIsNotDefinedAnywhere`/' "$repo3/docs/audit.md"
out=$(run_check "$repo3"); st=$?
assert_run "a bogus anchor fails the check" 1 "symbolThatIsNotDefinedAnywhere" "$out" "$st"
echo

# ---------------------------------------------------------------------------
echo "case 4: a deleted function in a subpackage is still caught"
# ---------------------------------------------------------------------------
# Removing the declaration AND its doc comment. With only the declaration
# removed, the textual fallback pass finds the name in a comment and passes --
# that is the check's documented second pass, not a defect, and asserting
# against it would encode an accident.
repo4=$(new_fixture deleted-symbol)
cat > "$repo4/subpkg/sub.go" <<'EOF'
package subpkg

type somethingElse struct{}

func somethingElse() int { return 2 }
EOF
out=$(run_check "$repo4"); st=$?
assert_run "a deleted subpackage function fails the check" 1 "subpkgOnly" "$out" "$st"
echo

# ---------------------------------------------------------------------------
echo "case 5: a repository with no audit document is a hard error"
# ---------------------------------------------------------------------------
# The check exits 2 when the document is missing. Silently passing there would
# make the gate green in a checkout where it has nothing to validate.
repo5=$(new_fixture no-doc)
rm -f "$repo5/docs/audit.md"
out=$(run_check "$repo5"); st=$?
assert_run "missing audit.md exits 2" 2 "audit.md not found" "$out" "$st"
echo

# ---------------------------------------------------------------------------
echo "case 6: an audit document with no anchors is a hard error"
# ---------------------------------------------------------------------------
# The other vacuous-pass shape: a reformat that drops every anchor would leave
# the check with nothing to verify and still printing success.
repo6=$(new_fixture no-anchors)
printf '# Fixture audit\n\nNothing anchored here.\n' > "$repo6/docs/audit.md"
out=$(run_check "$repo6"); st=$?
assert_run "anchor-less document exits 2" 2 "no anchors found" "$out" "$st"
echo

# ---------------------------------------------------------------------------
echo "case 7: the real repository still passes"
# ---------------------------------------------------------------------------
# A fixture can satisfy every case above while the real checkout is broken, so
# the real thing is asserted last, against the real script.
out=$(cd "$REPO" && python3 scripts/audit-drift-check.py 2>&1); st=$?
assert_run "the real repository passes its own check" 0 "all resolve" "$out" "$st"
echo

# ---------------------------------------------------------------------------
echo "case 8: the checked-in script still walks recursively"
# ---------------------------------------------------------------------------
# A direct structural assertion, and the cheapest one. The behavioural cases
# above all depend on the rglob expression staying in the shape they were
# written against; if it is refactored into something correct but different,
# the control in case 2 stops being able to locate it and the suite degrades
# quietly. This fails loudly instead.
if grep -q 'rglob' "$CHECK"; then
  ok "the checked-in script uses a recursive walk"
else
  bad "the checked-in script no longer uses rglob; rebuild the case-2 control"
fi
if grep -q 'sorted' "$CHECK"; then
  ok "the source set is sorted (stable corpus ordering)"
else
  bad "the source set is no longer sorted; corpus ordering is filesystem-dependent"
fi
echo

echo "----------------------------------------"
printf 'drift-selftest: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
