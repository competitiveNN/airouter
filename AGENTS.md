# AI Router — Constraints, Requirements & User Preferences

This file is the authoritative source of truth for the airouter gateway. 
Every requirement below was derived from the project's commit history (fixes, 
invariants, and audit rounds) and supersedes any earlier version. The codebase, 
tests, scripts, and CI must satisfy all of them. The code must be gofmt-clean, 
go vet clean, and pass `go test -race -count=1 ./...`.

## 1. Core gateway & model profiles

- Expose exactly four logical model profiles: `smart` (most intelligent), 
  `work` (coding workhorse), `fast` (small/quick/simple tasks), `large` (very 
  large context windows + context compression). No extra profiles (e.g. `test`) 
  are allowed.
- OpenAI-compatible endpoint in and out: `/v1/chat/completions`, `/v1/responses`, 
  `/v1/responses/ws`, `/v1/models`, `/health`, `/metrics`, admin endpoints.
- Each profile routes through a fallback chain of concrete endpoints. Chain order 
  is configured; the router walks it in order on failure.
- Authentication via env-var API keys only (`GATEWAY_API_KEY` / `-api-key`); 
  `-allow-no-auth` is the explicit opt-in to unauthenticated mode. `checkAuth` 
  fails closed on an empty gateway key. Constant-time key compare. Request body 
  limited to 32 MiB. Hop-by-hop headers stripped.
- Custom providers with custom URLs are supported; config is stored/read from 
  storage (config.yaml + cooldowns.db).
- Every concrete endpoint carries an explicit `vision: true/false`. `nil` Vision 
  is treated as supported (default-on) for backward compat.
- `/v1/models` advertises `max_context_tokens` = the SMALLEST context window in 
  the profile's chain (conservative, safe for every fallback). Field is named 
  `max_context_tokens`, not `max_tokens` (input capacity, not output ceiling).
- When a client omits `max_tokens`, the gateway injects a default ceiling: half 
  the advertised context window, or `preferences.default_max_tokens` (nil=auto, 
  0=never, positive=absolute). Injection mutates the raw request body and happens 
  before session derivation so affinity is preserved. Thinking tokens live outside 
  visible output and can exceed a half-context budget — document this.
- Initial model rotation: new sessions spread across the first 
  `preferences.initial_rotation_window` (default 8) chain positions, derived from 
  `hash(sessionID)` (not an arrival counter) so concurrent same-session requests 
  agree. Window is anchored to chain positions, not first N eligible endpoints, 
  so cooldowns shrink the set; a new session's first pick can only improve. Applies 
  only to the first endpoint of a brand-new session; pinned sessions stay sticky; 
  retries walk strictly in configured order.
- `intelligence:` is a real parsed field on `ModelEndpoint` (not just a comment), 
  emitted by `regenerate_config.py`.

## 2. Session stickiness & fallback

- Session IDs are content-derived: `bodySessionID` hashes `model + messages` from 
  the request body. Same body → same session → same sticky model. No client can 
  inject or pin a session ID (no `X-Session-ID` / query param). Mid-stream resume 
  preserves the session because `handleStream` captures `sessionID` once before the 
  retry loop.
- A session is always routed to the same model. Sticky session is updated only on 
  initial selection (`len(tried)==0`) so concurrent same-session requests do not 
  stomp each other's fallback routing.
- Tried endpoints are tracked per request (`tried` map) and per session (`triedKeys` 
  on `sessionEntry`). `SelectEndpoint` excludes tried keys; on exhaustion, picks the 
  first endpoint even if all are tried/cooled. `RecordSuccess` clears the tried set.
- When all endpoints in a chain are cooled, `SelectEndpoint` returns `nil` + 
  `minCooldownWait` (never the first endpoint with zero wait — that caused infinite 
  429 retry loops).
- Retry budget: `maxAttempts = chainLen*3 + 1`. All four fallback loops (chat 
  non-stream, chat stream, Responses non-stream, Responses stream) and the 
  WebSocket turn loop are bounded by `maxAttempts` AND a shared wall-clock budget 
  (`maxFallbackWallClock = 30s`, equal to `maxSessionInterruption`). Per-attempt timeout uses `firstByteReader` 
  (time-to-first-token guard only); the parent context stays alive for the rest of 
  generation.
- On retry-budget exhaustion, migrate the session to the substitute chain instead 
  of failing.
- Vision requests: if the chain has no vision-capable model → reply HTTP 200 with 
  assistant message "Vision not supported" (streaming variant emits a single SSE 
  chunk + [DONE]). If vision-capable models exist but all are cooled/noVision → 
  "Vision currently not available". Same interception at the maxAttempts-exhausted 
  gate so a long cooldown cannot fall through to a generic 503.
- `VisionChainState` / `VisionChainStatus` inspect the chain; sticky session 
  pinned to a non-vision model cannot satisfy a vision request — the next eligible 
  model is counted.
- Client disconnects must not cool a healthy model (classify as client disconnect).
- Only the client-facing status is counted in `requests_total`; attempts have their 
  own counters (`recordAttempt`, `endpoint_attempt_failures`).
- Non-200 upstream attempts that ultimately succeed must not be double-counted.
- On upstream saturation, fall back to the next model instead of returning 503; 
  treat saturation as gateway-wide limit and bound Responses concurrency.
- Mid-stream proxy failure: retry the same endpoint DIRECT once before trying 
  another proxy (`StreamAttemptOptions.Direct`). If direct also fails, move on.

## 3. Streaming, SSE framing & Responses API invariants

- `streamSSE` buffers SSE events until the first real content token (or `[DONE]`); 
  an upstream error before that point is discarded so the client receives nothing 
  and the router falls back with no duplicated output. Scanner buffer 16 MB. Flush 
  a trailing SSE event lacking a blank line.
- The `[DONE]` sentinel must be the LAST event and appear EXACTLY ONCE. 
  `streamSSE` intercepts `[DONE]` unconditionally, flushes the pending buffer in 
  order, and emits the single terminating sentinel itself. The tool-calls branch 
  no longer needs its own isDone guard.
- SSE comment lines (`: keepalive`) are ignored (SSE spec) and must not leak to 
  the client or trigger release.
- `sseEventIsRelease` fails open deliberately (so an unrecognized event shape cannot 
  stall the stream forever); comments are kept out of the event path instead.
- Stream-to-client returns an additional `completionTokens` int (from the SSE usage 
  event) and emits a synthetic `[DONE]` when the upstream closes the stream without 
  one. Non-SSE 200 responses are treated as failures and trigger fallback.
- `streamSSE` writes the event that triggers release AFTER flushing the buffer it 
  belongs behind, so delta ordering is preserved (role=assistant not after first 
  content token).
- Mid-stream fallback replays partial content and tool_calls (`accumulateDelta` + 
  `appendAssistantMessage`); partial-content replay must not duplicate.
- Partial-content prefix stripping must not duplicate output when the next model 
  refactors the prefix.
- Responses API: `responsesRequest`/item/contentPart/envelope; `imageRef` normalizes 
  bare string or `{"url":...}`. `finish_reason == "length"` → status `"incomplete"`. 
  Tool calls emitted whole (added → args done → done). Translates chat SSE into 
  Responses event lifecycle. Terminates on `response.completed` exactly once, last. 
  An upstream SSE "error" event aborts the attempt (no empty successful-looking 
  `response.completed`). Buffered: every Responses attempt is translated into an 
  in-memory buffer and only replayed on success, so a budget-exhausted error writes 
  to a clean stream.
- WebSocket transport carries several correlated turns over one socket; upstream 
  calls go through the normal HTTP chain, so routing/cooldowns are shared. Budget 
  bounded.
- Degraded SSE writers must emit the terminating sentinel (`[DONE]` / 
  `response.completed`) so clients don't hang.
- `ChatCompletionMessage.UnmarshalJSON` must carry `tool_calls` through; must 
  marshal multimodal array content when `ImageURLs` is set.
- `idleTimeoutReader` uses a safe stop-then-reschedule timer; `firstByteReader` 
  closes the underlying reader on first-byte timeout (no goroutine leak). Watchdog 
  goroutines must exit on Close.
- Keep streaming clients alive during silence (keepalives / SSE comments every 10s).

## 4. Cooldowns & circuit breakers

- Cooldowns persisted to SQLite (`cooldowns.db`) with WAL + `busy_timeout`, single 
  connection, transactional apply/import. Survive restarts (loaded on startup, 
  expired filtered). Atomic write (temp + fsync + rename). Debounced saves (5s). 
  Flush pending save on `Close()`.
- Cooldown escalation is status-aware and bounded (cap 30 min / 10 min for streaming 
  grace). Fixed schedule replaced runaway exponential: 60s, 1h, 8h, 24h, 3d, 7d 
  capped at 7d (per endpoint). `RecordSuccess` resets error count. ErrorCount capped 
  at 100.
- Status classes: 429 low cooldown; 404 high (7 days); 401/403 30 min (auth — 
  user-side fix); client errors (400/413/422) → 0 cooldown; protocol/quota 
  refusals elevated; timeouts ramp. `isClientRequestError`.
- Cooldown grace for streaming requests: wait out short cooldowns instead of failing.
- Jitter: up to 25% on 0/429/5xx, wired via `Preferences.CooldownJitterFraction()` → 
  `NewRouter` and `ReloadConfig` (config changes take effect without restart). Clamped.
- `Retry-After` (delta-seconds and HTTP-date) parsed via `ParseRetryAfter` and 
  honored as a floor on computed cooldown. `ProviderError.RetryAfter` populated at 
  every construction site with an HTTP response.
- Circuit breaker: Closed/Open/Half-Open state machine with threshold (default 5) 
  and probe budget (default 1). `RecordFailure`/`RecordSuccess` through all handler 
  paths (non-stream, stream, responses, ws). Persisted in cooldowns.db with 
  backward-compatible legacy loading. `/v1/airouter/state` exposes 
  `GetAllCircuitsState()`. Prometheus metric for transitions.
- Malformed circuit entry must not wipe all cooldown state: decode cooldowns and 
  circuits independently. `loadCooldowns` tolerant.
- Tried-endpoint retry storms prevented: mark model "tried" for the session so 
  `SelectEndpoint` won't pick it again after cooldown expires.
- Stale entries (cooldown/noVision/tps) cleaned up when models are removed from 
  config.
- `GetSession` uses RLock (not Lock).
- DELETE `/admin/cooldowns[?model=...]` authenticated, 404 on unknown model, 
  persists immediately.
- Max cooldown grace 5m to prevent 503s under load.

## 5. Observability & metrics

- `/metrics` mounted and gated behind `checkAuth`. Dashboard endpoints authenticated.
- Prometheus exposition correctness: HELP/TYPE name must match sample family (use 
  `airouter_*` consistently). Histogram invariants: `le` present, strictly 
  increasing, exactly one `+Inf` last, cumulative counts non-decreasing, `+Inf` == 
  `_count`, and buckets consistent with `_sum` (`_sum >= above * topBound`). 
  Fuzz-tested (FuzzExpositionRoundTrip, FuzzHistogramInvariantsSurviveGarbage).
- Label escaping: backslash, quote, newline escaped at every site (`labelEscape`); 
  round-trip fuzzed.
- Cardinality bounds: every label map capped at `preferences.max_label_cardinality` 
  (default 512, range 16–65536, rejected not clamped out of range). Admission + 
  decision + insert under one write lock. Unknown values collapse into `__overflow__` 
  (under their own label name) and are counted via `airouter_metrics_label_overflow_total`. 
  Already-tracked labels stay distinct (FIFO recycling by admission, not LRU; 
  per-bucket histogram data not merged). Overflow bucket gets no histogram series; 
  totals published as gauges `airouter_evicted_attempts_latency_seconds_sum`/`_count`. 
  Counter maps also bounded (`counterStatus`, `counterFailure`); status is upstream 
  status.
- Metrics labels must not leak storage keys (no `requests_by_model_` prefix in label).
- CircuitState exported via `.String()` (not rune).
- Dead/unwired metrics must not be shipped; if a metric is exposed it must be 
  incremented at the real path.
- Dashboard telemetry: retention ring (7d default), journal restore, prune. 
  `DashboardQuery` must not hold the global write lock during aggregation; snapshot 
  then aggregate outside lock. Telemetry stop must quiesce background writes 
  (WaitGroup). Restore must clear stale ring slots outside the restored window.
- Metrics must not OOM: retention structures have a practical, configurable bound; 
  document memory cost.
- Config churn must not grow labels unbounded (hot-reload safe).

## 6. Config pipeline & sync rules

- Sync commands: `python3 fetch-free-models.py --json --probe-auto --save 
  /tmp/free-models.json`, then `python3 regenerate_config.py --write`, then 
  `python3 scripts/check-rules.py`. `--probe-auto` records whether each chain 
  terminator actually answers; the generator refuses to write a terminator the 
  provider has refused.
- Free-tier refusals need no sync-time handling: the daemon records them in 
  `cooldowns.db` from real client traffic and backs the endpoint off at request 
  time. Never hard-code a model's free status into a curated list.
- Permanent blacklist: cooldowns.db rows reporting 404/"not found" on a model 
  older than two weeks land in `blacklisted-models.json` (committed). The sync 
  runs this automatically. `fetch-free-models.py --blacklist-report` prints the 
  current blacklist. `--no-blacklist` re-admits everything and must not be left 
  set. Rule 14 keeps a blacklisted id out of config.yaml.
- Rule 8: every model in a chain must be in the fetched model list. Rule 6: 
  terminator comment must match probe verdict.
- `regenerate_config.py` safe by default (no `--write` = report only). 
  `validate_config_text` refuses to write if a provider lost its url, a protected 
  top-level key vanished, or a profile ends up empty. Backup (`config.yaml.bak`) 
  then temp file + fsync + `os.replace`.
- Header preservation anchors on `(?m)^models:` (not the literal inside comments). 
  Find block end as the next top-level key.
- Model matching: tiered slug matcher (exact normalized → alnum-folded equality → 
  contiguous containment, least decoration wins → difflib ≥0.85 restricted to 
  insertion/deletion-only edits; sizes never collapsed; ambiguity refused; opaque 
  ids `x-*`, `stealth`, `preview` never matched). Google `*-latest` aliases resolve 
  via v1beta/models to the newest stable numbered version (`match_id`) before AA 
  enrichment; unresolvable aliases skipped.
- AA enrichment migrated to V2 API; stop chain collapsing. Arena.ai code ELO as 
  fallback intelligence signal.
- First-seen cache records fetch date for models without AA release dates; stable 
  across runs. Sort: intelligence desc, release date desc (newest first), size-tier 
  desc (large before small).
- Google keys are a key group: emit the same model on every configured Google key 
  (gemini..gemini5) consecutively, like nvidia/nvidia2/nvidia3 and 
  commandcode/commandcode2.
- NVIDIA models emitted as consecutive nvidia/nvidia2/nvidia3 trio.
- Opencode endpoints probed with a real 1-token call; only those answering 200 
  emitted. Drop provably-dead opencode endpoints.
- Protocol normalization parity between Python and Go (`responses_outbound.go` 
  `protocolFor`).
- `preferences` struct: `newest_first_on_tie`, `initial_rotation_window`, 
  `default_max_tokens`, `cooldown_jitter`, `max_label_cardinality`, 
  `circuit_breaker_threshold`, `circuit_half_open_probes`.
- Systemd: `scripts/run-airouter.sh` sources `.envrc` under `set -a` and execs the 
  daemon; fails closed when `.envrc` missing. Port 9090.

## 7. Verification & gate procedures

- `scripts/gate.sh` is the canonical entry point: serialized with flock, fingerprints 
  the tree, refuses to call a run valid if the tree changed underneath (exit 3). 
  Run everything through it.
- `scripts/run-config-tests.py` runs pytest in-process (nothing rewrites output) 
  and asserts a collection floor (default 40), so "collected 0" is a failure. Real 
  binary for opaque failures: `/home/fra/.local/bin/pytest`.
- The shell layer rewrites `python3 -m pytest` and `go test` output into a one-line 
  summary and reports a suite that FAILED TO COLLECT as "No tests collected" with 
  exit 0. Never read a pass from that form.
- `scripts/mutation-check.py` plants a defect in the sync chain, proves the suites 
  go red, restores the tree, and exits non-zero if any planted defect went unnoticed. 
  Run after changing the config pipeline, fetcher, or rules checker. Exact per-suite 
  test floors (`SUITE_TEST_FLOORS`); a suite losing even one test fails the build.
- `scripts/secret-scan.sh` checks tracked content, recent history, `.git/config`. 
  Covers gh*/github_pat_/glpat-/AKIA/xox* and private-key headers. No false positives 
  on `api_key_env` or CI key fixtures.
- `scripts/sse-negative-check.sh` proves SSE ordering tests have teeth: reintroduces 
  the historical bug, requires tests to fail, restores, requires pass + byte-identical 
  file. Exit 2 if `streamSSE` refactored past the mutation site.
- `scripts/audit-drift-check.py` resolves every anchor in `docs/audit.md` against Go 
  sources; fails on a missing one. Uses rglob (not root-only glob), skipping `.git` 
  and `vendor`, sorted.
- CI: gofmt, go vet, go test -race, config validation, generator regression tests, 
  py_compile, sse-contract job, fuzz-gate job, unpushed-export job. 
  `continue-on-error: false`, `fail-fast: false`.
- Fuzzing: `scripts/fuzz-gate.sh` discovers targets recursively, fuzzes one package 
  per invocation, checks untracked crashers per target directory. 
  `scripts/fuzz-gate-selftest.sh` reproduces the multi-package layout.
- After editing a test file, grep for the new test by name before believing a result. 
  A write can report success and not land.
- Never `git checkout -- <path>` to undo your own edit — it discards the WHOLE 
  working-tree file and silently destroys prior uncommitted work. Check 
  `git status --porcelain -- <path>` before editing; restore from a copy you made 
  (`cp` before, `cp` back after).
- Never commit secrets (`.env`, credentials, keys). Don't use `git remote -v` (may 
  include tokens).
- Model list changes go through the documented sync pipeline; don't hand-edit 
  config.yaml chains.

## 8. Forbidden / required behaviors (audit-derived)

- No client-visible errors: any upstream error (including mid-stream) must be 
  caught, the session moved to the next model, and streaming resumed with only a 
  slight delay. The client must never see an error.
- No duplicated or truncated SSE output: exactly one terminating sentinel, last; 
  no leading sentinel; comments not leaked.
- No infinite loops: all fallback loops bounded by maxAttempts + wall clock.
- No data races: config pointers atomic; session/cooldown maps guarded; no write 
  under RLock; inflight uses RWMutex.
- No unbounded memory: metrics labels capped; ErrorCount capped; stale entries 
  cleaned; debounced saves.
- No 503 on saturation: fall back instead.
- Config reload atomic: single atomic store onto one shared `*atomic.Pointer[Config]`.
- HandleAdminConfig POST/PUT reuses ReloadConfig.
- Hot-reload config from disk: watchConfig polls SHA-256 every 3s, invalid configs 
  logged and skipped, last good config stays active.
- Request body session derivation must happen after injection and before routing.
- Vision requests must never 503 generically when a vision-capable model exists in 
  the chain (intercept + friendly message).
- `/metrics` must be authenticated; health/state endpoints should be access-controlled 
  when auth enabled.
- Dead code / unwired handlers / unwired metrics are defects — remove or wire them.
- gofmt clean; go vet clean; go test -race green.
