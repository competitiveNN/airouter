#!/usr/bin/env bash
# Prove the chat page's benchmark survives a reload: the run lands in the chat
# history, the history renders it back, and it is NOT sent to the model as
# context on the next turn.
#
# The Go unit tests pin strings in the chatHTML constant. A pin cannot see the
# class of bug that actually shipped: every pinned line present and correct
# while a ReferenceError or a missing state transition leaves the benchmark
# invisible after a reload. A grep has no way to distinguish "the line exists"
# from "the line runs". So this check runs the page's own JavaScript in node,
# against the real gateway, exactly the way a browser does.
#
# WHY THE BENCHMARK RUNS AGAINST THE REAL GATEWAY
#
# The page's benchmark measures whatever the gateway reports in the
# `airouter.stats` SSE event. Mocking that event by hand would test the page
# against a payload the gateway never sends -- and the shape of that event is
# precisely what the benchmark's numbers depend on. So the harness posts to the
# real daemon, which proxies to the fake upstream on loopback and appends the
# real stats event. Hermetic: nothing outside loopback is contacted.
#
# Usage:
#   scripts/smoke_chat.sh
#   VERBOSE=1 scripts/smoke_chat.sh     # print the harness's own assertion log
#   KEEP=1 scripts/smoke_chat.sh        # leave the daemon running
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
    echo "smoke_chat: kept daemon on port $PORT, workdir $WORK"
    return 0
  fi
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT INT TERM

note() { printf '  %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*" >&2; fail=1; }
fail=0

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

for tool in python3 node go; do
  if ! command -v "$tool" >/dev/null 2>&1 && [ ! -x "/usr/lib/golang/bin/$tool" ]; then
    echo "smoke_chat: $tool not found" >&2
    exit 2
  fi
done
command -v go >/dev/null 2>&1 || PATH="/usr/lib/golang/bin:$PATH"

PORT="$(pick_port)"
UPSTREAM_PORT="$(pick_port)"
# A dummy key the gateway compares against what the client sends. The fake
# upstream never sees it. Distinct from "" on purpose: an empty key with
# allowNoAuth=false is fail-closed and every request would 401, which is the
# one state this check must not verify.
APIKEY="smoke-chat-key"

# Build into the workdir, NOT the repo's ./airouter, so this check can neither
# race a concurrent build of the real binary nor leave a stale one behind.
BIN="$WORK/airouter"
note "building airouter (this is the slow part)"
if ! ( cd "$REPO" && go build -o "$BIN" . ) 2>&1; then
  echo "smoke_chat: go build failed" >&2
  exit 2
fi
[ -x "$BIN" ] || { echo "smoke_chat: build produced no binary" >&2; exit 2; }

# --- the fake upstream -------------------------------------------------------
# One model that answers a known-good SSE stream ending in a usage event, so
# the gateway has something to measure and report as chatStats.
python3 "$REPO/scripts/fake-sse-upstream.py" --port "$UPSTREAM_PORT" >"$WORK/upstream.log" 2>&1 &
UPSTREAM_PID=$!
if ! wait_for_port "$UPSTREAM_PORT"; then
  echo "smoke_chat: fake upstream never came up" >&2
  cat "$WORK/upstream.log" >&2
  exit 2
fi

# --- the fixture -------------------------------------------------------------
# All four chains lead with one healthy endpoint, so every model the chat page
# can select is served and a benchmark can run against any of them.
cat > "$WORK/config.yaml" <<YAML
providers:
  fake:
    url: http://127.0.0.1:$UPSTREAM_PORT
    api_key_env: SMOKE_FAKE_KEY
preferences:
  initial_rotation_window: 0
models:
  smart:
    chain:
      - provider: fake
        model: fake-model
  work:
    chain:
      - provider: fake
        model: fake-model
  fast:
    chain:
      - provider: fake
        model: fake-model
  large:
    chain:
      - provider: fake
        model: fake-model
YAML

SMOKE_FAKE_KEY=dummy GATEWAY_API_KEY="$APIKEY" \
  "$BIN" -config "$WORK/config.yaml" -port "$PORT" >"$WORK/airouter.log" 2>&1 &
DAEMON_PID=$!
if ! wait_for_port "$PORT"; then
  echo "smoke_chat: airouter never came up" >&2
  cat "$WORK/airouter.log" >&2
  exit 2
fi

BASE="http://127.0.0.1:$PORT"
AUTH="Authorization: Bearer $APIKEY"

# --- fetch the page the handler actually renders ------------------------------
CHAT_HTML="$(curl -s -H "$AUTH" "$BASE/chat")"
printf '%s' "$CHAT_HTML" > "$WORK/chat.html"
grep -q "Benchmark" <<<"$CHAT_HTML" || bad "the chat page rendered no benchmark control"

# --- run the page's own JavaScript --------------------------------------------
cat > "$WORK/chat_harness.js" <<'NODE'
// Execute the chat page's script the way a browser does: a mocked DOM, a
// localStorage, and a fetch that resolves the page's relative URLs against
// the daemon the page came from. Drives a real benchmark run against the
// real gateway and asserts the persistence contract, which no grep on the
// HTML can prove: a benchmark that runs fine but is never written to the
// chat history is invisible to every string-pinning test.
//
// The page's script calls init() at load, so the harness's DOM must survive
// the whole bootstrap, not just the render path under test.
const fs = require('fs');
const vm = require('vm');
const [pagePath, baseUrl] = process.argv.slice(2);
const html = fs.readFileSync(pagePath, 'utf8');
const m = html.match(/<script>([\s\S]*)<\/script>/);
if (!m) { console.error('no <script> in page'); process.exit(1); }
const script = m[1];

const realFetch = global.fetch;
const store = {};

function mkEl(tag) {
  const e = {
    tagName: (tag || '').toUpperCase(),
    children: [],
    className: '',
    style: {},
    dataset: {},
    // The chat page's select elements read .value back after renderModels()
    // fills them; a browser keeps the last selected value.
    value: '',
    // A row buffer is what a browser's HTMLTableRowElement looks like from
    // the page: cells are numbered, and insertCell(-1) appends.
    cells: [],
    scrollTop: 0,
    scrollHeight: 0,
    focus() {},
    addEventListener() {},
    appendChild(c) { this.children.push(c); return c; },
    insertRow() { const r = mkEl('tr'); this.children.push(r); return r; },
    insertCell() { const c = mkEl('td'); this.cells.push(c); this.children.push(c); return c; },
    deleteCell(i) { this.cells.splice(i, 1); },
    querySelector(sel) {
      const want = sel.replace(/^\./, '').toLowerCase();
      if ((this.className || '').toLowerCase().split(/\s+/).includes(want)) return this;
      for (const c of this.children) {
        if (c.querySelector) { const r = c.querySelector(sel); if (r) return r; }
      }
      return null;
    },
    querySelectorAll(sel) {
      const want = sel.replace(/^\./, '').toLowerCase();
      const out = [];
      const walk = n => {
        if ((n.className || '').toLowerCase().split(/\s+/).includes(want)) out.push(n);
        (n.children || []).forEach(walk);
      };
      walk(this);
      return out;
    },
  };
  let own = '';
  Object.defineProperty(e, 'textContent', {
    get() { return own + this.children.map(c => c.textContent).join(''); },
    set(v) { own = v; this.children = []; },
  });
  let inner = '';
  Object.defineProperty(e, 'innerHTML', {
    get() { return inner; },
    set(v) { inner = v; if (v === '') { e.children = []; e.cells = []; } },
  });
  return e;
}

// A browser's <select> starts on its first option, and the page reads the
// protocol straight off that element: a select the mock left empty would
// make the page post to "/chat/" and get a 404, which is a harness bug, not
// a product bug. Seed the mock from the page's own markup so the harness
// posts to the endpoint the browser would.
const selectDefaults = {};
for (const m of html.matchAll(/<select\b[^>]*\bid="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)) {
  const om = /<option\b[^>]*\bvalue="([^"]*)"/.exec(m[2]);
  if (om) selectDefaults[m[1]] = om[1];
}

global.document = {
  getElementById(id) {
    if (!store[id]) {
      store[id] = mkEl('div');
      if (selectDefaults[id] !== undefined) store[id].value = selectDefaults[id];
    }
    return store[id];
  },
  createElement(tag) { return mkEl(tag); },
  querySelectorAll() { return []; },
};
global.URLSearchParams = class {
  constructor(s) { this.s = s || ''; }
  get(k) {
    const mm = new RegExp('[?&]' + k + '=([^&]*)').exec(this.s);
    return mm ? decodeURIComponent(mm[1]) : null;
  }
};
const location_ = { search: '?api_key=smoke-chat-key', origin: baseUrl };
global.location = location_;
// The browser's window is a non-configurable property of the global object,
// and the page reaches its origin through window.location. Define it the same
// way so a top-level `let window` in the page throws here as it does there.
Object.defineProperty(globalThis, 'window', {
  value: { location: location_ },
  configurable: false,
  writable: false,
});
global.alert = (msg) => { throw new Error('the page alerted: ' + msg); };
global.confirm = () => true;

// A browser's localStorage is a per-origin string map. An in-memory object
// is enough: the page only ever round-trips through it, and the harness
// reads back through the same map.
const lsData = {};
global.localStorage = {
  getItem(k) { return Object.prototype.hasOwnProperty.call(lsData, k) ? lsData[k] : null; },
  setItem(k, v) { lsData[k] = String(v); },
  removeItem(k) { delete lsData[k]; },
};

// The page builds relative URLs ("/chat/completions?api_key=..."); a browser
// resolves them against the origin it was served from. The harness must do
// the same or the fetch would be a bare path node cannot route.
//
// Every request body is recorded: the harness asserts on what the page
// ACTUALLY sent to the model, which is the only proof that a stored
// benchmark stays out of the model's context.
const sentBodies = [];
global.fetch = (input, init) => {
  const raw = String(input);
  const url = /^https?:/.test(raw) ? raw : new URL(raw, baseUrl).toString();
  if (init && init.body) {
    try { sentBodies.push({ url: url, body: JSON.parse(init.body) }); } catch (e) {}
  }
  return realFetch(url, init);
};

const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const out = (ok, msg) => { if (!ok) { console.error(msg); process.exit(1); } };

  // Boot the page's script the way a browser does. The script is wrapped in
  // a function so the harness can reach its top-level functions and read its
  // live state (`chats`) after a reassignment, which a plain
  // runInThisContext(script) could not expose. Wrapping also gives every
  // boot its own scope, so a second boot is a genuine reload -- fresh vars,
  // fresh init() -- rather than a re-declaration in the same scope.
  const bootPage = () => vm.runInThisContext(
    '(function(){' + script + '\n;return {' +
    'get chats(){return chats},' +
    'get currentChatId(){return currentChatId},' +
    'set currentChatId(v){currentChatId=v},' +
    'get generating(){return generating},' +
    'runBenchmark: runBenchmark, renderMessages: renderMessages,' +
    'send: send, toResponsesItems: toResponsesItems,' +
    'ensureChat: ensureChat};})()',
    { filename: 'chat-page.js' }
  );

  const LS_CHATS = 'airouter.chat.chats';
  const stored = () => JSON.parse(lsData[LS_CHATS] || '[]');
  const benchEntries = () => stored().flatMap(c => c.messages)
    .filter(m => m.kind === 'benchmark');

  let page;
  try {
    page = bootPage();
  } catch (e) {
    console.error('chat page script threw at load: '
      + (e && e.stack ? e.stack.split('\n').slice(0, 4).join(' | ') : e));
    process.exit(1);
  }

  // --- the run --------------------------------------------------------------
  try {
    page.runBenchmark();
  } catch (e) {
    console.error('benchmark run threw: '
      + (e && e.stack ? e.stack.split('\n').slice(0, 4).join(' | ') : e));
    process.exit(1);
  }
  // runBenchmark is fire-and-forget: it returns as soon as the first request
  // is in flight, and the run writes its record when the last depth settles.
  // The harness polls the STORE, because the store is what a reload reads --
  // the live page's own state would prove nothing about persistence.
  const t0 = Date.now();
  while (Date.now() - t0 < 120000 && !benchEntries().length) {
    await sleep(200);
  }

  // --- the persistence contract ---------------------------------------------
  const chats = stored();
  out(chats.length > 0, 'no chat was persisted');
  const entries = benchEntries();
  out(entries.length === 1,
    'expected exactly one benchmark entry in the chat history, got ' + entries.length);

  const bench = entries[0].benchmark;
  out(bench && typeof bench === 'object', 'the stored entry carries no benchmark object');
  out(typeof bench.model === 'string' && bench.model.length > 0,
    'the stored benchmark has no model: ' + JSON.stringify(bench.model));
  out(Array.isArray(bench.rows) && bench.rows.length === 4,
    'the stored benchmark kept ' + (bench.rows || []).length + ' rows, want 4 (one per depth)');
  for (const r of bench.rows) {
    out(r.state === 'ok',
      'depth ' + r.depth + ' was stored as ' + JSON.stringify(r.state)
      + ', want ok: ' + JSON.stringify(r));
    out(typeof r.context_tokens === 'number' && r.context_tokens > 0,
      'depth ' + r.depth + ' stored no context_tokens');
  }

  // --- the reload ------------------------------------------------------------
  // A reload is a second boot of the page against the same localStorage,
  // with a fresh document. The stored benchmark must come back as a table:
  // this is the assertion the original bug failed, and the one a string pin
  // on the HTML could never make.
  for (const id of Object.keys(store)) delete store[id];
  let reloaded;
  try {
    reloaded = bootPage();
  } catch (e) {
    console.error('chat page script threw on reload: '
      + (e && e.stack ? e.stack.split('\n').slice(0, 4).join(' | ') : e));
    process.exit(1);
  }
  const transcript = store['messages'];
  out(!!transcript, 'the reloaded page has no transcript element');
  const table = transcript.querySelector('.bench-table');
  out(!!table, 'the reloaded history carries no .bench-table: the benchmark did not survive the reload');
  const rows = table.children || [];
  out(rows.length === 5,
    'the reloaded table has ' + rows.length + ' rows, want 5 (a header plus one per depth)');
  const cells = rows.reduce((n, r) => n + (r.children || []).length, 0);
  out(cells >= 20,
    'the reloaded table has ' + cells + ' cells, want >= 20 (4 depths x 5 columns)');
  // The depth column must carry the measured depths, so the reloaded table
  // is the run's table and not an empty shell.
  const depths = rows.slice(1).map(r => (r.children[0] || {}).textContent);
  out(depths.join(',') === '512,4k,16k,64k',
    'the reloaded table lists depths ' + JSON.stringify(depths)
    + ', want 512,4k,16k,64k');

  // --- the context contract --------------------------------------------------
  // The benchmark is a transcript record, not a turn in the conversation: it
  // must survive in the history but never reach the model as context.
  // Both halves are asserted on the SAME chat that holds the benchmark, by
  // sending a real turn through the reloaded page and reading the request the
  // page actually put on the wire.
  const owner = chats.find(c => c.messages.some(m => m.kind === 'benchmark'));
  const items = reloaded.toResponsesItems(owner.messages);
  out(items.length === 0,
    'the request payload carries ' + items.length + ' items, want 0 for a chat whose only turn is a benchmark');

  store['prompt'].value = 'hello there';
  reloaded.send();
  const s0 = Date.now();
  while (reloaded.generating && Date.now() - s0 < 60000) {
    await sleep(100);
  }
  const turn = sentBodies.filter(b => b.url.indexOf('/chat/completions') >= 0).pop();
  out(!!turn, 'the page sent no chat completion request');
  out(Array.isArray(turn.body.messages) && turn.body.messages.length === 1,
    'the page sent ' + (turn.body.messages || []).length
    + ' messages, want 1: the benchmark reached the model as context');
  out(turn.body.messages[0] && turn.body.messages[0].content === 'hello there',
    'the page sent ' + JSON.stringify(turn.body.messages[0]) + ', want the user turn alone');
  const sentText = JSON.stringify(turn.body);
  out(sentText.indexOf('context_tokens') < 0 && sentText.indexOf('prefill_tokens_per_sec') < 0,
    'the request that reached the model carries benchmark measurements: '
    + sentText.slice(0, 200));

  console.log('benchmark_rows=' + bench.rows.length
    + ' table_rows=' + rows.length
    + ' cells=' + cells
    + ' request_items=' + items.length);
  process.exit(0);
})().catch(e => {
  console.error('harness failed: ' + (e && e.stack ? e.stack.split('\n').slice(0, 4).join(' | ') : e));
  process.exit(1);
});
NODE

if node "$WORK/chat_harness.js" "$WORK/chat.html" "$BASE" >"$WORK/chat_harness.out" 2>&1; then
  note "chat benchmark persisted: $(tr '\n' ' ' <"$WORK/chat_harness.out")"
else
  bad "chat benchmark did not persist: $(tr '\n' ' ' <"$WORK/chat_harness.out")"
fi
if [ "$VERBOSE" = "1" ]; then
  note "harness log:"
  sed 's/^/    /' "$WORK/chat_harness.out"
fi

# --- report -------------------------------------------------------------------
if [ "$fail" -eq 0 ]; then
  note "smoke_chat: ALL PASS"
  exit 0
fi
echo "smoke_chat: FAILURES" >&2
tail -25 "$WORK/airouter.log" | sed 's/^/  airouter | /' >&2
exit 1
