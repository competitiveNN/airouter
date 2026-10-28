#!/usr/bin/env bash
# Sync airouter config.yaml with the live free-model list.
# 1. fetch-free-models.py fetches/enriches the model list -> /tmp/free-models.json
# 2. regenerate_config.py rewrites config.yaml placing models in the right chains
# 3. config is validated; on failure the previous config is restored
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_DIR"

MODELS_JSON="${MODELS_JSON:-/tmp/free-models.json}"
BACKUP="$PROJECT_DIR/config.yaml.bak"

# Provider API keys for the fetch script (NVIDIA, AA enrichment, ...)
if [ -f "$PROJECT_DIR/.envrc" ]; then
    set -a
    # shellcheck disable=SC1091
    source "$PROJECT_DIR/.envrc"
    set +a
fi

echo "[sync] fetching free model list..."
# The fetcher issues a few dozen API calls. --probe-auto calls both chain
# terminators (kilocode kilo-auto/free, opencode big-pickle) with one real
# 1-token completion each and records a live/dead/unknown verdict in the model
# list. regenerate_config.py acts on it: it will not write a refused terminator,
# and it treats "no verdict" (a 429, a 5xx, a timeout) as no evidence at all.
# Two extra calls per run.
python3 "$PROJECT_DIR/fetch-free-models.py" --json --probe-auto \
    --save "$MODELS_JSON"

cp "$PROJECT_DIR/config.yaml" "$BACKUP"

echo "[sync] regenerating config.yaml..."
# regenerate_config.py, NOT a coding agent. This step used to shell out to
# `maki` with scripts/sync-instruction.txt, and that was wrong in four
# independent ways, each of which had already broken the sync in production:
#
#  1. The `maki` on PATH is a wrapper that always injects --yolo, and the
#     invocation passed it again, so clap refused:
#       error: the argument '--yolo' cannot be used multiple times
#  2. With that fixed, the instruction text passed as an argument died on the
#     first word, because `maki [OPTIONS] [PROMPT] [COMMAND]` parses the first
#     positional as a subcommand and the file opens "Task: regenerate ...":
#       error: unexpected argument 'regenerate' found
#  3. With that fixed too, the step actually ran -- and took longer than the
#     unit's TimeoutStartSec, so the sync was killed at 30 minutes and had to
#     restore. Measured: 8.4s of CPU across 30 minutes of wall clock, i.e. it
#     was mostly idle, and 1G peak RSS.
#  4. A scheduled sync that drives a coding agent makes GIT COMMITS as a side
#     effect. A nightly model sync must not be able to commit to the repo.
#
# regenerate_config.py implements rules 3-11 and 12 directly and is the procedure
# AGENTS.md documents for exactly this. It takes ~0.1s, is deterministic (same
# input, same output, every run), and creates no commits. The rules live in
# scripts/sync-instruction.txt, which scripts/check-rules.py and
# scripts/test_regenerate_config.py still read, so they stay in one place.
#
# What the fetcher cannot know is whether the GATEWAY can still use an endpoint:
# a provider listing says a model is free while real traffic is refused (a 402
# billing response, a 403 free-tier error, a 404 on a deleted function). No
# provider API reports that class of failure. It is recorded in cooldowns.json,
# which the daemon owns and acts on at request time -- the cooldown is the
# backoff, and the sync does not second-guess it.
if ! python3 "$PROJECT_DIR/regenerate_config.py" --write; then
    echo "[sync] ERROR: regeneration failed, restoring previous config" >&2
    cp "$BACKUP" "$PROJECT_DIR/config.yaml"
    exit 1
fi

echo "[sync] validating config..."
if ! python3 "$PROJECT_DIR/scripts/validate-config.py"; then
    echo "[sync] ERROR: invalid config, restoring previous one" >&2
    cp "$BACKUP" "$PROJECT_DIR/config.yaml"
    exit 1
fi

# Structural validity is not the same as obeying the distribution rules, and
# only the second one needs the model list — which step 1 just wrote, so this is
# the one place the rules are enforced against real data instead of asserted
# from a transcript. Key-group completeness (the nvidia trio / commandcode
# pair), the auto-fallback terminator, vision+intelligence sync against the
# fetched records, and dead/invented ids all fail the sync here.
echo "[sync] checking distribution rules against $MODELS_JSON..."
if ! python3 "$PROJECT_DIR/scripts/check-rules.py" \
        --config "$PROJECT_DIR/config.yaml" --models "$MODELS_JSON"; then
    echo "[sync] ERROR: config violates the distribution rules, restoring previous one" >&2
    cp "$BACKUP" "$PROJECT_DIR/config.yaml"
    exit 1
fi

rm -f "$BACKUP"
echo "[sync] config.yaml updated successfully"

# Commit and push any tracked files the sync changed. The nightly sync
# regenerates config.yaml (which is gitignored and never committed) and may
# update first-seen-cache.json when a new model appears or a month passes
# without a listing. Only tracked files are committed; config.yaml and
# cooldowns.json stay out of the tree by design.
if git -C "$PROJECT_DIR" diff --quiet -- first-seen-cache.json blacklisted-models.json; then
    echo "[sync] first-seen-cache.json unchanged, nothing to commit"
else
    echo "[sync] committing updated caches..."
    # Identify ourselves for the commit (systemd units have no user context).
    git -C "$PROJECT_DIR" config user.name "${SYNC_GIT_USER:-airouter-sync}"
    git -C "$PROJECT_DIR" config user.email "${SYNC_GIT_EMAIL:-airouter-sync@localhost}"
    git -C "$PROJECT_DIR" add first-seen-cache.json blacklisted-models.json
    git -C "$PROJECT_DIR" commit -m "sync: update cache state from $(date +%F)" >/dev/null
    echo "[sync] pushing commit..."
    # Refresh the remote-tracking ref so --force-with-lease has a baseline to
    # compare against. This repo has a single operator (the sync service), so
    # after filter-repo rewrote every commit hash when config.yaml was purged
    # from history, local master and origin/master no longer share an ancestor.
    # --force-with-lease then aborts if someone else pushed to origin in the
    # meantime, which is safer than a bare --force.
    git -C "$PROJECT_DIR" fetch origin >/dev/null
    git -C "$PROJECT_DIR" push --force-with-lease origin HEAD:master
    echo "[sync] commit pushed"
fi
