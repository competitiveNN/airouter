#!/usr/bin/env bash
# Self-test for scripts/shell-lint.sh and scripts/shell_lint_rules.py.
#
# A linter that has never been seen to reject anything is not known to work. A
# linter that rejects valid code is worse than none: it trains people to ignore
# it, and it makes the real signal unmissable only by burying it.
#
# So this tests BOTH directions, and it does not take the linter's word for
# anything. Every case asserts against `bash -n` itself:
#
#   expect_flag  -- the linter must reject it AND bash must genuinely fail
#                   to parse it. If the linter flags something bash accepts,
#                   the rule is wrong and the case fails.
#   expect_clean -- the linter must accept it AND bash must genuinely parse
#                   it. If the linter accepts something bash rejects, the rule
#                   has a hole.
#
# The near-miss cases are the valuable half. `if ... then` is valid and must
# never be flagged; a `then` inside a comment or a string is not code.
#
# Usage: scripts/shell-lint-control.sh
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RULES="$REPO/scripts/shell_lint_rules.py"
[ -r "$RULES" ] || { echo "shell-lint-control: cannot read $RULES" >&2; exit 2; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/shell-lint-control.XXXXXX") || exit 2
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

# expect_flag <name> <expected-substring> <snippet>
expect_flag() {
  local name="$1" want="$2" snippet="$3"
  local f="$TMP/$name.sh"
  printf '%s\n' "$snippet" > "$f"
  local out status
  out=$(python3 "$RULES" "$f" 2>&1); status=$?
  if [ "$status" -ne 1 ]; then
    bad "$name (not flagged; exit $status)"
    return
  fi
  if ! printf '%s' "$out" | grep -qF -- "$want"; then
    bad "$name (flagged for the wrong reason: $out)"
    return
  fi
  if bash -n "$f" 2>/dev/null; then
    bad "$name (flagged, but bash parses it here -- the rule is wrong)"
    return
  fi
  ok "$name"
}

# expect_clean <name> <snippet>
expect_clean() {
  local name="$1" snippet="$2"
  local f="$TMP/$name.sh"
  printf '%s\n' "$snippet" > "$f"
  local out status
  out=$(python3 "$RULES" "$f" 2>&1); status=$?
  if [ "$status" -eq 1 ]; then
    bad "$name (flagged, but it is valid: $out)"
    return
  fi
  if ! bash -n "$f" 2>/dev/null; then
    bad "$name (not flagged, but bash cannot parse it -- the rule has a hole)"
    return
  fi
  ok "$name"
}

# A snippet whose embedded single quotes must survive the heredoc.
Q="'"

echo "shell-lint control: `while`/`until` terminated by `then`"
expect_flag while-then-toplevel 'then' 'i=0
while [ "$i" -lt 3 ]; then
  i=$((i + 1))
done'
expect_flag while-then-in-func 'then' 'f() {
  local i=0 n=3
  while [ "$i" -lt "$n" ]; then
    i=$((i + 1))
  done
}'
expect_flag until-then 'then' 'until [ 1 -lt 2 ]; then
  break
done'
expect_flag while-arith-then 'then' 'i=0
while (( i < 3 )); then
  i=$((i + 1))
done'

echo "shell-lint control: the valid forms must survive"
expect_clean while-do-toplevel 'i=0
while [ "$i" -lt 3 ]; do
  i=$((i + 1))
done'
expect_clean while-do-in-func 'f() {
  local i=0 n=3
  while [ "$i" -lt "$n" ]; do
    i=$((i + 1))
  done
}'
expect_clean until-do 'until [ 1 -lt 2 ]; do
  break
done'
expect_clean while-test-do 'f() {
  while test 1 -lt 2; do
    break
  done
}'
# `then` belongs to `if` and `case`. Flagging these would be a false positive
# on correct, extremely common code.
expect_clean if-then-toplevel 'if true; then
  echo ok
fi'
expect_clean if-then-in-func 'f() {
  if [ -n "$x" ]; then
    echo ok
  fi
}'
expect_clean nested-if-then 'f() {
  if true; then
    while [ 1 -lt 2 ]; do
      if true; then
        echo ok
      fi
    done
  fi
}'

echo "shell-lint control: then in comments and strings is not code"
expect_clean then-in-comment '# while [ 1 -lt 2 ]; then
echo ok'
expect_clean then-in-comment-in-func 'f() {
  # while [ 1 -lt 2 ]; then
  echo ok
}'
expect_clean then-in-single-quotes 'echo '"'"'while [ 1 -lt 2 ]; then'"'"''
expect_clean then-in-double-quotes 'echo "while [ 1 -lt 2 ]; then"'
expect_clean then-in-double-quotes-in-func 'f() {
  echo "while [ 1 -lt 2 ]; then"
}'

echo "shell-lint control: the real repository is clean"
if bash "$REPO/scripts/shell-lint.sh" >/dev/null 2>&1; then
  ok "every tracked script passes"
else
  bad "every tracked script passes (run scripts/shell-lint.sh)"
fi

echo ""
echo "----------------------------------------"
printf 'shell-lint control: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
