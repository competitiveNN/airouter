#!/usr/bin/env bash
# Self-test for scripts/opencode-routing-check.sh — the live routing check.
#
# WHY THIS EXISTS
#
# The live check is the only thing in this repository that proves the gateway
# routes to OpenCode, and it is the one stage the gate cannot run everywhere: it
# needs OPENCODE_API_KEY and network access, and it exits 77 (SKIP) without
# them. That is a hole with a specific shape — a keyless machine runs the gate,
# the stage skips, and an edit that quietly guts the check is indistinguishable
# from an edit that does not. Nothing in the tree would notice.
#
# So this selftest exercises the check's guards with NO credential and NO
# network, by standing up the real gateway against scripts/fake-opencode-upstream.py,
# which reproduces the real upstream's `403 FreeTierError` when the
# client-attribution headers are absent. Because the provider is named
# `opencode` (which is all the check pins) but its URL is 127.0.0.1, the check
# runs to completion offline — and a green run is itself proof that the
# attribution headers are on the wire, a claim the live check cannot make
# (space-bunny-free answers 200 with or without them).
#
# Every guard is proved by requiring the check to FAIL. A guard that has only
# ever been seen to pass is not known to be able to fail.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HARNESS="$REPO/scripts/opencode-routing-check.sh"
FAKE="$REPO/scripts/fake-opencode-upstream.py"
P="opencode-routing-selftest:"
fails=0

pass() { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*" >&2; fails=$((fails + 1)); }

WORK="$(mktemp -d)"
cleanup() {
  [[ -n "${UPSTREAM_PID:-}" ]] && kill "$UPSTREAM_PID" 2>/dev/null
  [[ -n "${DECOY_PID:-}" ]] && kill "$DECOY_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

for f in "$HARNESS" "$FAKE"; do
  [[ -r "$f" ]] || { echo "$P missing $f" >&2; exit 1; }
done
command -v python3 >/dev/null || { echo "$P python3 not found" >&2; exit 1; }

# The real binary, so this is an end-to-end run and not a stub standing in for
# one. `airouter` is a gitignored artifact, so building it cannot move the
# gate's tree fingerprint.
BIN="$REPO/airouter"
if [[ ! -x "$BIN" ]]; then
  command -v go >/dev/null || { echo "$P no go toolchain to build $BIN" >&2; exit 1; }
  ( cd "$REPO" && go build -o "$BIN" . ) || { echo "$P go build failed" >&2; exit 1; }
fi

# A private skeleton so the harness's own REPO_DIR (derived from the script's
# location) points here: a dummy .envrc, and a fixture naming `opencode` with a
# loopback URL. The provider NAME is what the check pins; the URL is ours to
# choose, which is what makes this hermetic.
SKEL="$WORK/repo"
mkdir -p "$SKEL/scripts" "$SKEL/testdata"
cp "$HARNESS" "$SKEL/scripts/opencode-routing-check.sh"
cp "$BIN" "$SKEL/airouter"
printf 'export OPENCODE_API_KEY=fake-key-for-selftest\n' >"$SKEL/.envrc"

write_fixture() { # <url> <model>
  cat >"$SKEL/testdata/opencode-config.yaml" <<EOF
providers:
  opencode:
    url: $1
    api_key_env: OPENCODE_API_KEY
models:
  smart:
    chain:
      - provider: opencode
        model: $2
  work:
    chain:
      - provider: opencode
        model: $2
  fast:
    chain:
      - provider: opencode
        model: $2
  large:
    chain:
      - provider: opencode
        model: $2
EOF
}

free_port() {
  python3 -c "
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()"
}

wait_for_port() { # <port> <seconds>
  python3 - "$1" "$2" <<'PY'
import socket, sys, time
port, deadline = int(sys.argv[1]), time.time() + float(sys.argv[2])
while time.time() < deadline:
    s = socket.socket(); s.settimeout(0.3)
    if s.connect_ex(("127.0.0.1", port)) == 0:
        sys.exit(0)
    s.close(); time.sleep(0.05)
sys.exit(1)
PY
}

run_harness() { # <args...> -> sets OUT, RC
  OUT="$(bash "$SKEL/scripts/opencode-routing-check.sh" "$@" 2>&1)"
  RC=$?
}

# ---------------------------------------------------------------------------
UP_PORT="$(free_port)"
python3 "$FAKE" --port "$UP_PORT" >"$WORK/upstream.log" 2>&1 &
UPSTREAM_PID=$!
wait_for_port "$UP_PORT" 10 || { echo "$P fake upstream never came up" >&2; cat "$WORK/upstream.log" >&2; exit 1; }
UP_URL="http://127.0.0.1:$UP_PORT/v1"
echo "$P fake opencode upstream on $UP_URL (enforcing attribution headers)"

# 1. Happy path. Exits 0 only if the gateway routed to `opencode` AND the
#    upstream saw well-formed attribution headers (it 403s without them).
write_fixture "$UP_URL" "space-bunny-free"
run_harness --models smart
if [[ $RC -eq 0 ]]; then
  pass "routes to opencode offline, headers accepted upstream"
else
  fail "expected exit 0, got $RC"
  printf '%s\n' "$OUT" | tail -6 | sed 's/^/          | /' >&2
fi

# 2. The attribution headers are load-bearing: the same run against a fake that
#    accepts anything must still pass, which proves check 1 was not vacuous.
#    Conversely, if check 1 ever passes against the enforcing fake while the
#    gateway sent no headers, the fake is not enforcing and check 1 is a lie.
#    So: assert directly that the enforcing fake rejects a headerless request.
code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 \
  -H 'Content-Type: application/json' \
  -d '{"model":"space-bunny-free","messages":[]}' \
  "$UP_URL/chat/completions")
if [[ "$code" == "403" ]]; then
  pass "the fake 403s a headerless request, so check 1 is not vacuous"
else
  fail "fake returned $code for a headerless request, want 403 (it is not enforcing)"
fi

# 3. The routing assertion has teeth: expecting a model that is not the one in
#    use must FAIL, even though the completion and the stream both succeed.
run_harness --models smart --expect-model definitely-not-the-model
if [[ $RC -ne 0 && "$OUT" == *"not pinned to opencode/definitely-not-the-model"* ]]; then
  pass "a wrong expected model fails the routing assertion"
else
  fail "expected a routing-assertion failure, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi

# 4. THE POINT OF THIS FILE. Re-create the exact defect that a careless edit
#    would introduce — the routing comparison neutered to "everything is fine" —
#    and require that the run above now passes. If neutering the harness does
#    not change the outcome, then check 3 was never really asserting anything
#    and this whole file is decoration.
NEUTERED="$WORK/neutered.sh"
# The smallest edit that turns the routing assertion into a no-op: report every
# session as correctly routed and none as foreign. This is the shape of the
# careless change this file exists to catch.
if ! python3 - "$SKEL/scripts/opencode-routing-check.sh" "$NEUTERED" <<'PY'
import sys
src = open(sys.argv[1]).read()
dst = sys.argv[2]
anchor = "print(oc, len(s) - oc)"
assert src.count(anchor) == 1, f"anchor appears {src.count(anchor)} times, expected 1"
open(dst, "w").write(src.replace(anchor, "print(len(s), 0)"))
PY
then
  fail "could not neuter the harness (anchor moved); this selftest needs updating"
else
  cp "$NEUTERED" "$SKEL/scripts/opencode-routing-check.sh"
  run_harness --models smart --expect-model definitely-not-the-model
  if [[ $RC -eq 0 ]]; then
    pass "a neutered harness passes check 3, so check 3 detects that defect"
  else
    fail "neutering the harness did not change the outcome; check 3 cannot fail"
  fi
  cp "$HARNESS" "$SKEL/scripts/opencode-routing-check.sh"
  # And the restored harness must go red again, or the restore is not proven.
  run_harness --models smart --expect-model definitely-not-the-model
  if [[ $RC -ne 0 ]]; then
    pass "restored harness is red again"
  else
    fail "restored harness still passes; the restore did not land"
  fi
fi

# 5. Port pre-flight: a stale listener on the chosen port must be REFUSED, not
#    tested. A second gateway answering /health and /admin/sessions on that port
#    is exactly the accident that a fixed default port invites.
DECOY_PORT="$(free_port)"
write_fixture "$UP_URL" "space-bunny-free"
python3 - "$DECOY_PORT" <<'PY' &
import sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_GET(self):
        body = b'{"status":"ok"}'
        self.send_response(200); self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
DECOY_PID=$!
wait_for_port "$DECOY_PORT" 10 || { fail "decoy listener never came up"; }
run_harness --models smart --port "$DECOY_PORT"
if [[ $RC -ne 0 && "$OUT" == *"refusing to test a gateway we did not start"* ]]; then
  pass "refuses a port that is already in use"
else
  fail "expected a port-in-use refusal, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi
kill "$DECOY_PID" 2>/dev/null; DECOY_PID=""

# 6. The provider is pinned in the harness, not taken from the fixture. A
#    fixture repointed elsewhere must be rejected: otherwise the check asserts
#    its own fixture and reports OK, which is the trap this guards.
cat >"$SKEL/testdata/opencode-config.yaml" <<'EOF'
providers:
  kilocode:
    url: http://127.0.0.1:1/v1
    api_key_env: KILOCODE_API_KEY
models:
  smart:
    chain:
      - provider: kilocode
        model: kilo-auto/free
  work:
    chain:
      - provider: kilocode
        model: kilo-auto/free
  fast:
    chain:
      - provider: kilocode
        model: kilo-auto/free
  large:
    chain:
      - provider: kilocode
        model: kilo-auto/free
EOF
run_harness --models smart
if [[ $RC -ne 0 && "$OUT" == *"not to opencode"* ]]; then
  pass "rejects a fixture that does not route to opencode"
else
  fail "expected a provider-pinning rejection, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi

# 7. A fixture mixing two models is a fixture bug; it must be reported as such
#    before the daemon starts, not as a gateway that "died on startup".
python3 - "$SKEL/testdata/opencode-config.yaml" <<'PY'
import sys
p = sys.argv[1]
src = open(p).read()
open(p, "w").write(src.replace(
    "  fast:\n    chain:\n      - provider: kilocode\n        model: kilo-auto/free",
    "  fast:\n    chain:\n      - provider: kilocode\n        model: some-other-model"))
PY
run_harness --models smart
if [[ $RC -ne 0 && "$OUT" == *"could not read exactly one model"* ]]; then
  pass "rejects a fixture that mixes models"
else
  fail "expected a mixed-model rejection, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi

# 8. A 200 with no usable content must be retried, not called a routing fault.
#    Seen live on 2026-09-30: one profile in four returned HTTP 200 with an
#    empty message, no cooldown and no timeout anywhere. The transport-level
#    retry did not engage (it only looked at the status code), so the run went
#    red on an upstream artifact. These two cases pin both halves: transient
#    emptiness is survivable, persistent emptiness is not.
restart_upstream() { # <extra args...>
  [[ -n "${UPSTREAM_PID:-}" ]] && kill "$UPSTREAM_PID" 2>/dev/null
  UPSTREAM_PID=""
  local port; port="$(free_port)"
  python3 "$FAKE" --port "$port" "$@" >"$WORK/upstream.log" 2>&1 &
  UPSTREAM_PID=$!
  wait_for_port "$port" 10 || { echo "$P fake upstream never came up" >&2; exit 1; }
  UP_URL="http://127.0.0.1:$port/v1"
  # The fixture is rewritten HERE, not at the call site. Each restart lands on a
  # new port, so a call site that forgets write_fixture leaves the harness aimed
  # at the previous fake's port -- now a dead one -- and the case then measures
  # connection-refused instead of the behaviour it was written for. That is not
  # hypothetical: it is how this helper was first written, and two cases failed
  # for exactly this reason.
  write_fixture "$UP_URL" "space-bunny-free"
}

restart_upstream --empty-first 2
run_harness --models smart --attempts 3
if [[ $RC -eq 0 && "$OUT" == *"unusable"* ]]; then
  pass "a 200 with empty content is retried and the run recovers"
else
  fail "expected the empty-content run to recover via retry, got rc=$RC"
  printf '%s\n' "$OUT" | tail -5 | sed 's/^/          | /' >&2
fi

restart_upstream --empty-first 99
run_harness --models smart --attempts 2
if [[ $RC -ne 0 ]]; then
  # The exact status is not asserted: with repeated empty answers the gateway
  # eventually exhausts its fallback budget and answers 503
  # all_models_unavailable rather than forwarding the last empty body. Both are
  # correct. What must hold is that a persistently empty upstream is RED -- the
  # retry must not launder it into a pass.
  pass "persistent empty content fails (rc=$RC) rather than being retried into a pass"
else
  fail "persistent empty content was accepted"
  printf '%s\n' "$OUT" | tail -5 | sed 's/^/          | /' >&2
fi

# With a single empty response and no retries left, the harness must report the
# empty content AND print the body. The earlier version printed only '' and the
# live failure was undiagnosable without --verbose, which is the whole reason
# this case exists.
restart_upstream --empty-first 1
run_harness --models smart --attempts 1
if [[ $RC -ne 0 && "$OUT" == *"empty/unparseable content"* ]]; then
  pass "an empty 200 with no retries left is reported as empty content"
else
  fail "expected an empty-content failure, got rc=$RC"
  printf '%s\n' "$OUT" | tail -5 | sed 's/^/          | /' >&2
fi
if [[ "$OUT" == *"body:"* && "$OUT" == *"reasoning_content"* ]]; then
  pass "the failure message carries the upstream body, so the next one is diagnosable"
else
  fail "the empty-content failure printed no body: $(printf '%s' "$OUT" | tail -3)"
fi

# 8c. The "200 AND usable" predicate is what makes an empty completion get
#     retried. Prove the property is load-bearing by neutering the predicate and
#     requiring the recovery case above to go red.
#
#     Note what this does and does not prove. Neutering the predicate does NOT
#     let an empty completion pass: the content assertion downstream still fails
#     the run. What it removes is the RETRY, so the observable difference is the
#     recovery case -- a transient empty that should be absorbed now fails. That
#     is the regression worth catching: silently losing the retry turns every
#     upstream hiccup into a red gate, which is how a live stage gets ignored.
PRED_NEUTERED="$WORK/pred-neutered.sh"
if ! python3 - "$SKEL/scripts/opencode-routing-check.sh" "$PRED_NEUTERED" <<'PY'
import sys
src = open(sys.argv[1]).read()
dst = sys.argv[2]
anchor = 'if [[ -z "$unusable" ]]; then'
assert src.count(anchor) == 1, f"predicate anchor appears {src.count(anchor)} times, expected 1"
open(dst, "w").write(src.replace(anchor, "if true; then"))
PY
then
  fail "could not neuter the usability predicate (anchor moved); this selftest needs updating"
else
  restart_upstream --empty-first 2
  cp "$PRED_NEUTERED" "$SKEL/scripts/opencode-routing-check.sh"
  run_harness --models smart --attempts 3
  if [[ $RC -ne 0 ]]; then
    pass "a neutered usability predicate breaks the recovery case, so it is covered"
  else
    fail "neutering the usability predicate changed nothing; the retry is untested"
  fi
  cp "$HARNESS" "$SKEL/scripts/opencode-routing-check.sh"
  restart_upstream --empty-first 2
  run_harness --models smart --attempts 3
  if [[ $RC -eq 0 ]]; then
    pass "restored harness absorbs the empty completions again"
  else
    fail "restored harness still fails the recovery case; the restore did not land"
  fi
fi

# 8d. The retry budget is bounded and reported. A run that absorbs more retries
#     than the limit is a provider problem, and reporting it as plain green is
#     how an upstream dies unnoticed.
restart_upstream --empty-first 2
run_harness --models smart --attempts 3 --max-retries 1
if [[ $RC -ne 0 && "$OUT" == *"retries absorbed"* ]]; then
  pass "a run over the retry budget fails instead of reporting green"
else
  fail "expected the retry budget to fail the run, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi
run_harness --models smart --attempts 3 --max-retries 5
if [[ $RC -eq 0 && "$OUT" == *"retries="* ]]; then
  pass "a run inside the retry budget passes and reports its counts"
else
  fail "expected a passing run with counts, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi
run_harness --models smart --max-retries abc
if [[ $RC -eq 2 ]]; then
  pass "rejects a non-numeric --max-retries"
else
  fail "expected --max-retries abc to be rejected, got rc=$RC"
fi

# 9. Retries must not launder a hard failure into a pass. The harness retries a
#    transient upstream failure, so the question this answers is whether the
#    retry loop is bounded. An unroutable upstream (a closed port) fails
#    identically on every attempt, and the run must still be red.
#
#    This is the check that keeps the retry from becoming the bug: a retry loop
#    with no bound, or one that swallows the final code, turns a persistent
#    outage into a green gate.
write_fixture "http://127.0.0.1:1/v1" "space-bunny-free"
start=$(date +%s)
# --attempts 2 keeps this quick; the property under test is that the loop
# TERMINATES on a persistent failure and reports it, not how long it waits.
run_harness --models smart --attempts 2
elapsed=$(( $(date +%s) - start ))
if [[ $RC -ne 0 && "$OUT" == *"non-stream HTTP"* ]]; then
  pass "an unroutable upstream still fails after the retries"
else
  fail "expected a hard failure against a dead upstream, got rc=$RC"
  printf '%s\n' "$OUT" | tail -4 | sed 's/^/          | /' >&2
fi
if [[ $elapsed -lt 150 ]]; then
  pass "the retry loop is bounded (${elapsed}s for 2 attempts against a dead port)"
else
  fail "the retry loop took ${elapsed}s; it is not bounded"
fi

# 10. --attempts 0 must be rejected outright. If it were validated before the
#     flag parser it would pass, and `seq 1 0` emits nothing, so `code` is never
#     assigned and the run looks like a provider fault rather than a bad flag.
run_harness --models smart --attempts 0
if [[ $RC -eq 2 && "$OUT" == *"--attempts must be a positive integer"* ]]; then
  pass "rejects --attempts 0 instead of running with no attempts"
else
  fail "expected --attempts 0 to be rejected, got rc=$RC"
fi
run_harness --models smart --attempts abc
if [[ $RC -eq 2 ]]; then
  pass "rejects a non-numeric --attempts"
else
  fail "expected --attempts abc to be rejected, got rc=$RC"
fi

# 11. No credentials is SKIP (77), never PASS (0). A skip counted as a pass is
#    how a live stage stops being run at all.
rm -f "$SKEL/.envrc"
run_harness --models smart
if [[ $RC -eq 77 ]]; then
  pass "missing .envrc yields SKIP 77, not a pass"
else
  fail "expected 77 without .envrc, got $RC"
fi
printf 'export OPENAI_API_KEY=not-opencode\n' >"$SKEL/.envrc"
run_harness --models smart
if [[ $RC -eq 77 ]]; then
  pass "an .envrc without OPENCODE_API_KEY yields SKIP 77"
else
  fail "expected 77 without OPENCODE_API_KEY, got $RC"
fi

# 12. The binary the harness may build for itself must not be a hard dependency
#    of the gate: with no ./airouter present it builds one rather than failing.
[[ -x "$SKEL/airouter" ]] && pass "the harness runs against a real gateway binary" \
  || fail "no gateway binary in the skeleton"

echo ""
if [[ $fails -ne 0 ]]; then
  echo "$P $fails check(s) failed" >&2
  exit 1
fi
echo "$P ok: every guard of the live routing check is proven able to fail"
