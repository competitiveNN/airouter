#!/usr/bin/env bash
# Source the .envrc sitting next to the airouter binary and export every
# variable into systemd's service manager, so the daemon and the model-sync
# timer see the same credentials an interactive shell would.
#
# systemd's EnvironmentFile= deliberately does NOT understand shell syntax:
# it silently ignores `export KEY=value` lines, so pointing EnvironmentFile=
# at .envrc directly imports nothing. Sourcing it through bash and using
# `systemctl --user import-environment` is the one approach that also
# resolves intra-file references (e.g. CLOUDFLARE_API_TOKEN=$CF_API_KEY).
#
# Only names matching the allowlist are imported, so unrelated shell state
# is not leaked into the service manager.
#
# Usage: scripts/export-envrc.sh [--dry-run]

set -euo pipefail

# Repo root = the directory containing the airouter binary, per the task:
# the .envrc is a symlink living beside the binary.
BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVRC="$BIN_DIR/.envrc"

DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

if [[ ! -r "$ENVRC" ]]; then
  echo "error: cannot read $ENVRC" >&2
  exit 1
fi

# Names the gateway actually resolves through ProviderConfig.APIKeyEnv,
# plus the base URLs the config may reference.
ALLOWLIST=(
  NVIDIA_API_KEY NVIDIA_API_KEY_2 NVIDIA_API_KEY_3 NVIDIA_BASE_URL
  KILOCODE_API_KEY KILOCODE_BASE_URL
  OPENCODE_API_KEY OPENCODE_BASE_URL
  GEMINI_API_KEY GEMINI_API_KEY_2 GEMINI_API_KEY_3 GEMINI_API_KEY_4 GEMINI_API_KEY_5
  GEMINI_BASE_URL
  OLLAMA_API_KEY
  COMMANDCODE_API_KEY COMMANDCODE_API_KEY_2
  AIROUTER_API_KEY
)

# Source into a subshell, keep only allowlisted names, print NUL-separated
# bare variable NAMES.
#
# `systemctl import-environment` takes bare names, NOT KEY=value pairs: it
# rejects the latter outright ("Not a valid environment variable name").
# It then copies each name's value out of ITS OWN environment, so it must be
# invoked with the sourced values in scope -- hence the bash -c wrapper that
# sources .envrc and execs systemctl itself.
collect_names() {
  bash -c '
    set -a
    . "$1"
    set +a
    for name in "${@:2}"; do
      [[ -n "${!name:-}" ]] && printf "%s\0" "$name"
    done
  ' _ "$ENVRC" "${ALLOWLIST[@]}"
}

if [[ $DRY_RUN -eq 1 ]]; then
  echo "would import from $ENVRC:"
  bash -c '
    set -a; . "$1"; set +a
    for name in "${@:2}"; do
      [[ -n "${!name:-}" ]] && printf "  %s (len %d)\n" "$name" "${#name}"
    done
  ' _ "$ENVRC" "${ALLOWLIST[@]}"
  exit 0
fi

# One systemctl call, with .envrc already sourced so import-environment can
# read the values out of its own environment.
bash -c '
  set -a
  . "$1"
  set +a
  names=()
  for name in "${@:2}"; do
    [[ -n "${!name:-}" ]] && names+=("$name")
  done
  ((${#names[@]})) || exit 0
  systemctl --user import-environment "${names[@]}"
  printf "imported %d environment variable(s) from %s\n" "${#names[@]}" "$1"
' _ "$ENVRC" "${ALLOWLIST[@]}"
