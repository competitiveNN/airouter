#!/usr/bin/env bash
# Assert the gateway really routes to OpenCode, end to end and live.
#
# Runs airouter against testdata/opencode-config.yaml, whose four logical chains
# contain nothing but an OpenCode endpoint. Any successful completion therefore
# proves the OpenCode path works: chain selection, the synthesized
# client-attribution headers (without them the free tier answers 403
# FreeTierError), sticky session pinning, and streaming.
#
# This needs real credentials and network access, so it is NOT part of
# scripts/gate.sh. It is the live complement to the hermetic fake-upstream CI
# job in testdata/ci-config.yaml.
#
# Usage:
#   scripts/opencode-routing-check.sh                 # all four profiles
#   scripts/opencode-routing-check.sh --models work    # subset
#   scripts/opencode-routing-check.sh --verbose        # print response bodies
#   scripts/opencode-routing-check.sh --keep           # leave the daemon running
#   scripts/opencode-routing-check.sh --fixture PATH   # use another fixture (mutation testing)
#   scripts/opencode-routing-check.sh --expect-model M # assert a different model (mutation
#                                                      # testing only; the gate never passes this)
#   scripts/opencode-routing-check.sh --attempts N     # attempts per request (testing only)
#
# Exit 0 = every profile routed to OpenCode.
# Exit 1 = at least one profile failed.
# Exit 77 = this machine has no OpenCode credentials, so the live check could
#           not run. Distinct from 0 on purpose: scripts/gate.sh reports it as
#           SKIP and does not count it as a pass.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/airouter"
FIXTURE="$REPO_DIR/testdata/opencode-config.yaml"
# Empty means "ask the OS for a free port" (see pick_port below). A hardcoded
# default is a trap: if something else already listens there, this harness talks
# to THAT process and happily asserts about a gateway it did not start.
PORT=""
MODELS=(smart work fast large)
# Attempts per profile. >1 because opencode.ai is a free tier and is sometimes
# slower than the gateway's per-request budget; 1 would make this stage a
# network flake detector. See the retry loop below for what is and is not retried.
ATTEMPTS=3
RETRY_SLEEP=3
# How many retries one run may absorb before it is treated as a provider
# problem rather than a blip. A retry is cheap; a run that needed several means
# the upstream is degrading, and reporting that as plain green is how a provider
# dies unnoticed. Overridable for experiments; the gate never passes it.
MAX_RETRIES=2
# Retry history goes OUTSIDE the repository on purpose. scripts/gate.sh
# fingerprints `git status --porcelain -uall` at the start of a run and compares
# it at the end; a log file created inside the tree would show up as an untracked
# path, change the fingerprint, and turn every gate run into INVALID RUN (exit
# 3). There is no logs/ entry in .gitignore for the same reason.
ROUTING_LOG="${OPENCODE_ROUTING_LOG:-${TMPDIR:-/tmp}/airouter-opencode-routing.log}"
# Mutation affordance ONLY: forces the model the /admin/sessions assertion
# compares against, so that assertion can be proved able to fail without having
# to stand up a misrouting gateway. Never used by scripts/gate.sh.
# Declared here because the flag parser below assigns it under `set -u`.
EXPECT_MODEL_OVERRIDE=""
VERBOSE=0
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --models) shift; IFS=, read -r -a MODELS <<< "$1" ;;
    --port) shift; PORT="$1" ;;
    --attempts) shift; ATTEMPTS="$1" ;;
    --max-retries) shift; MAX_RETRIES="$1" ;;
    --fixture) shift; FIXTURE="$1" ;;
    --expect-model) shift; EXPECT_MODEL_OVERRIDE="$1" ;;
    --verbose|-v) VERBOSE=1 ;;
    --keep) KEEP=1 ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

# Validated AFTER the parser, not before: --attempts is assigned there, and a
# check that runs before the assignment validates nothing. `--attempts 0` would
# otherwise make `seq 1 0` emit nothing, leaving `code` unset and, under the
# `[[ "$code" != 200 ]]` test, silently "not 200" — a fail that looks like a
# provider fault instead of a bad flag.
[[ "$ATTEMPTS" =~ ^[1-9][0-9]*$ ]] || { echo "--attempts must be a positive integer, got '$ATTEMPTS'" >&2; exit 2; }
[[ "$MAX_RETRIES" =~ ^[0-9]+$ ]] || { echo "--max-retries must be a non-negative integer, got '$MAX_RETRIES'" >&2; exit 2; }

fail=0
note() { printf '  %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*" >&2; fail=1; }

# Exit 77 = "this machine cannot run a live check", NOT "the check passed".
# scripts/gate.sh counts it as SKIP and says so out loud, because a stage that
# quietly degrades to a pass is how a check stops being run at all. Only the
# absence of credentials may produce it: once OPENCODE_API_KEY is present, a
# failure IS a routing failure and exits 1.
skip() { echo "SKIP: $*" >&2; exit 77; }

# pick_port returns a port nothing is listening on: bind to 0, read the port the
# kernel handed out, close, and use it. There is an unavoidable race between the
# close and the daemon's bind, which is why the pre-flight check below exists.
pick_port() {
  python3 -c "
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
"
}

# port_in_use reports 0 when a TCP connect to the port succeeds.
port_in_use() {
  python3 -c "
import socket, sys
s = socket.socket()
s.settimeout(1)
sys.exit(0 if s.connect_ex(('127.0.0.1', int(sys.argv[1]))) == 0 else 1)
" "$1"
}

# The binary is a gitignored build artifact, so building it here cannot move the
# gate's tree fingerprint. `go build ./...` does not produce it.
if [[ ! -x "$BIN" ]]; then
  command -v go >/dev/null 2>&1 || skip "no go toolchain to build $BIN"
  echo "building $BIN ..."
  ( cd "$REPO_DIR" && go build -o "$BIN" . ) || { echo "go build failed" >&2; exit 1; }
fi
[[ -x "$BIN" ]] || { echo "no airouter binary at $BIN" >&2; exit 1; }
[[ -r "$FIXTURE" ]] || { echo "missing fixture $FIXTURE" >&2; exit 1; }
[[ -n "$PORT" ]] || PORT="$(pick_port)"
[[ -r "$REPO_DIR/.envrc" ]] || skip "missing .envrc (source of the provider keys)"

set -a
# shellcheck disable=SC1091
. "$REPO_DIR/.envrc"
set +a
[[ -n "${OPENCODE_API_KEY:-}" ]] || skip "OPENCODE_API_KEY unset"

# WHAT IS ASSERTED, AND WHY IT IS SPLIT LIKE THIS
#
# The provider is pinned to "opencode" in this script, on purpose. This check
# exists to prove the gateway routes to OpenCode; an expectation read out of the
# fixture would let the fixture decide what "correct" means, and a fixture
# repointed at another provider would then pass by asserting its own
# replacement. That is not a hypothetical: deriving the provider from the
# fixture was tried first, and pointing the fixture at kilocode made this
# harness report OK. So the provider is a constant, and the fixture is checked
# against it below.
#
# The MODEL is the one thing derived from the fixture. Which opencode model this
# key is entitled to is provider-side state that changes without a code change
# (today: space-bunny-free answers and every other free model 403s), so pinning
# the model here would mean editing the harness every time opencode grants this
# key something new. The fixture is the single place that fact is recorded, and
# it is asserted against /admin/sessions below, so it cannot drift silently.
#
# All of this runs BEFORE the daemon starts. A fixture that names another
# provider, mixes models, or names no endpoint is a mistake in the fixture;
# reporting that as "daemon died on startup" would send whoever has to fix it
# looking at the binary instead.
EXPECTED_PROVIDER="opencode"
# Two values on one line, split on a character that is NOT in IFS. Do not use
# `read -r A B`: with a mixed-model fixture the model field is empty, and bash
# strips the leading whitespace instead of producing an empty first field, so
# the provider lands in the model variable and both checks read inverted. That
# bug shipped once already; scripts/opencode-routing-selftest.sh pins it.
FIXTURE_FACTS="$(python3 -c "
import re, sys
src = open(sys.argv[1]).read()
pairs = re.findall(r'^\s+- provider: (\S+)\n\s+model: (\S+)', src, re.M)
if not pairs:
    print('|')
    sys.exit(0)
models = sorted({m for _, m in pairs})
providers = sorted({p for p, _ in pairs})
print((models[0] if len(models) == 1 else '') + '|' + ','.join(providers))
" "$FIXTURE")"
EXPECTED_MODEL="${FIXTURE_FACTS%%|*}"
FIXTURE_PROVIDERS="${FIXTURE_FACTS#*|}"

if [[ -z "$EXPECTED_MODEL" ]]; then
  echo "could not read exactly one model out of $FIXTURE" >&2
  echo "  the fixture names no provider/model pair, or mixes several models;" >&2
  echo "  this check asserts one provider and one model for all four profiles." >&2
  exit 1
fi
if [[ "$FIXTURE_PROVIDERS" != "$EXPECTED_PROVIDER" ]]; then
  echo "$FIXTURE routes to [$FIXTURE_PROVIDERS], not to $EXPECTED_PROVIDER" >&2
  echo "  this check exists to prove routing to opencode; pointing it at another" >&2
  echo "  provider makes it assert its own fixture and report OK, which is not a test." >&2
  exit 1
fi
echo "expecting every profile to route to $EXPECTED_PROVIDER/$EXPECTED_MODEL"
echo "  (provider pinned in this script; model read from the fixture)"
if [[ -n "$EXPECT_MODEL_OVERRIDE" ]]; then
  echo "  MUTATION MODE: --expect-model $EXPECT_MODEL_OVERRIDE overrides the model"
  EXPECTED_MODEL="$EXPECT_MODEL_OVERRIDE"
fi
echo ""

# cooldowns.json is written next to the config file, so the fixture is copied
# into a temp dir: running against testdata/ would drop a state file into the repo.
WORK="$(mktemp -d)"
cp "$FIXTURE" "$WORK/config.yaml"
KEY="opencode-check-$$"
LOG="$WORK/airouter.log"

# Refuse to run against a port that is already taken. The daemon would fail to
# bind, the health probe would be answered by whatever is already listening, and
# every assertion below would be about someone else's process. Checking the
# daemon we started is still alive at the same time (the health loop below) does
# not help: /health is served by any airouter on that port.
if port_in_use "$PORT"; then
  echo "port $PORT is already in use; refusing to test a gateway we did not start" >&2
  echo "  pass --port with a free port, or stop whatever is listening there" >&2
  exit 1
fi

"$BIN" -config "$WORK/config.yaml" -port "$PORT" -api-key "$KEY" >"$LOG" 2>&1 &
PID=$!

cleanup() {
  if [[ $KEEP -eq 1 ]]; then
    note "daemon left running on port $PORT (pid $PID), log $LOG"
    return
  fi
  kill "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

for _ in $(seq 1 50); do
  if curl -fsS -m 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then break; fi
  kill -0 "$PID" 2>/dev/null || { echo "daemon died on startup:" >&2; cat "$LOG" >&2; exit 1; }
  sleep 0.2
done
curl -fsS -m 5 "http://127.0.0.1:$PORT/health" >/dev/null \
  || { echo "gateway never became healthy" >&2; cat "$LOG" >&2; exit 1; }

# post_with_retry <label> <outfile> <json-body> -> echoes the final HTTP code
#
# Bounded retries, because a real provider on a real network is occasionally
# slower than the gateway's per-request budget. Observed live on 2026-09-30: a
# `context deadline exceeded` on a 5s/~17-token budget, which the router then
# recorded as a 30s cooldown. That is not a routing fault, and a gate stage that
# goes red on it is a stage people learn to re-run and ignore.
#
# What is retried is an UPSTREAM/TRANSPORT failure OR a 200 that carries no
# usable content, never a routing verdict. A 503 `all_models_unavailable`
# produced by a 403 FreeTierError is deterministic: it survives all $ATTEMPTS
# attempts and still fails the run. Each retry first clears the cooldown the
# previous attempt armed (DELETE /admin/cooldowns?model=provider:model — the
# operator endpoint for exactly this), otherwise the next attempt would be
# turned away by the backoff instead of actually retrying upstream.
#
# Every retry is printed. A green run that needed one is still green, but it is
# not silently green, so a provider degrading over time is visible in the output.
post_with_retry() {
  # Retry notes go to STDERR, not through note(). This function's stdout is
  # captured by `$(...)` to obtain the status code, and note() prints to stdout,
  # so a single retry line used to arrive as part of the code: `code` became
  # "  non-stream: attempt 1/3 ...\n503", every `== 200` test failed, and the
  # reported status was that whole blob. Anything printed from inside a
  # command substitution has to go to stderr.
  rnote() { printf '  %s\n' "$*" >&2; }
  local label="$1" outfile="$2" payload="$3" attempt code unusable
  for attempt in $(seq 1 "$ATTEMPTS"); do
    if [[ "$label" == "stream" ]]; then
      code=$(curl -sS -N -m 120 -o "$outfile" -w '%{http_code}' \
        "http://127.0.0.1:$PORT/v1/chat/completions" \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "$payload" || echo 000)
    else
      code=$(curl -sS -m 120 -o "$outfile" -w '%{http_code}' \
        "http://127.0.0.1:$PORT/v1/chat/completions" \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "$payload" || echo 000)
    fi

    # A 200 is not automatically a usable answer. Observed live on 2026-09-30:
    # one profile in four came back HTTP 200 with no content at all, with no
    # cooldown and no timeout recorded anywhere -- an upstream artifact, not a
    # routing fault and not something the transport-level retry could see. So
    # the retry predicate is "200 AND usable", not "200".
    unusable=""
    if [[ "$code" == 200 ]]; then
      if [[ "$label" == "stream" ]]; then
        [[ $(grep -c '^data: \[DONE\]' "$outfile" || true) -eq 1 ]] || unusable="no single [DONE]"
        [[ $(grep -c '^data: ' "$outfile" || true) -ge 2 ]] || unusable="no content chunks"
      else
        unusable=$(python3 -c "
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    c = d['choices'][0]['message'].get('content') or ''
    print('' if c.strip() else 'no content in choices[0].message')
except Exception as exc:
    print('unparseable: %s' % exc)
" "$outfile")
      fi
      if [[ -z "$unusable" ]]; then
        [[ $attempt -gt 1 ]] && rnote "$label: attempt $attempt/$ATTEMPTS succeeded after a transient failure"
        echo "$code"
        return 0
      fi
    fi

    if [[ $attempt -lt $ATTEMPTS ]]; then
      # Recorded in a FILE, not a shell variable: post_with_retry is invoked as
      # `code=$(post_with_retry ...)`, so it runs in a subshell and any variable
      # it increments is discarded when the substitution ends. This is the same
      # subshell boundary that already bit this script once, when retry notes on
      # stdout were captured as the HTTP status.
      printf '%s\t%s\t%s\n' "$label" "$code" "${unusable:-transport}" >>"$WORK/retries"
      rnote "$label: attempt $attempt/$ATTEMPTS unusable (HTTP $code${unusable:+, $unusable}); clearing cooldown, retrying"
      curl -fsS -m 10 -X DELETE \
        "http://127.0.0.1:$PORT/admin/cooldowns?model=$EXPECTED_PROVIDER:$EXPECTED_MODEL" \
        -H "Authorization: Bearer $KEY" >/dev/null 2>&1 || true
      sleep "$RETRY_SLEEP"
    fi
  done
  echo "$code"
}

# The router derives the session id from a hash of model+messages, so a distinct
# prompt per profile yields a distinct session to inspect on /admin/sessions.
i=0
# Sessions matched correctly by the previous profile. Each profile must add at
# least one; see the per-profile assertion below.
SESSIONS_SEEN=0
for model in "${MODELS[@]}"; do
  i=$((i + 1))
  prompt="Reply with exactly one word: pong-$i"
  echo "== $model"

  # Non-stream. Deliberately sent WITHOUT the gate's stream+tools pair, because
  # this fixture's model is entitled regardless of body shape and this case is
  # about the gateway's response contract, not the free tier.
  #
  # Known limitation, verified live 2026-10-01: a gate-DEPENDENT model (say
  # mimo-v2.5-free) rejects this body with 403, because the gateway requires
  # stream=true. airouter forwards the client's body verbatim and does not
  # re-stream an upstream SSE into a single JSON completion, so a non-streaming
  # caller falls through to the next endpoint in the chain instead. That is the
  # fallback chain doing its job, not a routing fault, but it does mean
  # gate-dependent models currently serve streaming agent clients only.
  body=$(python3 - "$model" "$prompt" <<'PY'
import json, sys
print(json.dumps({
    "model": sys.argv[1],
    "messages": [{"role": "user", "content": sys.argv[2]}],
    "max_tokens": 256,
}))
PY
)

  resp_file="$WORK/resp-$model.json"
  code=$(post_with_retry "non-stream" "$resp_file" "$body")

  if [[ "$code" != 200 ]]; then
    bad "$model: non-stream HTTP $code: $(head -c 300 "$resp_file")"
    continue
  fi

  read -r content <<<"$(python3 - "$resp_file" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print((d["choices"][0]["message"].get("content") or "").replace("\n", " ")[:80])
except Exception as exc:
    print(f"<unparseable: {exc}>")
PY
)"
  if [[ -z "$content" || "$content" == \<unparseable* ]]; then
    # Report the body, bounded. An earlier version printed only '' and the
    # failure was undiagnosable without --verbose; the whole point of catching
    # it here is that the next occurrence can be read.
    bad "$model: empty/unparseable content: '$content'; body: $(head -c 300 "$resp_file" | tr -d '\n')"
  else
    note "non-stream ok: '$content'"
  fi
  [[ $VERBOSE -eq 1 ]] && note "body: $(head -c 400 "$resp_file")"

  # The completion proves OpenCode answered; /admin/sessions proves the gateway
  # actually chose OpenCode rather than answering from somewhere else.
  #
  # The assertion is per-profile and positive: THIS request must have created a
  # NEW session, on the fixture's provider and model. Checking only "at least
  # one session somewhere matches" would let the first profile's session vouch
  # for the other three, so a profile that never reached the router at all would
  # still read as routed. Growth since the previous profile is what makes each
  # line mean something.
  sessions=$(curl -fsS -m 10 "http://127.0.0.1:$PORT/admin/sessions" \
    -H "Authorization: Bearer $KEY" || echo '{"sessions":[]}')
  echo "$sessions" >"$WORK/sessions.json"
  read -r oc_count foreign <<<"$(python3 - "$WORK/sessions.json" "$EXPECTED_PROVIDER" "$EXPECTED_MODEL" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
provider, model = sys.argv[2], sys.argv[3]
s = d.get("sessions", [])
# A session counts as correctly routed only when BOTH the provider and the model
# match the fixture. Counting on the provider alone would pass for a session
# pinned to some other opencode model, which is not what was asked for.
oc = sum(1 for e in s
         if e.get("provider", "") == provider and e.get("model", "") == model)
print(oc, len(s) - oc)
PY
)"
  if [[ "$foreign" -ne 0 ]]; then
    bad "$model: $foreign session(s) not pinned to $EXPECTED_PROVIDER/$EXPECTED_MODEL: $sessions"
  elif [[ "$oc_count" -le "$SESSIONS_SEEN" ]]; then
    # No new session appeared, so this profile's completion was not routed
    # through the endpoint map at all and the match above is inherited.
    bad "$model: no new session for this request (still $oc_count, was $SESSIONS_SEEN);" \
        "routing for this profile is unproven: $sessions"
  else
    note "routing ok: this request added a session on $EXPECTED_PROVIDER/$EXPECTED_MODEL" \
         "(now $oc_count, was $SESSIONS_SEEN; none foreign)"
  fi
  SESSIONS_SEEN="$oc_count"

  # Streaming: the OpenCode path has to hold mid-stream too, not just on the
  # first byte. One [DONE] and non-empty content is the contract.
  sse_file="$WORK/sse-$model.txt"
  # The tool pair is sent even though this fixture's model does not need it.
  # OpenCode's free-tier gate requires stream=true AND a tools array holding
  # both `bash` and `read`; a body without them is answered 403 regardless of
  # the headers. Sending them keeps this check valid if the fixture is ever
  # repointed at a gate-dependent model.
  scode=$(post_with_retry "stream" "$sse_file" "$(python3 - "$model" "$prompt stream" <<'PY'
import json, sys


def tool(name, props, required):
    return {"type": "function",
            "function": {"name": name, "description": f"probe {name} tool",
                         "parameters": {"type": "object", "properties": props,
                                        "required": required,
                                        "additionalProperties": False}}}


print(json.dumps({
    "model": sys.argv[1],
    "messages": [{"role": "user", "content": sys.argv[2]}],
    "max_tokens": 256,
    "stream": True,
    "tools": [
        tool("bash", {"command": {"type": "string"}}, ["command"]),
        tool("read", {"filePath": {"type": "string"}}, ["filePath"]),
    ],
}))
PY
)")
  if [[ "$scode" != 200 ]]; then
    bad "$model: stream HTTP $scode"
  else
    done_count=$(grep -c '^data: \[DONE\]' "$sse_file" || true)
    chunks=$(grep -c '^data: ' "$sse_file" || true)
    if [[ "$done_count" -ne 1 ]]; then
      bad "$model: stream emitted $done_count [DONE] sentinels (want 1)"
    elif [[ "$chunks" -lt 2 ]]; then
      bad "$model: stream had $chunks data events (no content)"
    else
      note "stream ok: $chunks data events, 1 [DONE]"
    fi
    [[ $VERBOSE -eq 1 ]] && note "sse: $(head -c 400 "$sse_file")"
  fi
done

# Retry accounting, and the line that keeps "green" honest.
#
# The retry loop exists so a single slow response from a free tier does not fail
# the gate. That is only safe while retries stay rare. A run that absorbed
# several is not evidence that routing works -- it is evidence that the upstream
# is unwell and the retries are hiding it. So the count is surfaced, persisted
# outside the tree, and bounded: past the threshold the run goes red.
RETRIES=0
EMPTY_200=0
if [[ -f "$WORK/retries" ]]; then
  RETRIES=$(grep -c . "$WORK/retries" || true)
  EMPTY_200=$(grep -c 'no content' "$WORK/retries" || true)
fi
: "${RETRIES:=0}"; : "${EMPTY_200:=0}"

if [[ $RETRIES -gt $MAX_RETRIES ]]; then
  fail=1
  echo "opencode routing check: $RETRIES retries absorbed (limit $MAX_RETRIES)," >&2
  echo "  $EMPTY_200 of them HTTP 200 with no content. Routing may still be fine;" >&2
  echo "  the upstream is not, and this run is reported as failed rather than" >&2
  echo "  green. Recent attempts:" >&2
  tail -5 "$WORK/retries" 2>/dev/null | sed 's/^/    /' >&2
fi

# Appended AFTER the threshold, so the recorded verdict is the verdict. Writing
# it first recorded fail=0 for runs that went on to exit 1 -- a history whose
# only purpose is to explain why the gate went red, disagreeing with the gate.
STAMP=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
{
  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$STAMP" "$EXPECTED_PROVIDER/$EXPECTED_MODEL" "$fail" "$RETRIES" "$EMPTY_200"
} >>"$ROUTING_LOG" 2>/dev/null || true
echo "retries=$RETRIES empty200=$EMPTY_200 limit=$MAX_RETRIES log=$ROUTING_LOG"

if [[ $fail -ne 0 ]]; then
  echo "opencode routing check: FAILED" >&2
  echo "--- daemon log ---" >&2
  cat "$LOG" >&2
  exit 1
fi
echo "opencode routing check: OK (${MODELS[*]} -> $EXPECTED_PROVIDER/$EXPECTED_MODEL)"