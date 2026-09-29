#!/usr/bin/env bash
# Launch airouter with the API keys from the .envrc sitting next to the
# binary exported into the process environment.
#
# Why a wrapper instead of systemd's EnvironmentFile=:
# EnvironmentFile= is not a shell parser. It reads `KEY=value` lines and
# silently ignores anything else — including the `export KEY=value` form
# that direnv writes. Verified against systemd 252: pointing EnvironmentFile=
# at this .envrc imports zero variables, with no warning. Sourcing it with
# bash is also what resolves intra-file references such as
# CLOUDFLARE_API_TOKEN=$CF_API_KEY, which EnvironmentFile= would leave empty.
#
# `exec` replaces this shell so the daemon keeps the real PID and systemd
# still supervises it directly (no orphaned child, correct restart behaviour).
set -euo pipefail

# This script lives in scripts/, but the .envrc sits next to the BINARY, i.e.
# the repository root one level up. Resolve the root from the script location
# rather than from CWD, which systemd sets to WorkingDirectory but which a
# manual invocation may not set at all.
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/airouter"
ENVRC="$REPO_DIR/.envrc"

if [[ ! -x "$BIN" ]]; then
  echo "airouter: binary not found or not executable at $BIN" >&2
  exit 1
fi

if [[ ! -r "$ENVRC" ]]; then
  # Fail closed. Starting without credentials looks like it works (the daemon
  # starts and /v1/models responds) but every provider call fails 401, which
  # is far harder to diagnose than refusing to start.
  echo "airouter: no .envrc next to the binary at $ENVRC" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
. "$ENVRC"
set +a

exec "$BIN" "$@"
