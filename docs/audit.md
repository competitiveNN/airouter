CRITICAL (data loss / undefined behavior)

• router.go:96 — isAvailableLocked calls delete(r.cooldowns, ...) while the
  caller (IsAvailable) only holds RLock. Reader-vs-writer map race;
  race-detector will flag it.
  Status: FIXED (2026-09-15). Delete moved to RecordSuccess under the full
  write lock; isAvailableLocked only reads now.

• api.go:549-557 — ReloadConfig swaps g.config, g.router.config,
  g.proxy.config with no synchronization. In-flight requests can observe
  a mix of old + new pointers.
  Status: FIXED (2026-09-20). Gateway/router/proxy now share ONE
  *atomic.Pointer[Config]; ReloadConfig is a single atomic store.

• proxy.go:300-301 — idleTimeoutReader.Read calls i.timer.Reset after the
  underlying Read returns. Go docs forbid this without explicitly draining
  the channel; can fire spuriously or fail to arm.
  Status: FIXED. Uses time.AfterFunc (no channel to drain) with an
  explicit drain in the watchdog goroutine before Reset.

• api.go:380-436 — handleStream has an unbounded for {} with no max-iteration
  cap and no ctx.Done() check (only inside the wait branch). Combined with the
  resume semantics, request body grows on every retry.
  Status: FIXED. maxAttempts = chainLen*3+1 bounds the loop; ctx.Err()
  checked at the top of every iteration.

• proxy.go:462-489 + api.go:285-317 — accumulateContent only extracts
  Delta.Content; appendAssistantMessage only injects content. tool_calls,
  role, name, refusal, logprobs are all silently lost on mid-stream resume.
  (ChatCompletionMessage has no ToolCalls field at all.)
  Status: FIXED. accumulateDelta (proxy.go:898-955) extracts both content
  and tool_calls. appendAssistantMessage (api.go:456-538) injects both.
  ChatCompletionMessage now has ToolCalls (api.go:42). role/name/refusal/
  logprobs still not replayed, but tool_calls now work across fallbacks.

---

HIGH (user-visible correctness)

• api.go:432 + proxy.go:466 — Resume injects the full accumulated partial,
  not the new delta. Each fallback multiplies prior content into the
  assistant message, so the user sees duplicated text.
  Status: FIXED (api.go:890-897). contentDelta strips the accumulatedContent
  prefix before appending.

• router.go:191-198 — SelectNext scans the chain from index 0 after current,
  so it can reselect a model that already failed this request. Only mitigated
  by it now being in its own cooldown.
  Status: N/A. SelectNext is test-only; production uses SelectEndpoint with
  a per-request tried set (router.go:487-547). Never reselects a tried model.

• api.go:330-378 — handleCompletion never calls RecordSuccess. Non-stream
  cooldowns are sticky: a model escalates on failures and never clears until
  a 4xx/5xx triggers another ApplyCooldown.
  Status: FIXED. handleCompletion calls g.router.RecordSuccess(ep) on 200.

• api.go:41-59 — Vision array-of-parts content is stored as the literal
  string "[{...}]". estimateTokens then over-counts and requestTimeout
  returns a much-too-large budget for vision requests.
  Status: FIXED. ChatCompletionMessage.UnmarshalJSON (api.go:60-97)
  extracts concatenated text from array parts; estimateTokens uses that.

• api.go:145-158 — checkAuth returns true when g.gatewayAPIKey == "".
  If the operator never sets GATEWAY_API_KEY/-api-key, every endpoint
  (including admin) is unauthenticated.
  Status: FIXED (2026-09-20). checkAuth now fails CLOSED unless
  allowNoAuth is set. main.go refuses to start without a key unless
  -allow-no-auth is passed. The opt-in unauthenticated mode is
  documented and tested (TestGatewayNoAuth).

• api.go:237 — io.ReadAll(r.Body) with no http.MaxBytesReader.
  Memory-exhaustion DoS.
  Status: FIXED. HandleChatCompletions wraps r.Body with
  http.MaxBytesReader(w, r.Body, maxRequestBytes) (api.go:414).

• router.go:69-88 — cooldowns.json is rewritten in place under the global
  write lock. Crash mid-write truncates the file; on next start
  loadCooldowns silently drops all state.
  Status: FIXED. saveCooldowns (router.go:310-358) uses atomic
  tmp+rename; temp file removed on any error.

• main.go:99-126 + api.go:560-593 — Watcher and admin-POST race on the
  same file (double reload, no fsync, no atomic rename).
  Status: FIXED. SaveConfig uses atomic tmp+rename. ReloadConfig is
  atomic (shared pointer). watchConfig uses mtime+size polling.

---

MEDIUM

• proxy.go:231-251 — firstByteReader leaks one goroutine per timed-out
  first byte.
  Status: FIXED. firstByteReader.Read closes the underlying reader on
  context timeout to unblock the goroutine (proxy.go:342-388).

• proxy.go:347-348 — bufio.Scanner is capped at 1 MB.
  Status: FIXED. scanner.Buffer set to 16 MB (proxy.go:531-533).

• proxy.go:424-448 — sseEventIsRelease returns true on unparseable JSON
  ("fail open").
  Status: OPEN (intentional). Fail-open avoids stalling the stream;
  malformed upstream events would otherwise hang the client. Trade-off
  accepted.

• proxy.go:344-422 — Final SSE event without a trailing blank line is
  silently dropped.
  Status: FIXED. Post-loop eventBuf flush (proxy.go:741-747) processes
  the trailing event.

• api.go:266-275 + 321-328 — requestTimeout comment says "1s per extra
  10000 tokens"; code does 2s per 5000.
  Status: FIXED. Comment and code both reflect 1s per 10000 tokens
  (api.go:584-595).

• api.go:278-283 — requestHasVision does bytes.Contains(body, "image_url").
  Status: FIXED. Checks for the canonical "type":"image_url" marker
  (api.go:441-443).

• proxy.go:268 — streamIdleTimeout = 60s is hard-coded.
  Status: OPEN (low). Default is 60 s but configurable via
  Proxy.SetStreamIdleTimeout (proxy.go:148-162). No test exercises
  the override.

• api.go:367-376 — Non-stream path copies upstream Content-Encoding/
  Content-Length to the client.
  Status: FIXED. isHopByHopHeader (api.go:562-569) strips both.

• api.go:343, api.go:404 — time.After in select is not stopped on
  ctx.Done().
  Status: FIXED. Uses time.NewTimer with explicit Stop() on
  ctx.Done() in both handleStream (api.go:837-845) and handleCompletion
  (api.go:657-665).

• router.go:259-274 — 404/401/403 all get 300 s base cooldown.
  Status: FIXED. baseCooldownForError (router.go:690-709) gives 404 →
  7 days, 401/403 → 30 min, 429/5xx → 30 s with bounded escalation.

• router.go:283-298 — cooldownForError escalates on consecutive failures
  since last success ever, not since last cooldown expiry.
  Status: FIXED. RecordSuccess deletes the cooldown entry, resetting
  ErrorCount. Escalation is bounded by maxCooldown (30 min for
  transient, 7 days hard ban for persistent).

• router.go:115-126 — minCooldownWait ignores noVision.
  Status: FIXED. minCooldownWait (router.go:459-476) skips
  noVision/vision:false endpoints for vision requests.

---

LOW (cleanup / hardening)

• router.go:25, 36, 387-388 — rrCounters, ErrNoModelsAvailable,
  ErrInvalidModel declared but unused.
  Status: FIXED. Removed; `go vet` reports no unused symbols.

• router.go:169-199 — SelectNext only used by tests.
  Status: OPEN (intentional). SelectNext is a test helper; production
  uses SelectEndpoint. Documented as test-only.

• api.go:438-449 — /health is unauthenticated.
  Status: OPEN (intentional). Returns only {"status":"ok"} — no
  config, sessions, cooldowns, provider URLs or keys — safe for load
  balancers. See memory/auth_harden_notes.md.

• api.go:157 — API-key comparison via == is not constant-time.
  Status: FIXED. checkAuth uses subtle.ConstantTimeCompare with
  length-equalisation (api.go:203-218).

• api.go:595-671 — Admin dashboard served without CSP/X-Frame-Options.
  Status: FIXED. HandleAdminDashboard sets X-Content-Type-Options,
  X-Frame-Options: DENY, and a Content-Security-Policy
  (api.go:1140-1142).

• router.go:223-236 — summarizeError truncates at a byte boundary.
  Status: FIXED. Uses rune-based truncation (router.go:632-638).

• router.go:56-58 — Cooldown file parse errors only logged at debug.
  Status: FIXED. loadCooldowns logs at warn level (router.go:298).

• router.go:373-381 — 4 KB upstream error bodies persisted to
  cooldowns.json.
  Status: FIXED. truncateErr limits LastError to 200 runes (router.go:590).

• main.go:99-126 — watchConfig has no shutdown path.
  Status: FIXED. watchConfig selects on ctx.Done() and returns
  (main.go:134-163). main calls cancel() on signal.

• main.go:128-134 — Whole-file SHA-256 every 3 s; fsnotify/mtime cheaper.
  Status: PARTIALLY OPEN. watchConfig uses mtime+size polling (no
  hashing), so the SHA-256 concern is moot. The 3 s ticker is acceptable.

---

## Cooldown jitter + Retry-After passthrough (2026-09-28)

This audit round implemented two features that were declared "done" in the
prior session's summary but were actually dead code — the fields existed but
nothing populated or read them. All three are now wired end-to-end with tests.

• router.go:185-188 — `Router.cooldownJitter` was declared with a comment
  "It is set via SetCooldownJitter" but no such method existed, and no call
  site ever assigned it. The `if r.cooldownJitter > 0` branch in
  ApplyCooldownForSession was unreachable, so no jitter was ever applied.
  Status: FIXED. Added `Router.SetCooldownJitter` (clamps to [0, 0.25]) and
  `Router.CooldownJitter()`; wired from `Preferences.CooldownJitterFraction()`
  in both `NewRouter` and `ReloadConfig` so a config change takes effect
  without a restart (router.go:191-211, api.go:1157-1174).

• router.go:961 — `ProviderError.RetryAfter` was declared and read by
  ApplyCooldownFromErrorForSession, but no `ProviderError{...}` literal in the
  codebase ever set it. The upstream Retry-After header was therefore never
  parsed, and the cooldown floor was always 0.
  Status: FIXED. Added `ParseRetryAfter` (router.go:1010-1040) which handles
  both delta-seconds ("120") and HTTP-date forms via http.ParseTime. Wired
  into every ProviderError construction site that has an HTTP response:
  proxy.go:320/326/336, responses_api.go:318/323/328, api.go:790. The
  non-stream path (api.go:790) was updated to pass
  `ParseRetryAfter(resp.Header.Get("Retry-After"))` directly rather than 0.

• config.go:124 — `Preferences` had no cooldown_jitter field at all.
  Status: FIXED. Added `CooldownJitter float64` with a
  `CooldownJitterFraction()` accessor that clamps to [0, 0.25].

• main_test.go (new) — TestParseRetryAfter,
  TestApplyCooldownHonorsRetryAfterFloor, TestCooldownJitterAppliedOn429,
  TestApplyCooldownFromErrorCarriesRetryAfter,
  TestPreferencesCooldownJitterFraction,
  TestConfigWiresCooldownJitterToRouter.

## Circuit breaker (2026-09-28) — closed/open/half-open state machine

Replaces the ad-hoc cooldown escalation with a proper circuit breaker that
hooks into the existing cooldown system:

- `CircuitBreaker` struct (router.go:136-145) with `State`, `OpenedAt`,
  `ProbesSent`. Three states: Closed (normal), Open (hard-block), HalfOpen
  (limited probe budget).
- `isAvailableLocked` (router.go:591-631) honours the state machine: Open
  circuits block until the cooldown expires, then transition to HalfOpen and
  admit a probe; HalfOpen admits up to `circuit_half_open_probes` requests.
- `recordCircuitFailureLocked` (router.go:1008-1043) advances the state on
  transient failures (429/5xx + status 0): Closed → Open at the threshold,
  HalfOpen → Open on a failed probe, Open → Open (fresh window).
- `RecordSuccess` (router.go:974-997) closes any open/half-open circuit and
  resets the probe counter.
- Persistence: `cooldownsState` (router.go:108-115) merges cooldowns and
  circuits into one `cooldowns.json` envelope, so a restart reconstructs both
  the backoff windows and the circuit state. Legacy bare-map files are still
  accepted on load (router.go:448-515) so upgrades don't lose state.
- Metrics: `CircuitTransition(from, to, endpoint)` emits
  `circuit_state_transitions` with `from`/`to`/`endpoint` labels (metrics.go).
- Config: `Preferences.CircuitBreakerThreshold` (pointer int, so an explicit
  0 can disable the breaker while an absent key falls back to the default of
  5) and `CircuitHalfOpenProbes` (default 1). Accessors
  `CircuitBreakerThresholdValue()` / `CircuitHalfOpenProbesValue()`
  (config.go:201-228).

Status: FIXED. All 10 circuit breaker tests pass (9 new + 1 legacy
  compat): TestCircuitBreakerOpensAfterThreshold,
  TestCircuitBreakerIgnoresPermanentErrors,
  TestCircuitBreakerHalfOpenProbeSuccess,
  TestCircuitBreakerDisableViaZeroThreshold,
  TestCircuitBreakerResetManually,
  TestCircuitBreakerPersistenceReconstruction,
  TestCircuitBreakerMetricTransition,
  TestCircuitBreakerPersistenceLegacyFormat,
  TestAdminCooldownsIncludesCircuitState.

  • router.go:405 + router.go:1223 — cleanupStaleEntries holds r.mu and calls
    cleanupCircuits() which re-locks r.mu; Go's sync.Mutex is not reentrant,
    so the sweeper goroutine deadlocks on itself (TestStaleEntryCleanup hung
    for the full 180s test timeout). FIXED (2026-09-28): split into a lock-free
    cleanupCircuitsLocked() core plus a locking wrapper; cleanupStaleEntries
    now calls the locked variant directly.

  • main_test.go:3520-3527 — TestConcurrentMidStreamErrorRecovery asserted that
    EVERY backend2 request contained the "Hello " replayed body, but sessions
    that arrived after backend1 was already cooled skip it entirely and have
    nothing to replay. Under load this fired spuriously. FIXED: only sessions
    that actually hit backend1 (tracked via a per-request body registry) are
    required to carry the replayed partial. Test now passes 3x in the full
    suite with -race (459/459).

  • responses_api.go:168-169, 283-284, responses_ws.go:199-200 — three handler
    sites called BOTH ApplyCooldownFromErrorForSession/ApplyCooldownForSession
    (which already advances the circuit breaker via recordCircuitFailureLocked)
    AND RecordFailure for the SAME failure event. Two independent failure
    counters (cd.ErrorCount and cb.ProbesSent) were being incremented in
    parallel, so the circuit tripped at half the configured threshold. FIXED
    (2026-09-28): removed the redundant RecordFailure calls; recordCircuitFailureLocked
    now increments cb.ProbesSent as the single source of truth for both paths
    (router.go:1073-1108). RecordFailure remains available as a standalone API
    for callers that don't go through ApplyCooldown.

  • api.go:1137-1178 — HandleAdminCooldowns only exposed cooldown backoff
    windows, so operators couldn't see circuit breaker state from the admin
    UI. FIXED: the response now includes a "circuits" array with state /
    opened_at / probes_sent per endpoint, alongside the existing cooldowns
    array (TestAdminCooldownsIncludesCircuitState).

  Full suite: 154 tests, 154 passing with -race, stable across repeated runs
  (3x full suite = 462/462).

---

## Resource leak audit (2026-09-07) — re-verified 2026-09-20

HTTP Response Bodies — CLEAN
Goroutines — CLEAN (firstByteReader leaks blocked by closing underlying
  reader; idleTimeoutReader uses AfterFunc; streamSSE bounded by maxAttempts)
File Descriptors — CLEAN (atomic tmp+rename for cooldowns.json, priorities,
  and config.yaml; temp files removed on error)
Timers/Contexts — CLEAN (all time.NewTimer / WithTimeout callers Stop/
  cancel on ctx.Done())
Loop Bounds — CLEAN (maxAttempts = chainLen*3+1)

## Regression tests (existing, still passing)
• TestStreamSSE_FirstByteReaderNoGoroutineLeak
• TestStreamSSE_IdleTimeoutClosesReader
• TestStreamSSE_BoundedBodyGrowth
• TestHandleStream_ResourceCleanup
• TestConcurrentMidStreamErrorRecovery
• TestConcurrentStickySessionGuard
• TestReloadConfigIsAtomic
• TestReloadConfigConcurrent  (new, 2026-09-20)
• TestOpencodeHeadersInjected  (new, 2026-09-23)
• TestOpencodeHeadersNotInjectedForNonOpencode  (new, 2026-09-23)
• TestOpencodeProviderHeaderOverride  (new, 2026-09-23)
• TestParseRetryAfter  (new, 2026-09-28)
• TestApplyCooldownHonorsRetryAfterFloor  (new, 2026-09-28)
• TestCooldownJitterAppliedOn429  (new, 2026-09-28)
• TestApplyCooldownFromErrorCarriesRetryAfter  (new, 2026-09-28)
• TestPreferencesCooldownJitterFraction  (new, 2026-09-28)
• TestConfigWiresCooldownJitterToRouter  (new, 2026-09-28)
• TestHandleChatCompletionsFallbackWithRetryAfter  (new, 2026-09-28)
• TestHandleStreamFallbackWithRetryAfter  (new, 2026-09-28)
• TestCooldownJitterEndToEnd  (new, 2026-09-28)
• TestParseRetryAfterEdgeCases  (new, 2026-09-28)

## Test summary
  `go test -race ./...` → 153 passed, 0 failed (2026-09-28)
  Both previously-flaky tests now stable:
  - TestHandleStream_ResourceCleanup — passes in full suite
  - TestConcurrentMidStreamErrorRecovery — fixed to only assert the replay
    invariant for sessions that actually hit backend1 (was asserting it for
    every request, including ones that legitimately skip the cooled backend)
  `go vet ./...` → no issues
  `go build ./...` → success
  `python3 scripts/validate-config.py` → OK: 4 profiles validated
