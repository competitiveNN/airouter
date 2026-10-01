#!/usr/bin/env bash
# Sync airouter config.yaml with the live free-model list.
# 1. fetch-free-models.py fetches/enriches the model list -> /tmp/free-models.json
# 2. maki rewrites config.yaml placing models in the right profile chains
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
# --probe-auto calls both chain terminators (kilocode kilo-auto/free, opencode
# big-pickle) with one real 1-token completion each and records a
# live/dead/unknown verdict in the model list. regenerate_config.py acts on it:
# it will not write a refused terminator, and it treats "no verdict" (a 429, a
# 5xx, a timeout) as no evidence at all. Two extra calls per run.
python3 "$PROJECT_DIR/fetch-free-models.py" --json --probe-auto --save "$MODELS_JSON"

cp "$PROJECT_DIR/config.yaml" "$BACKUP"

echo "[sync] invoking maki to rebuild config.yaml..."
if ! maki -p --yolo --exit-on-done --max-turns 40 \
    "$(cat "$PROJECT_DIR/scripts/sync-instruction.txt")"; then
    echo "[sync] ERROR: maki failed, restoring previous config" >&2
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
