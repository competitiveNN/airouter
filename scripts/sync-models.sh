#!/usr/bin/env bash
# Sync airouter config.yaml with the live free-model list.
# 0. cooldowns.json (the gateway's own record) is read for free-tier refusals,
#    so the sync starts by stating what the running gateway is being refused
# 1. fetch-free-models.py fetches/enriches the model list -> /tmp/free-models.json
#    (and vetoes every endpoint those refusals name)
# 2. regenerate_config.py rewrites config.yaml placing models in the right chains
# 3. config is validated; on failure the previous config is restored
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_DIR"

MODELS_JSON="${MODELS_JSON:-/tmp/free-models.json}"
BACKUP="$PROJECT_DIR/config.yaml.bak"
# The daemon resolves cooldowns.json next to the config it was started with
# (main.go), so the file that carries the runtime verdict is the one the
# gateway is actually writing.
COOLDOWNS="${COOLDOWNS:-$PROJECT_DIR/cooldowns.json}"

# Provider API keys for the fetch script (NVIDIA, AA enrichment, ...)
if [ -f "$PROJECT_DIR/.envrc" ]; then
    set -a
    # shellcheck disable=SC1091
    source "$PROJECT_DIR/.envrc"
    set +a
fi

echo "[sync] checking $COOLDOWNS for free-tier refusals..."
# Step 0, and it is a report rather than a fix on purpose: it makes the sync
# log say which endpoints the GATEWAY -- not any provider listing -- has been
# refused on. Without it a sync that silently drops a model reads identically to
# one that never knew the model existed.
#
# This is the half of the free-tier question the provider APIs cannot answer.
# cooldowns.json is written by the daemon (router.go), so it is the only record
# of what real client traffic does to an endpoint:
#
#   ollama:minimax-m3                        402 x4  "not included in your free
#                                                   usage" -- Ollama's free set
#                                                   is curated in
#                                                   fetch-free-models.py and
#                                                   never probed, so nothing
#                                                   upstream reports this
#   opencode:muse-spark-1.3-contributor-free 403 x42 FreeTierError -- the
#                                                   fetcher's own probe passes,
#                                                   because its body carries the
#                                                   stream+tools the provider's
#                                                   gate requires
#   nvidia{,2,3}:moonshotai/kimi-k2.6        404 x1  function deleted upstream,
#                                                   still listed in /v1/models
#
# The report touches no network, so it still answers when every provider is
# down. Transient cooldowns (429/5xx/timeouts, and the `circuits` section, which
# records probes in flight rather than verdicts) are counted and ignored.
python3 "$PROJECT_DIR/fetch-free-models.py" --cooldowns-report --cooldowns "$COOLDOWNS"

echo "[sync] fetching free model list..."
# --probe-auto calls both chain terminators (kilocode kilo-auto/free, opencode
# big-pickle) with one real 1-token completion each and records a
# live/dead/unknown verdict in the model list. regenerate_config.py acts on it:
# it will not write a refused terminator, and it treats "no verdict" (a 429, a
# 5xx, a timeout) as no evidence at all. Two extra calls per run.
#
# The same run also applies the step-0 refusals as a veto: a model the gateway
# has been refused on repeatedly is dropped from the list, so this step cannot
# re-add what step 0 just reported. Set NO_COOLDOWN_VETO=1 to read the report
# without acting on it (diagnostics only; it re-admits the dead endpoints).
if [ "${NO_COOLDOWN_VETO:-0}" = "1" ]; then
    echo "[sync] NO_COOLDOWN_VETO=1 — emitting every candidate the providers list"
    python3 "$PROJECT_DIR/fetch-free-models.py" --json --probe-auto \
        --no-cooldown-veto --cooldowns "$COOLDOWNS" --save "$MODELS_JSON"
else
    python3 "$PROJECT_DIR/fetch-free-models.py" --json --probe-auto \
        --cooldowns "$COOLDOWNS" --save "$MODELS_JSON"
fi

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
# regenerate_config.py implements rules 3-11 and 13 directly and is the procedure
# AGENTS.md documents for exactly this. It takes ~0.1s, is deterministic (same
# input, same output, every run), and creates no commits. Rule 12 (the runtime
# free-tier veto) is enforced one step earlier, by fetch-free-models.py, which is
# where the daemon's cooldowns.json is read.
#
# scripts/sync-instruction.txt is kept for reference and is still read by
# scripts/check-rules.py and scripts/test_regenerate_config.py, so the rules it
# describes stay in one place.
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
