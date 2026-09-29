#!/usr/bin/env bash
# Fail if a credential is committed, tracked, or embedded in a local git URL.
#
# Why this exists: a GitHub PAT was found sitting in plaintext in
# .git/config's remote URL (`https://<token>@github.com/...`). It had not been
# committed, but it was on disk in cleartext, appeared in every `git remote -v`,
# and would show up in the process table of any `git push`. Nothing in CI
# would have flagged it, because CI never looks at .git/config.
#
# The two things that make credentials leak into git are both checked here:
#   1. committing a file that contains a token (tracked-file scan + history scan)
#   2. putting the token in the remote URL (.git/config scan)
#
# The local credential store (~/.git-credentials) is intentionally NOT scanned:
# it is expected to hold a token, it is outside the repo, and it is chmod 600.
#
# Usage: scripts/secret-scan.sh        # exit 1 on any finding
#        scripts/secret-scan.sh --quiet  # findings only, no summary

set -uo pipefail

QUIET=0
[ "${1:-}" = "--quiet" ] && QUIET=1

cd "$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not a git repository" >&2
  exit 2
}

findings=0
report() { # file, line-number, matched-token
  findings=$((findings + 1))
  # Print the file and line but never the token itself.
  echo "SECRET ${3:-found} at $1:$2" >&2
}

# Credential shapes we refuse to see in tracked content. Kept specific enough
# that documentation and fixtures (e.g. "api_key_env", "ci-test-key") do not
# trip them, and broad enough to catch the common token families.
PATTERNS='ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,}|gho_[A-Za-z0-9]{36}|ghu_[A-Za-z0-9]{36}|ghs_[A-Za-z0-9]{36}|ghr_[A-Za-z0-9]{36}|glpat-[A-Za-z0-9_-]{20}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----'

[ "$QUIET" -eq 0 ] && echo "secret-scan: checking tracked content, history, and local git config"

# --- 1. Working tree, tracked files only -------------------------------------
# Untracked runtime state and build output are excluded on purpose: a
# developer's local .env or cache is not what gets published.
#
# Only the sanitized report is printed. Letting raw grep output through would
# copy the secret into the CI log, turning a detection into a second exposure —
# CI logs are readable by everyone with repo access and are retained.
# `grep -o` extracts just the match; we replace it before anything is printed.
while IFS= read -r f; do
  [ -n "$f" ] || continue
  ln=$(grep -nEo "$PATTERNS" "$f" 2>/dev/null | head -1 | cut -d: -f1)
  report "$f" "${ln:-?}" "in tracked file"
done < <(git ls-files -z | xargs -0 -r grep -lE "$PATTERNS" 2>/dev/null)

# --- 2. Full history ----------------------------------------------------------
# A token that was committed and later removed still lives in history, so a
# green working-tree scan is not sufficient. Bounded to the last 200 commits to
# keep this fast; widen with SECRETSCAN_COMMITS if needed.
COMMITS="${SECRETSCAN_COMMITS:-200}"
if git log -p --all -n "$COMMITS" 2>/dev/null | grep -nE "$PATTERNS" >/dev/null; then
  report "git history" "-" "in a commit (last $COMMITS commits)"
  if [ "$QUIET" -eq 0 ]; then
    echo "  locate it with:" >&2
    echo "    git log --all -S'ghp_' --oneline" >&2
    echo "    git log --all -S'ghp_' -p -- . | less" >&2
    echo "  if it was never meant to be public, rotate the token — history" >&2
    echo "  rewrites are not enough on their own." >&2
  fi
fi

# --- 3. Local git config (not committed, but still plaintext on disk) --------
if [ -f .git/config ]; then
  if grep -nE 'https?://[^/@[:space:]]+:[^/@[:space:]]+@|https?://[^/@[:space:]]+@' .git/config >/dev/null 2>&1; then
    if grep -nE "$PATTERNS" .git/config >/dev/null 2>&1; then
      report ".git/config" "-" "embedded in a remote URL"
      if [ "$QUIET" -eq 0 ]; then
        echo "  fix with:" >&2
        echo "    git remote set-url origin https://github.com/<owner>/<repo>" >&2
        echo "  and rotate the token — it was readable in cleartext on disk." >&2
      fi
    fi
  fi
fi

if [ "$findings" -gt 0 ]; then
  echo "secret-scan: FAILED with $findings finding(s)" >&2
  exit 1
fi

[ "$QUIET" -eq 0 ] && echo "secret-scan: clean (tracked content, history, .git/config)"
exit 0
