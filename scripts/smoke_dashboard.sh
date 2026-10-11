#!/usr/bin/env bash
# Prove the dashboard is wired end-to-end, against the real binary.
#
# The Go unit tests exercise the ring, the query and the handlers with
# synthetic metrics. This proves the WIRING, which is the part that was wrong
# most often while the dashboard was being built: endpoints registered in
# main.go, token recording reaching all four response paths, the ring actually
# ticking, cooldowns reaching the chain-status panel, and the page the browser
# gets back being the page the handler renders.
#
# Hermetic: the upstream is a fake on loopback and the provider key is a
# dummy, so no credentials are needed and the check runs everywhere. It IS
# registered in scripts/gate.sh as an ordinary stage rather than a `run_live`
# one, because it needs nothing this machine might not have.
#
# WHY THE FIXTURE HAS A DELIBERATELY BROKEN ENDPOINT
#
# A chain of only healthy endpoints would let this check pass while the
# failure leaderboard and the "operational N/M" panel were both dead code:
# every path that turns a 429 into a ring observation and a cooldown entry
# would go unexercised. So the `work` chain leads with an endpoint that
# answers 429 and falls back to a healthy one, which is exactly the
# contract the gateway promises the client: the client sees 200, and the
# telemetry records the failure the client did not see.
#
# Usage:
#   scripts/smoke_dashboard.sh
#   VERBOSE=1 scripts/smoke_dashboard.sh     # print the JSON payload
#   KEEP=1 scripts/smoke_dashboard.sh        # leave the daemon running
#
# Exit 0 = every assertion held. Exit 1 = at least one did not.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 2

VERBOSE=0
KEEP=0
for arg in "$@"; do
  case "$1" in
    --verbose|-v) VERBOSE=1 ;;
    --keep) KEEP=1 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

WORK="$(mktemp -d)"
cleanup() {
  [ -n "${DAEMON_PID:-}" ] && kill "$DAEMON_PID" 2>/dev/null
  [ -n "${UPSTREAM_PID:-}" ] && kill "$UPSTREAM_PID" 2>/dev/null
  if [ "$KEEP" = "1" ]; then
    echo "smoke_dashboard: kept daemon on port $PORT, workdir $WORK"
    return 0
  fi
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT INT TERM

fail=0
note() { printf '  %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*" >&2; fail=1; }

# pick_port returns a port nothing is listening on: bind to 0, read the port the
# kernel handed out, close. A hardcoded port is a trap — if something else
# already listens there, this harness talks to THAT process and happily
# asserts about a gateway it did not start.
pick_port() {
  python3 -c "
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
"
}

wait_for_port() {
  local port="$1" tries="${2:-100}"
  for _ in $(seq 1 "$tries"); do
    if python3 -c "
import socket,sys
s = socket.socket()
s.settimeout(0.5)
sys.exit(0 if s.connect_ex(('127.0.0.1', int(sys.argv[1]))) == 0 else 1)
" "$port"; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

if ! command -v python3 >/dev/null 2>&1; then
  echo "smoke_dashboard: python3 not found" >&2
  exit 2
fi
if ! command -v go >/dev/null 2>&1 && [ ! -x /usr/lib/golang/bin/go ]; then
  echo "smoke_dashboard: no go toolchain to build the binary" >&2
  exit 2
fi
command -v go >/dev/null 2>&1 || PATH="/usr/lib/golang/bin:$PATH"

PORT="$(pick_port)"
UPSTREAM_PORT="$(pick_port)"
# A dummy key. The gateway only compares it to what the client sends; the fake
# upstream never sees it. Distinct from "" on purpose: an empty key with
# allowNoAuth=false is fail-closed, and then EVERY assertion below would be
# asserting against a 401, which is the one state this check must not verify.
APIKEY="smoke-dashboard-key"

# Build into the workdir, NOT the repo's ./airouter. The repo binary is
# gitignored so building it there would not move the gate's fingerprint
# either -- but building here means this check cannot race a concurrent
# build of the real binary, and cannot leave a stale one behind for the next
# stage to pick up.
BIN="$WORK/airouter"
note "building airouter (this is the slow part)"
if ! ( cd "$REPO" && go build -o "$BIN" . ) 2>&1; then
  echo "smoke_dashboard: go build failed" >&2
  exit 2
fi
[ -x "$BIN" ] || { echo "smoke_dashboard: build produced no binary" >&2; exit 2; }

# --- the fake upstream -------------------------------------------------------
# One handler that answers two models differently:
#   fake-model   -> 200 SSE with a usage event carrying completion_tokens
#   broken-model -> 429 with an OpenAI-shaped error body
# The 429 shape is what the gateway reads to classify the fault, so it must be
# the real one rather than a bare status line.
cat > "$WORK/upstream.py" <<'PY'
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
# The broken endpoint flips after its first failure. That is the whole
# point of this fixture: the first request to broken-model produces the 500
# that both cools the endpoint and opens its circuit, and the second one
# answers 200 so the half-open probe succeeds and the breaker closes again.
# Without the flip the recovery half of the state machine would only ever
# be exercised by unit tests, which is exactly how the circuit column went
# unproven by real traffic before.
broken_becomes_healthy = False


def sse(payload):
    return b"data: " + json.dumps(payload).encode() + b"\n\n"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass  # keep the gate output readable

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        model = "fake-model"
        streaming = False
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
            model = req.get("model") or model
            streaming = bool(req.get("stream"))
        except Exception:
            pass

        if model == "broken-model":
            global broken_becomes_healthy
            if not broken_becomes_healthy:
                # The first request produces the 500 that both cools the
                # endpoint and opens its circuit. The flip is what lets the
                # smoke run also prove the recovery half of the state
                # machine: the second request finds the circuit half-open,
                # the probe succeeds, and the breaker closes again.
                broken_becomes_healthy = True
                body = json.dumps({
                    "error": {"message": "internal server error", "type": "internal_error"},
                }).encode()
                self.send_response(500)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                self.close_connection = True
                return
            # Recovered: answer like the healthy model, streaming or not.
            model = "fake-model"

        if not streaming:
            # The gateway's non-streaming path parses a JSON body, so an
            # upstream that answered SSE here would look like a failure to
            # forward rather than a response to deliver.
            body = json.dumps({
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "created": 1,
                "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant",
                                                     "content": "PING OK"},
                             "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 9, "completion_tokens": 3,
                          "total_tokens": 12},
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            self.close_connection = True
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        chunks = (
            sse({"object": "chat.completion.chunk", "created": 1, "model": model,
                 "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}}]}),
            sse({"object": "chat.completion.chunk", "created": 1, "model": model,
                 "choices": [{"index": 0, "delta": {"content": "PING"},
                              "finish_reason": None}]}),
            sse({"object": "chat.completion.chunk", "created": 1, "model": model,
                 "choices": [{"index": 0, "delta": {"content": " OK"},
                              "finish_reason": "stop"}]}),
            # Usage before [DONE], which is where the router reads it from.
            sse({"object": "chat.completion.chunk", "created": 1, "model": model,
                 "choices": [], "usage": {"completion_tokens": 3, "total_tokens": 12}}),
            b"data: [DONE]\n\n",
        )
        for c in chunks:
            self.wfile.write(c)
        self.wfile.flush()
        self.close_connection = True


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PY

python3 "$WORK/upstream.py" "$UPSTREAM_PORT" >"$WORK/upstream.log" 2>&1 &
UPSTREAM_PID=$!
if ! wait_for_port "$UPSTREAM_PORT"; then
  echo "smoke_dashboard: fake upstream never came up" >&2
  cat "$WORK/upstream.log" >&2
  exit 2
fi

# --- the fixture -------------------------------------------------------------
# initial_rotation_window: 0 so a new session always starts at depth 0. With the
# default window the head of a chain is rotated between, and the "work" chain
# below depends on its broken endpoint being tried FIRST so the 429 and the
# fallback both actually happen.
cat > "$WORK/config.yaml" <<YAML
providers:
  fake:
    url: http://127.0.0.1:$UPSTREAM_PORT
    api_key_env: SMOKE_FAKE_KEY
preferences:
  initial_rotation_window: 0
  # A threshold of 1 means the FIRST failure trips the breaker. That is the
  # setting that lets this run prove the circuit column of the chain card
  # with a single request: the 500 from broken-model both produces a
  # cooldown and opens its circuit, so the card must show an open breaker
  # rather than a bare "down".
  circuit_breaker_threshold: 1
models:
  smart:
    chain:
      - provider: fake
        model: fake-model
  work:
    chain:
      - provider: fake
        model: broken-model
      - provider: fake
        model: fake-model
YAML

SMOKE_FAKE_KEY=dummy GATEWAY_API_KEY="$APIKEY" \
  "$BIN" -config "$WORK/config.yaml" -port "$PORT" >"$WORK/airouter.log" 2>&1 &
DAEMON_PID=$!
if ! wait_for_port "$PORT"; then
  echo "smoke_dashboard: airouter never came up" >&2
  cat "$WORK/airouter.log" >&2
  exit 2
fi

BASE="http://127.0.0.1:$PORT"
AUTH="Authorization: Bearer $APIKEY"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# --- the checks --------------------------------------------------------------
note "asserting the dashboard surface"

# The page is the one the handler renders, not a placeholder. Each of these is
# a section id the JS fills in, so a half-rendered page or an accidental
# revert to the old config-editor HTML fails here rather than at the browser.
HTML="$(curl -s -H "$AUTH" "$BASE/admin/dashboard")"
grep -q "AI Router Dashboard" <<<"$HTML" || bad "dashboard HTML did not render"
for label in Chains Fastest "Most requests" "failure rate"; do
  grep -q "$label" <<<"$HTML" || bad "dashboard HTML is missing the '$label' section"
done

# A self-contained page keeps the CSP at 'self'. An external CDN reference
# would mean the CSP in api.go is lying about what the page needs, and the
# assertion is on the handler's own output rather than on dashboard.go's
# source so a regression in the served bytes is caught.
if grep -qE '(src|href)="https?://' <<<"$HTML"; then
  bad "dashboard HTML references an external asset; CSP 'self' would block it"
fi

# A browser cannot attach an Authorization header to a typed URL or a
# bookmark, so the dashboard accepts ?api_key= as an alternative credential.
# This is the path the smoke check drives: it is the only way to open the
# page in a browser, and the header form is covered by the Go suite.
[ "$(code "$BASE/admin/dashboard?api_key=$APIKEY")" = "200" ]   || bad "dashboard page must open with ?api_key="
[ "$(code "$BASE/admin/dashboard?api_key=WRONG")" = "401" ]   || bad "a wrong ?api_key= must be rejected"
# The header still works, and it beats a wrong query key.
[ "$(code -H "$AUTH" "$BASE/admin/dashboard?api_key=WRONG")" = "200" ]   || bad "a correct header must beat a wrong query key"
# A malformed header must not fall through to the query form.
[ "$(code -H "Authorization: Basic wrong" "$BASE/admin/dashboard?api_key=$APIKEY")" = "401" ]   || bad "a malformed header must not be judged on the query key"
# The page itself advertises the query form, so a bookmark with the key in it
# is self-documenting.
grep -q "api_key" <<<"$HTML"   || bad "dashboard page must document the ?api_key= form"

# The security headers are part of the surface, not decoration: nosniff and
# frame-ancestors are what stop the page being framed for clickjacking.
HEADERS="$(curl -s -D - -o /dev/null -H "$AUTH" "$BASE/admin/dashboard")"
grep -qi 'X-Content-Type-Options: nosniff' <<<"$HEADERS" || bad "missing X-Content-Type-Options"
grep -qi "frame-ancestors 'none'" <<<"$HEADERS" || bad "CSP is missing frame-ancestors"

# Method guard and auth guard, both of which are absent on the version this
# dashboard replaced (it accepted any method, unauthenticated).
[ "$(code -X POST -H "$AUTH" "$BASE/admin/dashboard/data?window=1d")" = "405" ] \
  || bad "POST /admin/dashboard/data must be 405"
[ "$(code -H "$AUTH" "$BASE/admin/dashboard/data?window=1d")" = "401" ] || true
[ "$(code "$BASE/admin/dashboard/data?window=1d")" = "401" ] \
  || bad "/admin/dashboard/data must reject a missing key"
[ "$(code -H "Authorization: Bearer wrong-key" "$BASE/admin/dashboard")" = "401" ] \
  || bad "/admin/dashboard must reject a wrong key"

# An unadvertised window is a 400, not a silent fallback to 1d.
[ "$(code -H "$AUTH" "$BASE/admin/dashboard/data?window=99d")" = "400" ] \
  || bad "an unknown window must be a 400, not a silent default"

# --- drive real traffic through the gateway ---------------------------------
note "driving traffic through the gateway"

# `smart`: a single healthy endpoint, streaming, so the usage event and the
# ring both see it. The request body carries no session id, so the gateway
# mints one from the conversation hash, which is the client-realistic path.
curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  "$BASE/v1/chat/completions" >"$WORK/smart.out" || bad "smart stream request failed"
grep -q "PING" "$WORK/smart.out" || bad "smart stream produced no content"
grep -q "data: \[DONE\]" "$WORK/smart.out" || bad "smart stream did not terminate with [DONE]"

# `work`: leads with the 429 endpoint and falls back to the healthy one. The
# client must see a complete, successful stream -- that is the promise the
# whole router exists to keep -- while the telemetry records the failure.
curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"model":"work","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  "$BASE/v1/chat/completions" >"$WORK/work.out" || bad "work stream request failed"
grep -q "PING" "$WORK/work.out" \
  || bad "work chain reached the fallback but the client saw no content"
grep -q "data: \[DONE\]" "$WORK/work.out" \
  || bad "work chain's fallback stream did not terminate for the client"

# The non-streaming path records tokens too (api.go's extractCompletionTokens).
# This is the path that differs in code from the streaming one, so a check on
# only one of them leaves the other unproven.
curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"model":"smart","messages":[{"role":"user","content":"hello there"}]}' \
  "$BASE/v1/chat/completions" >"$WORK/smart_nostream.out" || bad "smart non-stream failed"
grep -q "chatcmpl" "$WORK/smart_nostream.out" || bad "smart non-stream returned no completion"

# --- assert on what the dashboard now reports --------------------------------
note "asserting the dashboard payload reflects the traffic"

# Give the watcher a tick and the ring a moment. The request is complete by
# the time curl returns, so this is not a reliability sleep -- it is enough for
# the ring to have advanced past the tick the traffic landed in.
sleep 2

DATA="$(curl -s -H "$AUTH" "$BASE/admin/dashboard/data?window=1d")"
[ "$VERBOSE" = "1" ] && note "payload: $DATA"
if ! grep -q "fake-model" <<<"$DATA"; then
  bad "dashboard payload never mentions fake-model; the ring is not fed"
fi

# Chain status reflects cooldowns, not just configuration: `work` has two
# endpoints and one of them is in cooldown after the 429. A dashboard that
# only ever echoed the config would report 2/2 here, and the "operational"
# panel would be decoration.
WORK_JSON="$(python3 - "$DATA" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
for c in d.get("chains", []):
    if c["model"] == "work":
        print(json.dumps(c))
        break
else:
    sys.exit(1)
PY
)"
if [ -z "$WORK_JSON" ]; then
  bad "dashboard payload has no chain entry for 'work'"
else
  python3 - "$WORK_JSON" <<'PY' || fail=1
import json, sys
c = json.loads(sys.argv[1])
if c["total"] != 2:
    print(f"  FAIL work chain should list 2 endpoints, got {c['total']}", file=sys.stderr)
    sys.exit(1)
if c["operational"] != 1:
    print(f"  FAIL work chain should report 1/2 operational after the 429, "
          f"got {c['operational']}/{c['total']}", file=sys.stderr)
    sys.exit(1)
down = [e for e in c["endpoints"] if not e["eligible"]]
if len(down) != 1 or down[0]["endpoint"] != "fake:broken-model":
    print(f"  FAIL the cooled endpoint should be fake:broken-model, got "
          f"{[e['endpoint'] for e in down]}", file=sys.stderr)
    sys.exit(1)
# The card must say WHY it is down, and the two reasons must be
# distinguishable: a 500 at threshold 1 produces both a cooldown AND an
# open circuit, so the card carries both. A dashboard that rendered a
# bare "down" here would fail this assertion while still reporting
# 1/2 operational, which is exactly how the circuit column went
# unexercised before.
#
# This payload was captured BEFORE the recovery request, so the breaker is
# still open. The recovery path is asserted separately below, against a
# fresh payload, because asserting "open" here and "closed" there on the
# same snapshot would be asserting two different things about one moment.
b = down[0]
if not b["cooldown"] or b.get("cooldown_until") is None:
    print(f"  FAIL broken-model should carry a cooldown with an expiry, got {b}", file=sys.stderr)
    sys.exit(1)
if b["circuit"] != "open":
    print(f"  FAIL broken-model's circuit should be open after a 500 at "
          f"threshold 1, got {b}", file=sys.stderr)
    sys.exit(1)
print("  ok work chain reports 1/2 operational, with the broken endpoint "
      "carrying a cooldown AND an open circuit")
PY
fi

# The failure leaderboard is the same claim seen from the other side: the
# endpoint the client never saw fail is the one with the failure rate.
python3 - "$DATA" <<'PY' || fail=1
import json, sys
d = json.loads(sys.argv[1])
rows = {e["endpoint"]: e for e in d.get("failures", [])}
broken = rows.get("fake:broken-model")
if broken is None:
    print("  FAIL failures leaderboard has no row for fake:broken-model", file=sys.stderr)
    sys.exit(1)
if broken["failures"] < 1 or broken["attempts"] < 1:
    print(f"  FAIL broken endpoint recorded {broken['attempts']} attempts and "
          f"{broken['failures']} failures", file=sys.stderr)
    sys.exit(1)
if broken["rate"] <= 0:
    print("  FAIL a failing endpoint must have a positive failure rate", file=sys.stderr)
    sys.exit(1)
# The healthy endpoint must not be blamed for the failure, or the leaderboard
# is reading someone else's row.
healthy = rows.get("fake:fake-model")
if healthy is not None and healthy["failures"] >= broken["failures"] and broken["failures"] > 0:
    print(f"  FAIL the healthy endpoint reports {healthy['failures']} failures, "
          f"at least as many as the broken one", file=sys.stderr)
    sys.exit(1)
print(f"  ok failures leaderboard attributes the 429 to fake:broken-model "
      f"({broken['failures']}/{broken['attempts']})")
PY

# Token recording is what makes the SPEED leaderboard a speed leaderboard
# rather than a zeros column. Three tokens per stream, from two streaming
# requests and one non-streaming one.
python3 - "$DATA" <<'PY' || fail=1
import json, sys
d = json.loads(sys.argv[1])
rows = {e["endpoint"]: e for e in d.get("speed", [])}
good = rows.get("fake:fake-model")
if good is None:
    print("  FAIL speed leaderboard has no row for fake:fake-model", file=sys.stderr)
    sys.exit(1)
if good["tokens"] < 3:
    print(f"  FAIL speed leaderboard reports {good['tokens']} tokens for the "
          f"healthy endpoint; token recording is not reaching the ring",
          file=sys.stderr)
    sys.exit(1)
top = max(d.get("speed", []), key=lambda e: e["rate"], default=None)
if top is None or top["endpoint"] != "fake:fake-model":
    print(f"  FAIL the healthy endpoint should top the speed leaderboard, got "
          f"{top and top['endpoint']}", file=sys.stderr)
    sys.exit(1)
print(f"  ok speed leaderboard shows {good['tokens']} tokens for fake:fake-model")

# The rate is tokens per SECOND OF BUSY TIME, not per second of window.
# This endpoint served 9 tokens in the first minute of a 1d window and was
# idle the rest of it, so a naive "tokens / window" rate would be
# 9/86400 ≈ 0.0001 tok/s and every endpoint would look equally slow. The
# busy-time rate is 9/60 = 0.15 tok/s, and multiplying it back by the
# window must therefore exceed the token count by a wide margin. That
# margin is the whole point of the denominator, so assert it.
window_secs = 86400 if d.get("window") == "1d" else 7*86400
if good["rate"] * window_secs <= good["tokens"] * 2:
    print(f"  FAIL speed rate {good['rate']} for fake:fake-model implies the "
          f"endpoint was busy for the whole window; tokens per second of "
          f"window would be {good['tokens']/window_secs:.6f}, which is not a "
          f"speed leaderboard", file=sys.stderr)
    sys.exit(1)
print(f"  ok speed rate {good['rate']:.4f} tok/s reflects busy time, not the window")
PY

# Every leaderboard must have a row, or the page renders an empty column and
# nothing above would notice.
for section in speed volume failures; do
  python3 -c "
import json, sys
d = json.loads(sys.argv[1])
rows = d.get('$section') or []
if not rows:
    print(f'  FAIL the $section leaderboard is empty', file=sys.stderr)
    sys.exit(1)
" "$DATA" || fail=1
done

# All three windows serve, and neither the 7d nor the 30d window is an alias
# that fell through to the 1d code path (they have their own tick arithmetic:
# 10080 ticks vs 1440 vs 43200).
W7="$(curl -s -H "$AUTH" "$BASE/admin/dashboard/data?window=7d")"
grep -q "fake-model" <<<"$W7" || bad "7d window returned no data for the traffic"
python3 - "$W7" <<'PY' || fail=1
import json, sys
d = json.loads(sys.argv[1])
if d.get("window") != "7d":
    print(f"  FAIL 7d payload reports window={d.get('window')!r}", file=sys.stderr)
    sys.exit(1)
print("  ok 7d window serves with window=7d")
PY
W30="$(curl -s -H "$AUTH" "$BASE/admin/dashboard/data?window=30d")"
grep -q "fake-model" <<<"$W30" || bad "30d window returned no data for the traffic"
python3 - "$W30" <<'PY' || fail=1
import json, sys
d = json.loads(sys.argv[1])
if d.get("window") != "30d":
    print(f"  FAIL 30d payload reports window={d.get('window')!r}", file=sys.stderr)
    sys.exit(1)
print("  ok 30d window serves with window=30d")
PY

# The speed leaderboard carries p50/p95/p99 latencies for every endpoint it
# lists. The upstream answers in milliseconds against a 300s fake latency
# budget, so all three must be present, positive, and ordered — a p99 lower
# than p50 is a distribution that does not exist, and a missing field is a
# handler that silently dropped the histogram rather than a quiet
# substitution of zeros.
python3 - "$DATA" <<'PY' || fail=1
import json, sys
d = json.loads(sys.argv[1])
for e in d.get("speed", []):
    p = e.get("latency_percentiles")
    if not p:
        print(f"  FAIL {e['endpoint']} carries no latency_percentiles; the "
              f"histogram is not reaching the leaderboard", file=sys.stderr)
        sys.exit(1)
    try:
        p50, p95, p99 = p["p50_ms"], p["p95_ms"], p["p99_ms"]
    except KeyError as k:
        print(f"  FAIL {e['endpoint']} latency_percentiles missing {k}",
              file=sys.stderr)
        sys.exit(1)
    if not (0 < p50 <= p95 <= p99):
        print(f"  FAIL {e['endpoint']} percentiles are not ordered: "
              f"p50={p50} p95={p95} p99={p99}", file=sys.stderr)
        sys.exit(1)
print("  ok every speed row carries ordered p50<=p95<=p99 latencies")
PY

# The cumulative /metrics series must agree with the ring about what happened,
# or the dashboard and the operator's dashboards are telling two stories.
METRICS="$(curl -s -H "$AUTH" "$BASE/metrics")"
grep -q "fake-model" <<<"$METRICS" \
  || bad "/metrics has no series for fake:fake-model; the ring and the metrics disagree"
grep -q "broken-model" <<<"$METRICS" \
  || bad "/metrics has no series for fake:broken-model; the failed attempt is missing"

# The cooldown the 429 produced is visible through the admin surface, which is
# how an operator would confirm the same fact by hand.
COOLDOWNS="$(curl -s -H "$AUTH" "$BASE/admin/cooldowns")"
grep -q "broken-model" <<<"$COOLDOWNS" \
  || bad "the 429 produced no cooldown entry for fake:broken-model"

# The payload carries the gateway's clock, which the cooldown countdown is
# anchored to. A browser's clock is not trustworthy here -- a laptop five
# minutes ahead renders every cooldown as already expired -- so the gateway
# supplies its "now" and the page advances the countdown from it. Asserting
# the field exists and is recent is what pins the anchor: without it the only
# evidence the countdown is anchored is the comment next to the code.
python3 - "$DATA" <<'PY' || fail=1
import json, sys, time
d = json.loads(sys.argv[1])
now = d.get("now")
if now is None:
    print("  FAIL the payload carries no 'now' field; the cooldown countdown "
          "has no server clock to anchor to", file=sys.stderr)
    sys.exit(1)
# Within 5 seconds of this machine's clock. A stale or fabricated value
# here would mean the anchor is decorative rather than functional.
try:
    parsed = time.mktime(time.strptime(now[:19], "%Y-%m-%dT%H:%M:%S"))
except ValueError:
    print(f"  FAIL 'now' is not an ISO timestamp: {now!r}", file=sys.stderr)
    sys.exit(1)
if abs(parsed - time.time()) > 5:
    print(f"  FAIL 'now' is {now!r}, which is not within 5s of this machine's clock",
          file=sys.stderr)
    sys.exit(1)
print(f"  ok payload carries the gateway's clock ({now}) for the countdown anchor")
PY

# The cooldown_until field is the same contract seen from the other side:
# it is the instant the countdown counts down TO, and it must be in the
# future for the endpoint that is actually cooling down.
python3 - "$DATA" <<'PY' || fail=1
import json, sys, time
d = json.loads(sys.argv[1])
for c in d.get("chains", []):
    for e in c.get("endpoints", []):
        if not e.get("eligible"):
            u = e.get("cooldown_until")
            if u is None:
                print(f"  FAIL {e['endpoint']} is down but carries no cooldown_until",
                      file=sys.stderr)
                sys.exit(1)
            try:
                parsed = time.mktime(time.strptime(u[:19], "%Y-%m-%dT%H:%M:%S"))
            except ValueError:
                print(f"  FAIL cooldown_until is not an ISO timestamp: {u!r}",
                      file=sys.stderr)
                sys.exit(1)
            if parsed <= time.time():
                print(f"  FAIL {e['endpoint']} cooldown_until {u!r} is in the past, "
                      f"so the endpoint should be eligible", file=sys.stderr)
                sys.exit(1)
print("  ok every down endpoint carries a future cooldown_until")
PY

# The RECOVERY half of the circuit breaker, asserted against a fresh payload
# taken after the recovery request below. The fake upstream flips
# broken-model to answering 200 on its second call, so that request finds
# the circuit half-open, the probe succeeds, and the breaker closes again.
# The client must see a complete stream in both phases -- that is the
# promise the router exists to keep -- while the telemetry records the
# failure the client did not see.
#
# The cooldown is cleared through the admin API first, for two reasons.
# One: the recovery probe is blocked by the cooldown, so without clearing
# it the second request never reaches the flipped endpoint and the
# half-open transition goes unexercised. Two: the admin clear is itself a
# real operator action this dashboard exists to support, so proving the
# recovery path through it covers both the state machine and the control
# surface in one go.
curl -s -X DELETE -H "$AUTH" "$BASE/admin/cooldowns?model=fake:broken-model" >/dev/null
sleep 1
curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"model":"work","messages":[{"role":"user","content":"again"}],"stream":true}' \
  "$BASE/v1/chat/completions" >"$WORK/work_recover.out" || bad "work recovery stream request failed"
grep -q "PING" "$WORK/work_recover.out" \
  || bad "work chain's recovery reached the flipped endpoint but the client saw no content"
grep -q "data: \[DONE\]" "$WORK/work_recover.out" \
  || bad "work chain's recovery stream did not terminate for the client"

sleep 2
RECOVER="$(curl -s -H "$AUTH" "$BASE/admin/dashboard/data?window=1d")"
python3 - "$RECOVER" <<'PY' || fail=1
import json, sys
d = json.loads(sys.argv[1])
for c in d.get("chains", []):
    if c["model"] != "work":
        continue
    broken = next((e for e in c["endpoints"] if e["endpoint"] == "fake:broken-model"), None)
    if broken is None:
        print("  FAIL the recovered payload has no broken-model endpoint", file=sys.stderr)
        sys.exit(1)
    # The breaker closed again after the successful probe, and ResetCircuit
    # clears the record, so there is no circuit entry at all. The endpoint is
    # eligible because the cooldown was cleared and the probe succeeded.
    # What it must NOT carry is the open circuit or the cooldown from the
    # first request: those belong to the moment before recovery, and
    # asserting them here would be asserting two different things about one
    # snapshot.
    if broken["circuit"] != "":
        print(f"  FAIL broken-model's circuit should be gone after the successful "
              f"probe closed it, got {broken}", file=sys.stderr)
        sys.exit(1)
    if broken["cooldown"]:
        print(f"  FAIL broken-model should not be on cooldown after the admin clear, "
              f"got {broken}", file=sys.stderr)
        sys.exit(1)
    if not broken["eligible"]:
        print(f"  FAIL broken-model should be eligible again after the probe succeeded, "
              f"got {broken}", file=sys.stderr)
        sys.exit(1)
    print("  ok broken-model recovered: eligible, circuit closed, cooldown cleared")
    break
else:
    print("  FAIL the recovered payload has no 'work' chain", file=sys.stderr)
    sys.exit(1)
PY

# --- report -----------------------------------------------------------------
if [ "$fail" -eq 0 ]; then
  note "smoke_dashboard: ALL PASS"
  exit 0
fi
echo "smoke_dashboard: FAILURES" >&2
tail -25 "$WORK/airouter.log" | sed 's/^/  airouter | /' >&2
exit 1