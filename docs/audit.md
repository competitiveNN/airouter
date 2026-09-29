# Security & correctness audit (2026-08-29, updated 2026-09-29)

> **Reading the `file.go:NNN` references.** These are *point-in-time* citations
> from the audit rounds, recorded so the original finding stays reproducible.
> They are not maintained and they have drifted: line numbers in this document
> no longer correspond to the current source. **Cite the symbol, not the line**
> when you act on a finding — the findings themselves are accurate, the
> coordinates are not. Where a finding was fixed, the `Status:` line names the
> function that now carries the fix, and that name is the durable anchor.
>
> Status vocabulary used below:
>
> - **FIXED** — the defect no longer exists in the code.
> - **ACCEPTED** — deliberately not fixed; the trade-off is recorded and,
>   where it matters, pinned by a test. This is not a backlog item.
> - **N/A** — the code the finding describes no longer exists.
> - **OPEN** — genuinely unresolved. This list should be short.

CRITICAL (data loss / undefined behavior)

• `Router.isAvailableLocked` calls `delete(r.cooldowns, ...)` while the
  caller (`IsAvailable`) only holds RLock. Reader-vs-writer map race;
  race-detector will flag it.
  Status: FIXED (2026-09-15). Delete moved to `RecordSuccess` under the full
  write lock; `isAvailableLocked` only reads now.

• `GatewayContext.ReloadConfig` (was api.go:549-557) — ReloadConfig swaps g.config, g.router.config,
  g.proxy.config with no synchronization. In-flight requests can observe
  a mix of old + new pointers.
  Status: FIXED (2026-09-20). Gateway/router/proxy now share ONE
  *atomic.Pointer[Config]; ReloadConfig is a single atomic store.

• `idleTimeoutReader.Read` (was proxy.go:300-301) — idleTimeoutReader.Read calls i.timer.Reset after the
  underlying Read returns. Go docs forbid this without explicitly draining
  the channel; can fire spuriously or fail to arm.
  Status: FIXED. Uses time.AfterFunc (no channel to drain) with an
  explicit drain in the watchdog goroutine before Reset.

• `handleStream` (was api.go:380-436) — handleStream has an unbounded for {} with no max-iteration
  cap and no ctx.Done() check (only inside the wait branch). Combined with the
  resume semantics, request body grows on every retry.
  Status: FIXED. maxAttempts = chainLen*3+1 bounds the loop; ctx.Err()
  checked at the top of every iteration.

• `accumulateContent` (was proxy.go:462-489 + api.go:285-317) — accumulateContent only extracts
  Delta.Content; appendAssistantMessage only injects content. tool_calls,
  role, name, refusal, logprobs are all silently lost on mid-stream resume.
  (ChatCompletionMessage has no ToolCalls field at all.)
  Status: FIXED. accumulateDelta (proxy.go:898-955) extracts both content
  and tool_calls. appendAssistantMessage (api.go:456-538) injects both.
  ChatCompletionMessage now has ToolCalls (api.go:42). role/name/refusal/
  logprobs still not replayed, but tool_calls now work across fallbacks.

---

HIGH (user-visible correctness)

• `contentDelta` / `Resume` (was api.go:432 + proxy.go:466) — Resume injects the full accumulated partial,
  not the new delta. Each fallback multiplies prior content into the
  assistant message, so the user sees duplicated text.
  Status: FIXED (api.go:890-897). contentDelta strips the accumulatedContent
  prefix before appending.

• `Router.SelectNext` — scanned the chain from index 0 after current, so it
  could reselect a model that already failed this request. Only mitigated by it
  now being in its own cooldown.
  Status: N/A. `SelectNext` was removed. Production uses `Router.SelectEndpoint`
  with a per-request tried set, which never reselects a tried model.
  The two tests that had kept the dead name alive — `TestRouterSelectNext` and
  `TestRouterSelectNextAllInCooldown` — were renamed to
  `TestRouterSelectEndpointSkipsCooldown` and
  `TestRouterSelectEndpointAllInCooldown`. The name outlived the symbol for
  several rounds and sent readers looking for something that no longer existed.

• `handleCompletion` (was api.go:330-378) — handleCompletion never calls RecordSuccess. Non-stream
  cooldowns are sticky: a model escalates on failures and never clears until
  a 4xx/5xx triggers another ApplyCooldown.
  Status: FIXED. handleCompletion calls g.router.RecordSuccess(ep) on 200.

• `ChatCompletionMessage.UnmarshalJSON` (was api.go:41-59) — Vision array-of-parts content is stored as the literal
  string "[{...}]". estimateTokens then over-counts and requestTimeout
  returns a much-too-large budget for vision requests.
  Status: FIXED. ChatCompletionMessage.UnmarshalJSON (api.go:60-97)
  extracts concatenated text from array parts; estimateTokens uses that.

• `checkAuth` (was api.go:145-158) — checkAuth returns true when g.gatewayAPIKey == "".
  If the operator never sets GATEWAY_API_KEY/-api-key, every endpoint
  (including admin) is unauthenticated.
  Status: FIXED (2026-09-20). checkAuth now fails CLOSED unless
  allowNoAuth is set. main.go refuses to start without a key unless
  -allow-no-auth is passed. The opt-in unauthenticated mode is
  documented and tested (TestGatewayNoAuth).

• request body read in the handlers (was api.go:237) — io.ReadAll(r.Body) with no http.MaxBytesReader.
  Memory-exhaustion DoS.
  Status: FIXED. HandleChatCompletions wraps r.Body with
  http.MaxBytesReader(w, r.Body, maxRequestBytes) (api.go:414).

• `saveCooldowns` (was router.go:69-88) — cooldowns.json is rewritten in place under the global
  write lock. Crash mid-write truncates the file; on next start
  loadCooldowns silently drops all state.
  Status: FIXED. saveCooldowns (router.go:310-358) uses atomic
  tmp+rename; temp file removed on any error.

• `watchConfig` vs `HandleAdminConfig` (was main.go:99-126 + api.go:560-593) — Watcher and admin-POST race on the
  same file (double reload, no fsync, no atomic rename).
  Status: FIXED. SaveConfig uses atomic tmp+rename. ReloadConfig is
  atomic (shared pointer). watchConfig uses mtime+size polling.

---

MEDIUM

• `firstByteReader` (was proxy.go:231-251) — firstByteReader leaks one goroutine per timed-out
  first byte.
  Status: FIXED. firstByteReader.Read closes the underlying reader on
  context timeout to unblock the goroutine (proxy.go:342-388).

• `Proxy.streamSSE` — bufio.Scanner is capped at 1 MB.
  Status: FIXED. `scanner.Buffer` set to 16 MB.

• `sseEventIsRelease` returns true on unparseable JSON ("fail open").
  Status: ACCEPTED, not open. Fail-open avoids stalling the stream; a malformed
  upstream event would otherwise hang the client. The trade-off is deliberate
  and is pinned by `TestSSEEventIsRelease_FailOpen`.

• `Proxy.streamSSE` (was proxy.go:344-422) — Final SSE event without a trailing blank line is
  silently dropped.
  Status: FIXED. Post-loop eventBuf flush (proxy.go:741-747) processes
  the trailing event.

• `requestTimeout` (was api.go:266-275 + 321-328) — requestTimeout comment says "1s per extra
  10000 tokens"; code does 2s per 5000.
  Status: FIXED. Comment and code both reflect 1s per 10000 tokens
  (api.go:584-595).

• `requestHasVision` (was api.go:278-283) — requestHasVision does bytes.Contains(body, "image_url").
  Status: FIXED. Checks for the canonical "type":"image_url" marker
  (api.go:441-443).

• proxy.go streamSSE — the event that triggers the release was written
  *after* the buffered pre-release prefix.
  Status: FIXED. `sseEventIsRelease` only becomes true for an event carrying
  content or a tool call, so the held prefix is exactly the role-only /
  reasoning-only opener. processEvent set `released = true` and then tested
  `if released`, so the releasing event jumped the queue and the client got
  `role: "assistant"` *after* its first content token. That inverts SSE delta
  ordering and reads to clients as two separate responses. Now
  `flushBuffered()` runs before the releasing event is forwarded; pinned by
  TestStreamSSE_BuffersPreReleaseEventsUntilReleasePoint for both
  toolCalls modes.

• `Proxy.streamIdleTimeout` = 60s is hard-coded.
  Status: ACCEPTED, not open. The default is 60 s and is overridable via
  `Proxy.SetStreamIdleTimeout`; an earlier version of this doc claimed no test
  exercised the override, which was stale — `TestStreamSSE_IdleTimeoutClosesReader`
  and the `SetStreamIdleTimeout` validation tests (rejecting 0 and negative
  values) both do.

• non-stream header copy in `handleCompletion` (was api.go:367-376) — Non-stream path copies upstream Content-Encoding/
  Content-Length to the client.
  Status: FIXED. isHopByHopHeader (api.go:562-569) strips both.

• `time.After` in the fallback loops (was api.go:343, api.go:404) — time.After in select is not stopped on
  ctx.Done().
  Status: FIXED. Uses time.NewTimer with explicit Stop() on
  ctx.Done() in both handleStream (api.go:837-845) and handleCompletion
  (api.go:657-665).

• `baseCooldownForError` (was router.go:259-274) — 404/401/403 all get 300 s base cooldown.
  Status: FIXED. baseCooldownForError (router.go:690-709) gives 404 →
  7 days, 401/403 → 30 min, 429/5xx → 30 s with bounded escalation.

• `cooldownForError` (was router.go:283-298) — cooldownForError escalates on consecutive failures
  since last success ever, not since last cooldown expiry.
  Status: FIXED. RecordSuccess deletes the cooldown entry, resetting
  ErrorCount. Escalation is bounded by maxCooldown (30 min for
  transient, 7 days hard ban for persistent).

• `minCooldownWait` (was router.go:115-126) — minCooldownWait ignores noVision.
  Status: FIXED. minCooldownWait (router.go:459-476) skips
  noVision/vision:false endpoints for vision requests.

---

LOW (cleanup / hardening)

• unused router symbols (was router.go:25, 36, 387-388) — rrCounters, ErrNoModelsAvailable,
  ErrInvalidModel declared but unused.
  Status: FIXED. Removed; `go vet` reports no unused symbols.

• `Router.SelectNext` only used by tests.
  Status: RESOLVED. `SelectNext` was deleted outright, and the two tests that
  had kept the name alive were renamed to match the function they actually
  call. No Go source in the tree references a `SelectNext` symbol any more.

• `HandleHealth` (/health) is unauthenticated.
  Status: ACCEPTED, not open. It returns only `{"status":"ok"}` — no config,
  sessions, cooldowns, provider URLs or keys — so it is safe for load balancers
  and is the documented liveness probe. Contrast with /metrics, which IS
  authenticated (see below), because its series are labelled by provider and
  model.

• main.go — /metrics was implemented but never registered in the mux.
  Status: FIXED. HandleMetrics existed in api.go and the full Metrics
  collector (requests, fallbacks, cooldowns, circuit transitions) was
  written, but no `mux.HandleFunc("/metrics", ...)` existed, so the endpoint
  404'd in every deployment. docs/RUNBOOK.md even claimed the handler was
  "wired into all four request surfaces" — the claim was never verified.
  Lesson: an unwired handler is indistinguishable from a working one until
  something actually scrapes it. The `sse-contract` CI job now scrapes it.

• main.go / api.go — /metrics served with no authentication.
  Status: FIXED. The series are labelled by provider and model
  (`endpoint="provider:model"`) and expose routing behaviour, so an
  unauthenticated scrape let anyone on the network enumerate configured
  upstreams and watch them fail. Now gated behind checkAuth like /admin.
  Pinned by TestHandleMetricsServesAttemptTelemetryWithAuth and by a CI
  assertion that an unauthenticated scrape is refused.

• `checkAuth` (was api.go:157) — API-key comparison via == is not constant-time.
  Status: FIXED. checkAuth uses subtle.ConstantTimeCompare with
  length-equalisation (api.go:203-218).

• `HandleAdminDashboard` (was api.go:595-671) — Admin dashboard served without CSP/X-Frame-Options.
  Status: FIXED. HandleAdminDashboard sets X-Content-Type-Options,
  X-Frame-Options: DENY, and a Content-Security-Policy
  (api.go:1140-1142).

• `summarizeError` (was router.go:223-236) — summarizeError truncates at a byte boundary.
  Status: FIXED. Uses rune-based truncation (router.go:632-638).

• `loadCooldowns` (was router.go:56-58) — Cooldown file parse errors only logged at debug.
  Status: FIXED. loadCooldowns logs at warn level (router.go:298).

• `truncateErr` (was router.go:373-381) — 4 KB upstream error bodies persisted to
  cooldowns.json.
  Status: FIXED. truncateErr limits LastError to 200 runes (router.go:590).

• `watchConfig` (was main.go:99-126) — watchConfig has no shutdown path.
  Status: FIXED. watchConfig selects on ctx.Done() and returns
  (main.go:134-163). main calls cancel() on signal.

• `watchConfig` — Whole-file SHA-256 every 3 s; fsnotify/mtime cheaper.
  Status: ACCEPTED, not open. `watchConfig` uses mtime+size polling and never
  hashes the file, so the SHA-256 cost concern is moot. The remaining 3 s ticker
  latency is a deliberate trade-off: fsnotify would add a platform-dependent
  dependency for a config that changes a few times a month at most, and a
  missed event silently fails to reload where a poll cannot.

---

## Cooldown jitter + Retry-After passthrough (2026-09-28)

This audit round implemented two features that were declared "done" in the
prior session's summary but were actually dead code — the fields existed but
nothing populated or read them. All three are now wired end-to-end with tests.

• `Router.cooldownJitter` (was router.go:185-188) — `Router.cooldownJitter` was declared with a comment
  "It is set via SetCooldownJitter" but no such method existed, and no call
  site ever assigned it. The `if r.cooldownJitter > 0` branch in
  ApplyCooldownForSession was unreachable, so no jitter was ever applied.
  Status: FIXED. Added `Router.SetCooldownJitter` (clamps to [0, 0.25]) and
  `Router.CooldownJitter()`; wired from `Preferences.CooldownJitterFraction()`
  in both `NewRouter` and `ReloadConfig` so a config change takes effect
  without a restart (router.go:191-211, api.go:1157-1174).

• `ProviderError.RetryAfter` (was router.go:961) — `ProviderError.RetryAfter` was declared and read by
  ApplyCooldownFromErrorForSession, but no `ProviderError{...}` literal in the
  codebase ever set it. The upstream Retry-After header was therefore never
  parsed, and the cooldown floor was always 0.
  Status: FIXED. Added `ParseRetryAfter` (router.go:1010-1040) which handles
  both delta-seconds ("120") and HTTP-date forms via http.ParseTime. Wired
  into every ProviderError construction site that has an HTTP response:
  proxy.go:320/326/336, responses_api.go:318/323/328, api.go:790. The
  non-stream path (api.go:790) was updated to pass
  `ParseRetryAfter(resp.Header.Get("Retry-After"))` directly rather than 0.

• `Preferences` (was config.go:124) — `Preferences` had no cooldown_jitter field at all.
  Status: FIXED. Added `CooldownJitter float64` with a
  `CooldownJitterFraction()` accessor that clamps to [0, 0.25].

• main_test.go (new) — TestParseRetryAfter,
  TestApplyCooldownHonorsRetryAfterFloor, TestCooldownJitterAppliedOn429,
  TestApplyCooldownFromErrorCarriesRetryAfter,
  TestPreferencesCooldownJitterFraction,
  TestConfigWiresCooldownJitterToRouter.

## Default max_tokens injection (2026-09-28)

- `Preferences.DefaultMaxTokens *int` (yaml `default_max_tokens`) with three
  distinguishable states via pointer:
  - nil (absent) → **auto**: inject `max_tokens = min_context / 2` for requests
    that omit it.
  - 0 → **never default**: send the request upstream without `max_tokens`,
    letting the provider choose.
  - positive → use that absolute value regardless of profile.
- `defaultMaxTokensForModel` resolves the effective ceiling per profile.
- `injectMaxTokens` mutates the raw request body (not just the parsed
  struct), so the injection reaches every path: non-stream, stream,
  Responses HTTP, and Responses WebSocket. It preserves all other fields
  and ordering, and returns the body unchanged on parse failure.
- The injection happens BEFORE `bodySessionID`, so a client that re-sends
  the same conversation with `max_tokens` toggled still hashes to the same
  session (max_tokens is not part of the fingerprint — see
  `bodySessionID` at api.go:361).
- Tests: `TestDefaultMaxTokensInjectedByHalfContext`,
  `TestDefaultMaxTokensPreservesClientValue`,
  `TestDefaultMaxTokensDisabledWhenExplicitZero`,
  `TestDefaultMaxTokensNoContextSkips`.

## /v1/models advertises a max_context_tokens floor (2026-09-28)

- `ModelEndpoint` gained a `ContextLength int` field (yaml `context_length`),
  parsed from config.yaml and surfaced via `ContextWindow()`.
- `HandleModels` derives each logical model's `max_context_tokens` as the
  **smallest** context window in its fallback chain. This is the
  conservative value that is safe for every backend the profile might
  relay to, including fallbacks after a failure.
- The field is `max_context_tokens` (input capacity), NOT `max_tokens`
  (output ceiling): the upstream model lists publish no output-token data
  for any backend, and neither does OpenAI's own /v1/models schema.
  Advertising an output ceiling we don't have would be a lie.
- `regenerate_config.py` now emits `context_length` as a real YAML field
  (previously it was only a trailing comment, so the Go side never saw
  it). The `large` profile — whose entire purpose is large-context work —
  is the main beneficiary: it now advertises a real floor instead of
  nothing.
- Test: `TestHandleModelsAdvertisesMaxContextTokens` verifies min-of-chain
  semantics, single-endpoint chains, and the absent-field case.

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

  • `cleanupStaleEntries` (was router.go:405 + router.go:1223) — cleanupStaleEntries holds r.mu and calls
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

  • `HandleAdminCooldowns` (was api.go:1137-1178) — HandleAdminCooldowns only exposed cooldown backoff
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
  `go test -race ./...` → 159 passed, 0 failed (2026-09-28)
  Both previously-flaky tests now stable:
  - TestHandleStream_ResourceCleanup — passes in full suite
  - TestConcurrentMidStreamErrorRecovery — fixed to only assert the replay
    invariant for sessions that actually hit backend1 (was asserting it for
    every request, including ones that legitimately skip the cooled backend)
  `go vet ./...` → no issues
  `go build ./...` → success
  `python3 scripts/validate-config.py` → OK: 4 profiles validated
