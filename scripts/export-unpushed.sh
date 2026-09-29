#!/usr/bin/env bash
# Export the commits that origin is missing, as a bundle and a patch.
#
# WHY THIS IS A SCRIPT AND NOT A COMMITTED ARTIFACT
#
# The obvious approach — commit the bundle — is self-referential and can never
# converge: committing the bundle creates a new commit, which the bundle then
# does not contain, and refreshing it creates another one. Every "refresh the
# bundle" commit makes the problem worse rather than solving it.
#
# So the bundle and patch are build artifacts, generated on demand and left
# untracked. This script is the committed part, plus dist/README.md explaining
# what to do with the output.
#
# Run it whenever the unpushed set changes, including right before handing the
# repo off to someone else.
set -euo pipefail

cd "$(dirname "$0")/.."

# The upstream the local branch diverged from. Override if the remote layout
# differs: UPSTREAM=origin/master ./scripts/export-unpushed.sh
UPSTREAM="${UPSTREAM:-origin/master}"

if ! git rev-parse --verify --quiet "$UPSTREAM" >/dev/null; then
  echo "error: $UPSTREAM does not exist. Fetch first, or set UPSTREAM." >&2
  exit 1
fi

count=$(git rev-list --count "$UPSTREAM"..HEAD)
if [ "$count" -eq 0 ]; then
  echo "nothing unpushed; $UPSTREAM is up to date with HEAD"
  exit 0
fi

mkdir -p dist
git bundle create dist/airouter-unpushed.bundle "$UPSTREAM"..HEAD
git format-patch "$UPSTREAM"..HEAD --stdout > dist/airouter-unpushed.patch

echo "exported $count commits ($UPSTREAM..HEAD):"
git log --oneline "$UPSTREAM"..HEAD
echo
echo "verify before relying on these:"
echo "  git clone <repo> /tmp/check && cd /tmp/check"
echo "  git fetch <abs-path>/dist/airouter-unpushed.bundle 'HEAD:refs/heads/fb'"
echo "  git checkout fb && go build ./... && go test ./..."
