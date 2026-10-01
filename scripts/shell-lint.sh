#!/usr/bin/env bash
# Lint the shell scripts for constructs that do not parse.
#
# WHY THIS EXISTS
#
# `while` and `until` take `do`. They never take `then` -- that is plain POSIX,
# not a host quirk:
#
#     while [ 1 -lt 2 ]; then   # syntax error, at every scope
#     while [ 1 -lt 2 ]; do     # fine
#     until [ 1 -lt 2 ]; do     # fine
#     if true; then            # fine
#
# It is worth a linter anyway because the failure is confusing rather than
# obvious. bash reports it at the `done`, several lines after the actual
# mistake, and the message points at an innocent loop. In a file with a dozen
# helper functions that is a long way from the line that is wrong, and it is
# easy to "fix" the wrong loop.
#
# The rule is enforced mechanically here because the alternative is that a
# future reader or formatter changes `do` to the more familiar `then` -- the
# change every other language's `while` would suggest -- and reintroduces a
# parse error in a file nobody was reading.
#
# scripts/shell-lint-control.sh proves this linter can actually reject
# something: it feeds the rules both offending and near-miss snippets and
# asserts that flagged code genuinely fails `bash -n` while unflagged code
# genuinely parses. A linter that has never been seen to fire is not known to
# work, and one that fires on valid code is worse than none.
#
# scripts/shell-lint-control.sh proves this linter can actually reject
# something: it feeds the rules both offending and near-miss snippets and
# asserts that flagged code genuinely fails `bash -n` while unflagged code
# genuinely parses. A linter that has never been seen to fire is not known to
# work, and one that fires on valid code is worse than none.
#
# Enforced on TRACKED scripts only (an untracked scratch file must not be able
# to fail the build). Untracked scripts under scripts/ are linted as a printed
# NOTICE so a brand-new file's parse errors surface immediately without being
# able to redden the gate; see the tail of this file.
#
# Usage: scripts/shell-lint.sh [--verbose]
# Exit 0 = no violations among tracked scripts. Exit 1 = at least one.
# Exit 2 = could not run.
set -uo pipefail

VERBOSE=0
[ "${1:-}" = "--verbose" ] && VERBOSE=1

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 2

command -v python3 >/dev/null 2>&1 || {
  echo "shell-lint: python3 not found" >&2; exit 2; }

# The TRACKED script set, not whatever is on disk. An untracked scratch file
# must not be able to fail the build, and a tracked file must not be able to
# hide from the check.
SCRIPTS=$(git ls-files '*.sh' 2>/dev/null)
if [ -z "$SCRIPTS" ]; then
  echo "shell-lint: no tracked shell scripts found" >&2
  exit 2
fi

rc=0
count=0
for f in $SCRIPTS; do
  count=$((count + 1))
  out=$(python3 scripts/shell_lint_rules.py "$f" 2>&1)
  status=$?
  if [ "$status" -ne 0 ] && [ "$status" -ne 1 ]; then
    echo "shell-lint: $f could not be checked:" >&2
    echo "$out" | sed 's/^/  /' >&2
    exit 2
  fi
  if [ "$status" -eq 1 ]; then
    [ "$VERBOSE" -eq 1 ] && echo "checking $f"
    echo "$out"
    rc=1
  elif [ "$VERBOSE" -eq 1 ]; then
    echo "ok $f"
  fi
done

if [ "$rc" -eq 0 ]; then
  echo "shell-lint: clean ($count script(s))"
fi

# ---------------------------------------------------------- untracked NOTICE --
# A second, NON-ENFORCED sweep over scripts/ that git does not track yet.
#
# The contract above is deliberately tracked-only: an untracked scratch file must
# not be able to fail the build. But that same rule means a script someone just
# wrote is invisible to the linter until it is committed, and a parse error in
# it is only found later -- possibly by the person who committed it, possibly
# never, if the file is a fixture. The gap is real and it is exactly the window
# in which new work is least reviewed.
#
# So untracked scripts are linted too, and reported as a NOTICE. It cannot fail
# the build (a scratch file with a `while ... then` in it is the operator's
# problem, not the gate's), but the violation is printed rather than swallowed.
# Once the file is tracked the same violation becomes an enforced failure with
# no further action: the sweep above picks it up on its own.
UNTRACKED=$(git ls-files --others --exclude-standard -- 'scripts/*.sh' 2>/dev/null)
if [ -n "$UNTRACKED" ]; then
  noticed=0
  for f in $UNTRACKED; do
    [ -f "$f" ] || continue
    out=$(python3 scripts/shell_lint_rules.py "$f" 2>&1)
    status=$?
    if [ "$status" -eq 1 ]; then
      echo "shell-lint: NOTICE $f has a parse violation but is untracked, so this is not enforced:"
      echo "$out" | sed 's/^/  /'
      noticed=$((noticed + 1))
    fi
  done
  if [ "$noticed" -eq 0 ]; then
    n=$(printf '%s\n' "$UNTRACKED" | grep -c .)
    echo "shell-lint: $n untracked script(s) under scripts/ linted, clean, not enforced until tracked"
  fi
fi

exit "$rc"
