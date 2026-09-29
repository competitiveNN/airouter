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

# Name given to the bundle's tip so a fetcher can reference it. Override only if
# it collides with a branch you actually have.
EXPORT_REF="${EXPORT_REF:-airouter-unpushed-export}"

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

# Give the bundle tip a real ref name. "git bundle create A..HEAD" records the
# tip as the ref "HEAD", not "refs/heads/master", and git fetch then refuses it:
#
#   fatal: couldn't find remote ref refs/heads/HEAD
#
# So the recovery instructions this script prints could not be followed -- the
# bundle verified, was non-empty, and was unfetchable. Creating a temporary
# local ref at HEAD and bundling that gives the bundle a name a fetch can ask
# for by. The ref is deleted afterwards; nothing about the repository changes.
git update-ref "refs/heads/$EXPORT_REF" HEAD
# shellcheck disable=SC2064  # expand now, not at trap time
trap "git update-ref -d refs/heads/$EXPORT_REF" EXIT
git bundle create dist/airouter-unpushed.bundle "refs/heads/$EXPORT_REF" --not "$UPSTREAM"
git format-patch "$UPSTREAM"..HEAD --stdout > dist/airouter-unpushed.patch

echo "exported $count commits ($UPSTREAM..HEAD):"
git log --oneline "$UPSTREAM"..HEAD
echo
echo "verify before relying on these:"
echo "  git clone <repo> /tmp/check && cd /tmp/check"
echo "  git fetch <abs-path>/dist/airouter-unpushed.bundle 'refs/heads/$EXPORT_REF:refs/heads/fb'"
echo "  git checkout fb && go build ./... && go test ./..."
echo
echo "note: $EXPORT_REF was a temporary local ref, created only so the bundle"
echo "has a name git fetch can resolve. It is not a branch on your machine."
