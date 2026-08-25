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
python3 "$PROJECT_DIR/fetch-free-models.py" --json --save "$MODELS_JSON"

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

rm -f "$BACKUP"
echo "[sync] config.yaml updated successfully"
