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

## Metrics registry (2026-09-29)

Round focused on `/metrics`, which had been mounted only a few commits earlier
and had never been exercised against a hostile or merely churning input. Three
defects, all in `metrics.go`, all now fixed and pinned.

• `Metrics.Request` (was metrics.go:183-205) — label values were storage keys
  leaking into the exposition output. The store was keyed by a composite
  (`"requests_by_model_" + model`, `"requests_by_status_" + status`) and that
  composite was interpolated straight into the label, so every series read
  `airouter_requests_total{model="requests_by_model_smart"}`. Same for
  `status="requests_by_status_502"`. The key and the label are now separate
  concerns and the label carries only the model name or status code. This was
  cosmetic in isolation, but it meant every dashboard query, alert rule, and
  recording rule had to hard-code a storage detail that no operator would
  otherwise guess, so the series were effectively undiscoverable
  (TestMetricsModelLabelIsTheBareModelName).

• `boundedAttemptKey` (was metrics.go, unbounded before this round) — the
  per-endpoint attempt maps had no cardinality bound at all. Label values come
  from config, and config is not fixed for the process lifetime:
  `HandleAdminConfig` accepts an arbitrary new config and `watchConfig`
  hot-reloads the file on a 3s poll. A config churn loop — or a nightly
  free-model sync that keeps introducing fresh model ids — therefore fed
  genuinely unbounded values into `attemptsByEndpoint`,
  `attemptFailuresByEnd`, `attemptLatencyNSByEnd`, and `attemptHistByEnd`.
  Nothing evicted a series, and each endpoint cost four counters plus a
  12-bucket histogram, so the process grew until it was OOM-killed: a metrics
  endpoint that can be used to kill the gateway is a self-inflicted DoS.
  Measured before the fix: 5000 distinct labels produced 5000 entries and a
  6.9 MB scrape response. FIXED: every label map is capped at
  `defaultMaxLabelValues` (512, ~8x the 60 endpoint keys in the live config)
  and unknown values
  collapse into a single `__overflow__` bucket. Collapsed observations are
  still counted, and `airouter_metrics_label_overflow_total` makes the
  condition visible instead of silent (TestMetricsAttemptCardinalityIsBounded,
  TestMetricsCardinalityBoundedAcrossAllLabelMaps,
  TestMetricsCardinalityBoundIsRaceFree,
  TestHandleMetricsServesBoundedOutputEndToEnd).

  Two implementation notes worth keeping, because both were bugs found by the
  tests rather than by review. Bounding each map independently inside
  `lazyCounter` was wrong twice over: the bounded value never propagated back to
  the caller, so the histogram map was still keyed by the *raw* label and grew
  to 6400 entries while the counter maps stopped at 512; and separate checks
  let the maps disagree at the boundary, so an endpoint could have attempt
  counts with no latency histogram. Deciding once in `boundedAttemptKey` and
  threading that key through all four maps makes the key sets identical by
  construction. Separately, the first version let the map reach `cap+1` by
  admitting `__overflow__` on top of `cap` real endpoints — a limit that is not
  actually the limit is worse than no limit, because it reads as a guarantee it
  does not provide, so one slot is now reserved for the bucket.

• `labelEscape` (was metrics.go, no escaping before this round) — every label
  value was interpolated into a `"..."` quoted string with no escaping. The
  provider and model names are config-derived and config is writable through
  `HandleAdminConfig` and rewritten by the model-sync job, so a provider named
  `x" 1\nairouter_requests_total{model="victim` produced two syntactically valid
  series on the next scrape, both of which an operator or a downstream scraper
  reads as genuine. The exposition format requires escaping backslash, double
  quote, and newline. FIXED: `labelEscape` is applied at every interpolation
  site, and `parseExposition` in `metrics_cardinality_test.go` asserts the
  output still parses as one series per line with exactly one model-labelled
  request series (TestMetricsLabelValuesCannotForgeSeries). CI re-checks this
  against the booted gateway, and the check was verified to reject both an
  unescaped forgery and a raw-newline injection, so it is not decorative.

  While fixing this, `go vet` caught a latent conversion bug: `CircuitState` is
  an int-backed enum, so `string(from)` emits the rune for the state number
  (`"\x00"`) instead of `closed`/`open`/`half-open`. Fixed to use
  `CircuitState.String`, and pinned by
  TestMetricsCircuitTransitionLabelsAreReadable.

### Round 2 (2026-09-29) — eviction, configurability, and the malformed-sample bug

The bound from round 1 was correct but incomplete, and the incompleteness was
worse than the bug it fixed: it held the line, but it starved.

• `boundLabelValue` (was metrics.go, first-come admission) — the cap admitted
  the first 512 distinct labels and then refused everything else, permanently.
  Because config is hot-reloadable, labels belonging to a config that is no
  longer live hold their slots for the life of the process. A newly hot
  endpoint appearing afterwards is collapsed into `__overflow__` forever, and
  the gateway reports a broken upstream as "no data" indefinitely — the single
  worst failure mode for the series whose entire job is answering "is this
  upstream healthy?". FIXED: `attemptsByEndpoint` now recycles. An untracked
  label arriving at a full window evicts the least-recently-admitted tracked
  endpoint via `rollUpToOverflowLocked` and makes room. Eviction is FIFO by
  admission, not LRU: re-touching an endpoint does not refresh its position,
  because doing so would take the global write lock on every observation to
  protect a distinction that only matters under sustained churn.

  Preserving the totals through an eviction is the part that is easy to get
  quietly wrong, and it was wrong three times on the way:
    - attempt counts, failure counts and latency sums must roll into the
      bucket, or a churn loop quietly deletes history;
    - the rollup read-then-maybe-wrote, so the *first* label of a kind dropped
      its failure and latency history on the floor — which is every label that
      failed on its only attempt, i.e. precisely the labels worth knowing about;
    - the bucket was originally created in `attemptsByEndpoint` alone, giving a
      series with a count and no histogram, whose `attempt_duration_seconds`
      printed a zeroed distribution.
  Per-bucket *histogram* data is deliberately not merged, and cannot honestly
  be: after the endpoint is gone, "the 0.25s band held 40 observations, all
  from a label we evicted" is not representable. `__overflow__`'s `_count` can
  therefore sit below what its `_sum` implies. That is the design, and the
  RUNBOOK says so, so nobody later "fixes" it.

• `Attempt` (was metrics.go, admission and update under separate locks) —
  admission reserved a slot under one critical section and updated the counters
  under another. A concurrent eviction could remove the key in between, after
  which the in-flight observation recreated it — pushing the map one over the
  cap and double-counting. Admission and update are now one atomic operation
  under a single write lock. The bucket is also pinned *outside* the recycling
  ring: it was briefly added to the ring and skipped only when picking a victim,
  which reported one real eviction as two.

• `ServeHTTP` (was metrics.go, raw `%s` of a pre-rendered label set) — the
  cardinality bound had a failure mode that was strictly worse than the one it
  fixed. The maps bound labels in two shapes: `attemptsByEndpoint` keys on a
  bare value, but `circuitTransitions` keys on a pre-rendered label set
  (`from="..",to="..",endpoint=".."`) that the exporter splices in unquoted.
  Its overflow key is therefore the bare word `__overflow__`, sitting where a
  label set belongs, and the output line is
  `airouter_circuit_state_transitions{__overflow__} 4` — not a legal sample. A
  Prometheus scrape is all-or-nothing, so hitting the cap would have taken down
  every dashboard, alert and recording rule on the instance. The bound on
  cardinality is worthless if the price of reaching it is losing the metrics.
  FIXED: the bucket renders as `{endpoint="__overflow__"}`. Every earlier test
  passed while this was broken, because they asserted on map contents and
  substrings rather than parsing the output;
  `TestMetricsOverflowNeverEmitsMalformedSamples` now forces the overflow
  condition on all five bounded maps and asserts the *parsed* output, including
  that every label name is a legal Prometheus identifier. The test was verified
  to fail on the unfixed code with exactly the malformed line above.

• `Config.validate` (was config.go, nil deref) — the range check for
  `preferences.max_label_cardinality` dereferenced `c.Preferences` directly.
  `Preferences` is a pointer, the key is `omitempty`, and a config without a
  `preferences:` block is perfectly valid, so the check panicked on the most
  ordinary config there is and took the whole suite down with it. Every other
  preference accessor in the file is nil-safe; this one now is too.

• `labelEscape` (was metrics.go, hand-picked cases only) — the injection guard
  was verified by a table of nasty strings, which is the shape of bug that
  ships: the test passes, the reviewer concludes the escaper is sound, and the
  untested case is the one a config author types next. `FuzzExpositionRoundTrip`
  now asserts the property the guard actually rests on — for *any* label value,
  what the escaper writes is what `parseExposition` reads back, and the result
  is still exactly one well-formed sample — driving the parser over the whole
  line so the brace/quote/escape state machine is explored for panics and
  non-termination too. 174M executions, no counterexample.

The cap is now configurable rather than a constant, because a memory bound that
cannot be raised is a bug report waiting to happen: `Preferences.MaxLabelCardinality`
→ `MaxLabelCardinalityValue` → `SetLabelCap`, applied at construction and again
on every `ReloadConfig` (the collector is built once and outlives every reload,
so a change applied only at construction would silently no-op until restart).
Out-of-range values are **rejected, not clamped** — below the floor destroys
the series the cap exists to protect, above the ceiling defeats the memory
bound, and a setting that silently does something other than what it says is
worse than a startup failure. `scripts/validate-config.py` enforces the same
bounds before a restart, so the rule is implemented twice; the duplication is
the point (config is checked before and after a restart) and
`TestValidateConfigRangeMatchesGo` parses the script's source to assert the two
constants agree, so they cannot drift apart silently. It was verified to fail
when the script's ceiling is changed alone.

### Round 3 (2026-09-29) — the bug class, and two defects the new tests found

Round 2's fix was a special case in one exporter. That is the wrong shape of fix
for a defect that comes from a *convention*, so this round removed the
convention.

• `labelKey` (was metrics.go, two coexisting key conventions) — the maps did not
  agree on what a label value is. `attemptsByModel`, `fallbacksByFromEndpoint`
  and `attemptsByEndpoint` keyed on a BARE value and escaped it at export;
  `circuitTransitions` keyed on a PRE-RENDERED label set
  (`from="..",to="..",endpoint=".."`) that the exporter spliced in raw. The
  second shape means the map key is a whole label SET, so the overflow bucket —
  a single bare value — no longer fits the key space. Nothing recorded which
  convention applied where, so a new map would pick one at random and the only
  symptom would be a scrape that fails to parse, in production, from the cap
  that exists to make things safer. FIXED: every bounded map keys on a
  `labelKey` (the exact bytes between the braces), built by `singleLabelKey` /
  `circuitTransitionKey` and written by `writeSample`. A bare value becomes a
  one-label set, so the bucket is a legal key everywhere and the exporter has no
  special case left to forget.

  The general part is `TestEveryBoundedMapHasAWellFormedKeyShape`: one table
  asserting, for all six maps, that every key parses as a label set, round-trips
  to the value that produced it, and that each has a well-formed bucket. Adding
  a map without adding a row fails the test, so "which maps are bounded" stops
  being something to remember. Writing it immediately found:

• `counterStatus` / `counterFailure` (was metrics.go, unbounded) — these two
  were the only maps with a cap, a comment claiming they were bounded, and no
  call enforcing it. The status is `resp.StatusCode` forwarded from the UPSTREAM
  response, so the value set is whatever a remote server returns; an upstream
  that varies its status per request drives unbounded labels into both maps.
  Measured with the guard removed: 640 distinct labels against a cap of 16. This
  is the same self-inflicted DoS the rest of the file exists to prevent, still
  open, and it survived two rounds of cardinality work because every prior test
  asserted on endpoints and models. FIXED, pinned by TestStatusMapsAreBounded
  (verified to fail without it).

• `admitCollapsible` (was metrics.go, check-then-insert across two locks) — all
  five collapsing counters decided the key under a read lock, released it, then
  inserted under a write lock. N goroutines with N new labels could all observe
  "there is room" and all insert, so the map reached cap+N. The memory bound
  silently did not apply under exactly the load that generates the most labels.
  Same defect and same fix as `Attempt` had: one write lock for re-check,
  decision and insert, with a read-lock fast path for already-tracked keys.
  `TestConcurrentDistinctLabelsNeverExceedTheCap` samples len() *while* workers
  run, because the overshoot is what remains at the end and an after-the-fact
  assertion passes against this bug. (The first version of that watcher read
  len() without the lock; the race detector caught it, which is the test earning
  its keep before it ever failed on the product code.)

• `bareKey` (was metrics.go, `%q` on an already-escaped value) — keys are
  escaped once when built and written verbatim at export, and `%q` is *Go* string
  quoting, which re-escapes the backslashes. Every label containing a quote or
  newline came out double-escaped: still valid, still safe, still parsing, but
  no longer matching the configured value, so the series could never be joined
  to config.yaml. Silent data corruption that every existing check passed,
  including the round-trip fuzz target — a double escape unescapes to the
  original. Fixed by concatenating the quotes by hand.

  This is where the "looks about right" heuristic earned its keep by failing:
  asserting no `"` in the output flagged every correct seed, and the fuzzer then
  produced `\\\"` as a false positive. Replaced with `referenceEscape`, an
  independent naive escaper in the test file that `labelEscape` is compared
  against byte for byte. An oracle that shares code or early-exit logic with the
  thing under test can only confirm the bugs they have in common.

• `overflowKeyFor` (was metrics.go, one global bucket key) — the collapsing
  maps all collapsed into `endpoint="__overflow__"`, including the status maps.
  That silently changes a metric's label schema, and Prometheus reads a
  label-set change as a new series: a query grouping by status would lose the
  bucket entirely, and an alert on a status series would stop covering it. The
  cap is a safety valve and must not reshape healthy data. Each map now collapses
  under its own label name — `status="__overflow__"`, `model="__overflow__"`.

### Round 4 (2026-09-29) — the overflow bucket was an invalid histogram

Rounds 1-3 made the bound *hold*. This round asked a question none of them did:
what does the output actually say, and is it true? It was not.

• `serveAttemptMetrics` (was metrics.go, overflow bucket emitted as a histogram)
  — round 2 established that per-band histogram data cannot be rolled into the
  overflow bucket, and recorded the consequence as "`__overflow__`'s `_count` can
  sit below what its `_sum` implies. That is the design." That was a wrong
  conclusion reached from a correct observation, and it shipped. The bucket was
  still exported as a full histogram, with a zeroed band distribution:

  ```text
  airouter_endpoint_attempt_duration_seconds_bucket{endpoint="__overflow__",le="0.25"} 0
  airouter_endpoint_attempt_duration_seconds_bucket{endpoint="__overflow__",le="45"}   0
  airouter_endpoint_attempt_duration_seconds_bucket{endpoint="__overflow__",le="+Inf"} 11
  airouter_endpoint_attempt_duration_seconds_sum{endpoint="__overflow__"} 0.110000
  airouter_endpoint_attempt_duration_seconds_count{endpoint="__overflow__"} 11
  ```

  Every finite bucket is 0 and `+Inf` is 11, so the series claims all 11
  observations exceeded 45 seconds while its own `_sum` says they averaged 10ms.
  A Prometheus histogram is cumulative, so finite bounds must be `<= +Inf`; this
  one is not. It is not a histogram, it is a lie with a legal-looking name.

  The severity is the part that matters. Nothing rejects it: the scrape parses,
  the family declares `histogram`, every dashboard renders, no alert fires, and
  `histogram_quantile` returns a plausible number with no relationship to
  reality. A silent wrong answer is strictly worse than an error, and this only
  appears once the cap is crossed — the healthy path is fine, which is why
  every prior test, all of which ran below or just past the cap, passed.

  FIXED: the overflow label gets **no** histogram series at all. Its count and
  latency sum are published as gauges,
  `airouter_evicted_attempts_latency_seconds_{sum,count}`, named so that no
  quantile function will accept them by accident and emitted for every endpoint
  label (zero for live ones) so the series neither appears and disappears at the
  cap nor needs a special case in a "total minus live" query. They are gauges
  and not counters because an eviction *adds* to them, and a `rate()` over a
  series that can decrease is worse than no series.

  The lesson generalises past this bug: a bound that fires only under stress
  needs a test that forces the stress. "Parse succeeds" was the wrong oracle
  here — the corrupt output parsed perfectly. `TestOverflowBucketIsNotExposedAsHistogram`
  asserts the conservation identity instead
  (`live_histogram_counts + gauge_count == total_attempts`, likewise for the sum),
  which is the property that must hold for any cap and any label mix, and which
  fails both if the bucket is histogrammed again (83 vs 49, double-counted) and
  if the gauges go missing (unreported rather than honestly reported). It also
  asserts a live label still has a real histogram, so the fix cannot pass by
  breaking histograms everywhere.

  The first draft of the fix used `break` where `continue` was meant, which
  silently stopped the endpoint loop at the bucket and dropped every later
  sample. The test caught it; the shape of the mistake is worth recording
  because the dropped samples produce no error either.

• `TestEveryLabelKeyMapIsBoundedAndRenders` (was metrics_cardinality_test.go,
  hand-listed structural coverage) — round 3's structural test named six maps,
  and there are eight. A hand-maintained list of the things a test is supposed
  to cover is a list that drifts, and this one had already drifted before it
  ran once. Replaced with a reflection walk over every
  `map[labelKey]*atomic.Int64` field in `Metrics`, so a new map is covered the
  moment it is declared. Verified to have teeth by adding a ninth, deliberately
  unbounded map and confirming the test failed.

  Note the reflection goes through `reflect.ValueOf(m).Elem()`, not
  `reflect.ValueOf(*m)`: `Metrics` embeds a mutex, and copying it trips
  `go vet`'s copylocks check.

• `referenceEscape` (was metrics_cardinality_test.go, compared against itself) —
  round 3 introduced an independent escaper as the fuzz oracle, which is only an
  oracle if it is pinned. Both were free to drift together, or the escaper
  could be "fixed" to match a regression. `TestReferenceEscapeMatchesPrometheusRules`
  now pins it against fixed vectors — quote, backslash, backslash-then-quote,
  newline, CR, tab, NUL, UTF-8, empty, and a realistic injection attempt —
  compared byte for byte, because a substring heuristic for double-escaping
  produced false positives on the entirely legitimate input `\"`. Verified to
  fail when the oracle's backslash handling is removed.

• `export-unpushed.sh` (was scripts/, unfetchable bundle) — the recovery
  instructions the script printed could not be followed.
  `git bundle create A..HEAD` records the tip as the ref `HEAD`, and
  `git fetch <bundle> 'HEAD:refs/heads/fb'` fails with `couldn't find remote
  ref refs/heads/HEAD`. The bundle verifies, is non-empty, and cannot be
  fetched. This is the same failure shape as the histogram above: a check that
  confirms the artifact *exists* rather than confirming the documented
  recovery actually works. FIXED: the script creates a temporary
  `refs/heads/airouter-unpushed-export`, bundles that, and deletes it on exit,
  so the printed `git fetch` has a ref it can resolve. The `unpushed-export` CI
  job now runs the script's own instructions and counts the recovered commits.

  Two further notes on that job. Its upstream is a dedicated `base` ref rather
  than `origin/master`, because on a pull_request event HEAD is a merge commit
  and the job's result would depend on the event type. And its no-op assertion
  deletes the generated artifacts first: they are build output, so asserting
  "no bundle exists after the run" fails the moment anyone runs the script
  locally before pushing. The property under test is "this run created
  nothing", not "no artifact has ever existed".

### Round 5 (2026-09-29) — generalising the histogram guard, and the invariant it was missing

Round 4 fixed the overflow bucket. The test that pinned it named that one
family, which is the same shape of gap that let the bug survive rounds 1-3: a
well-formedness check written against the specific series it was thinking of.
Nothing would have caught the identical defect in the request-duration
histogram, or in any histogram added later.

• `checkHistogramInvariants` (was metrics_cardinality_test.go, no general
  guard) — the exposition is now walked in full and every family, every label
  set, is checked against the spec rather than against a hand-picked list. Six
  invariants: `le` present, strictly increasing, exactly one `+Inf` and it
  last, cumulative counts non-decreasing, `+Inf` equal to `_count`, and — the
  one added last and the one that matters — **buckets consistent with `_sum`**.

  That sixth invariant exists because of how round 4's negative control came
  out. The obvious well-formedness checks are all monotonicity checks over
  *consecutive* buckets, and the real bug was:

  ```text
  le=0.25 ... le=45   all 0
  le=+Inf             11
  _sum                0.110000
  _count              11
  ```

  Every consecutive pair is non-decreasing. `+Inf` agrees with `_count`. A
  guard built from the obvious invariants passes this output cleanly, and the
  series is still a lie: the buckets say all 11 observations exceeded 45
  seconds while the sum says they averaged 10ms. The buckets and the sum
  describe different worlds, and *that* is the defect — not a decrease.

  The invariant that catches it: if `above` observations are attributed to the
  open band above the top finite bound, each of them exceeded that bound, so
  `_sum >= above * topBound`. Here that demands 11 × 45 = 495 and is given
  0.11. This is only implied by monotonicity when the buckets are populated;
  the overflow bucket is exactly the case where they cannot be, which is why
  the bug produced zeroed bands in the first place. Written as a strict `<`
  comparison so a missing or zero `_sum` is not flagged.

  The lesson is that a spec-compliance checklist is not the same as an
  invariant set, and the difference only shows up when you try to violate
  each one deliberately. A guard that has only ever been run against correct
  output has an unknown set of untested branches, and those branches are
  exactly where a latent detector hides.

• `TestEachHistogramInvariantHasTeeth` (was metrics_cardinality_test.go,
  guard verified only in the accepting direction) — feeds
  `checkHistogramInvariants` one deliberately broken exposition per invariant
  and requires each to be reported. The checker returns strings rather than
  calling `t.Error` specifically so this is possible.

  Two of the seven cases failed when first written, and both failures were
  informative rather than annoying:
    - the "zeroed finite buckets" case reported nothing, which is how the
      missing `_sum` invariant above was found;
    - the "`+Inf` is not last" case was a no-op mutation — it moved `+Inf` to
      the end, where it already was — so it passed vacuously while looking
      like coverage. A mutation that does not mutate is the most expensive kind
      of useless test, because it is indistinguishable from a real one in
      review.
  The baseline fixture was also wrong before it was right: its first `_sum` was
  inconsistent with its own bucket layout, so the "clean" input was not clean.
  Fixtures used as negative controls have to satisfy the invariants they are
  not testing, or every case passes for the wrong reason.

### Round 6 (2026-09-29) — hardening the guard itself

Round 5 added a general histogram check and found, in the act of writing its
own negative controls, that the obvious invariants did not catch the bug the
round was about. This round finishes that job: it pins the threshold choice,
removes the test class that already produced one false pass, and stops relying
on hand-picked cases alone.

• `checkHistogramInvariants` (was metrics_cardinality_test.go, threshold
  unexplained and its guards untested) — invariant (6)'s threshold,
  `_sum >= above * topBound`, was written without recording *why* it is that
  bound rather than some other. The comment now states it: `topBound` is a
  lower bound and never an equality, because the top finite band is
  `(topBound, +Inf]` and observations just above the bound contribute almost
  nothing. That makes the check deliberately loose — it catches the categorical
  case where buckets and sum differ by orders of magnitude, and will not catch
  a mildly skewed distribution, which cannot be caught at all from exposition
  text because the observations are not in it. Choosing a tighter bound that
  "looks stricter" would be a false precision.
  `TestHistogramInvariantSixBoundaries` pins all four guards (`sum > 0`,
  `topBound > 0`, `above > 0`, strict `<`) and both equality boundaries, so
  the behaviour is asserted rather than assumed. Verified to fail when the
  comparison is loosened to `<=`, which is what makes the "equality is legal"
  case a fact rather than a hope.

• `seriesFingerprint` (was metrics_cardinality_test.go, no-op mutations) — one
  case in round 5's table was a no-op from the start: it moved `+Inf` to the
  end of the slice, where it already was, so the guard correctly reported
  nothing and the case still read as coverage. Every case now compares a
  canonical fingerprint of the parsed input before and after its mutation and
  fails if they are equal. The fingerprint preserves sample *order*, not just
  the set, because "is `+Inf` last" is precisely a reordering and a set-based
  comparison would call it unchanged. Verified by restoring the original no-op:
  the detector reports "changed nothing; this case cannot fail and is not a
  test", which is the failure a reviewer cannot see by reading the table.

• `FuzzHistogramInvariantsSurviveGarbage` (was metrics_cardinality_test.go,
  seven hand-written cases) — seven hand-picked cases are still seven
  hand-picked cases, and hand-picked cases are how the original defect survived
  three rounds. The checker is now driven by arbitrary exposition text and
  asserted to never panic, never hang, and never emit an empty or unanchored
  violation. Nine seeds, including the real pre-fix output verbatim, a
  single-bucket family, a `_count` that disagrees, and malformed input. 385M
  executions, no counterexample. The working corpus is gitignored; only
  minimized crashers under `testdata/fuzz` would be committed.

• `dist/` bundle (was stale, one commit behind) — the exported bundle still
  pointed at `a861dd5` while HEAD was `4c03ed2`, so the artifact a colleague
  would have been handed did not contain the two rounds of fixes. Regenerated,
  and then verified the way it would actually be used: a fresh clone at
  `origin/master`, a single `git fetch` of the bundle, then `go build` and the
  full test suite from the recovered tree. The script's own printed recovery
  instructions are the only test that would have caught a bundle which verifies
  and is unfetchable or incomplete, which is exactly the bug round 4 found in
  it.

### Round 7 (2026-09-29) — making the guard's coverage durable

Round 6 built a fuzzer and ran it once for 385M executions. A one-off run on
one machine is not coverage: it says nothing about the next commit, and the
corpus it built is gitignored. This round makes the coverage stand on its own.

• `TestGoldenExpositions` + `testdata/metrics/` (was metrics_cardinality_test.go,
  fuzz seeds in a gitignored cache) — the single most valuable input here is a
  specific historical output that no fuzzer is likely to rediscover, and it is
  precisely the one that survived three audit rounds. It is now committed as
  `overflow-as-histogram.prom`, transcribed from a real pre-fix scrape, and
  asserted by a deterministic test. Both directions are pinned: the guard must
  reject it *and* must accept `valid-histograms.prom`, which is 256 samples of
  genuine output captured from a running gateway under load with 17 evictions
  and the `__overflow__` gauges present. A guard that only ever sees the bad
  case is a guard that also fires on the good one, and would be disabled on
  first contact with production. Verified to fail when the bad fixture is
  edited into a valid histogram.

• `FuzzHistogramInvariantsSurviveGarbage` (was metrics_cardinality_test.go,
  run once by hand) — now two CI steps. One replays the seed corpus explicitly
  rather than relying on `go test ./...` sweeping it up incidentally, so the
  dependency is visible and a renamed target fails loudly instead of silently
  dropping coverage. One runs a bounded 20s fuzz, enough to rediscover a
  crasher on any PR without slowing a commit down.

• `testdata/fuzz` (was crasher corpus discoverable only by remembering to look)
  — Go already writes a crashing input to `testdata/fuzz/<Target>/`, which
  makes it replay as an ordinary part of the corpus on every later run. That
  only works if it is committed, so CI now fails if a crasher appears in the
  working tree. A crasher that exists only in a CI log is a regression nobody
  can reproduce.

• Invariant (6)'s threshold, measured rather than argued. Round 6 claimed a
  tighter bound would be "false precision" without testing it, which is an
  argument, not evidence. Two measurements:

  - Sweeping the multiplier over the real-output fixture: `sum >= k * above *
    topBound` produces **zero** violations all the way to `k = 100`. Genuine
    gateway traffic satisfies the loose bound with an enormous margin, because
    the observations in the top band are not sitting just above it.
  - Comparing `k=1` against `k=2` over the full corpus (both fixtures plus all
    nine fuzz seeds): the tighter bound finds **zero** additional hits.

  So the loose bound loses no detection power on anything known, and the
  measurement is stronger than the original claim rather than merely
  consistent with it. The bound is still left at `k=1`: it is the tightest one
  that is *sound by construction* rather than tuned, so it cannot become a
  false positive when a future deployment legitimately clusters observations
  just above a bucket edge — a case no current fixture covers but which real
  traffic will eventually produce. Recorded here because "we measured it and it
  did not help" is a result; "we did not measure it" would not have been.

### Round 8 (2026-09-29) — the gate that was never run, and the measurement that measured nothing

Round 7 added `scripts/fuzz-gate.sh` and wired three hand-named CI steps to it.
Auditing that work found the script was never actually executed as documented,
and that two of its checks could not distinguish the case they were written for
from the case they were not.

**`FROZEN=1` was a trap, and the documented invocation was the broken one.** The
usage line says `FROZEN=1 scripts/fuzz-gate.sh`, but `FROZEN=1` is a *shell*
assignment, not a shell word: it sets a variable and passes nothing. The
universal form — `FROZEN=1 ./fuzz-gate.sh` — binds it to `$1`, so the string
`FROZEN` became `SECONDS_PER_TARGET`, and the script then reported:

    scripts/fuzz-gate.sh: line 114: [: FROZEN: integer expected
    --- fuzzing FuzzExpositionRoundTrip for FROZENs
    invalid value "FROZENs" for flag -test.fuzztime: invalid duration
    ::error::FuzzExpositionRoundTrip found a counterexample.

A usage error was reported as a fuzz counterexample, pointing the reader at
`testdata/fuzz/FuzzExpositionRoundTrip/` for a crasher that does not exist, and
exiting 1. The one invocation a reader is told to use was the one that fails, and
it fails by fabricating a bug. A typo (`20x`) took the same path. Now the
duration is validated as a non-negative integer before it reaches `-fuzztime`,
`FROZEN` is read only from the environment, and `FROZEN=1` combined with an
explicit duration is a usage error rather than a silent override. Verified: the
documented form now exits 0, and `abc` exits 2 with a usage message instead of a
fake crasher.

**A committed corpus made the gate permanently red.** `check_uncommitted_crashers`
ended with `[ "$n" -gt 0 ] && echo ...` as the last statement of the function, so
the function returned the *predicate's* status. With no `testdata/fuzz` directory
yet — the normal state before any crasher exists — the directory is absent, `n`
is 0, and the function returned 1. Both call sites read non-zero as "a crasher
was lost". So the gate failed on a clean checkout with an error message naming a
file that did not exist. This is the same class of bug as the one the function
was written to prevent, caused by the fix. The informational `echo` no longer
sets the return value. Verified both directions: a committed corpus file exits
0 ("1 corpus file(s), all tracked"), an uncommitted one exits 1 and names it.

**Discovery was root-only.** `for f in $(ls *_test.go)` finds the two targets
that happen to live in the repository root and silently misses every target
added under a subdirectory — the same coverage-that-looks-like-coverage failure
as the hardcoded list it replaced, except it also survives auditing the list.
Discovery now walks every package directory via `git ls-files -z`, with each
path passed to `sed` as one quoted argument (the unquoted `$(ls ...)` would split
on whitespace in a filename). Since `git ls-files` skips untracked files, a
cross-check compares the on-disk `*_test.go` set against the tracked one and
fails loudly on a difference, so an untracked test file cannot drop its fuzz
targets off the gate with no signal. Verified: planting an untracked
`*_test.go` exits 1 and names the file.

**The threshold study was measuring a code path that never executes.** Round 7's
headroom claim — "zero violations at k=100 on real output" — was true and
vacuous. On `valid-histograms.prom`, all 16 histogram label sets short-circuit
invariant (6) on `above <= 0`: every observation lands in the top *finite*
bucket, so `+Inf - topBound == 0` and the comparison is never reached. The
sweep could have been run with the guard's arithmetic deleted and it would have
reported the same thing. `TestInvariantSixThresholdHasNoHeadroom` now asserts
eligibility first (`invariantSixEligibility`) and fails if no label set in the
corpus reaches the comparison, and it reports the tightest `k` that would reject
the real fixture — currently "no k", i.e. the honest answer, logged rather than
asserted because it is a property of the traffic rather than of the code. The
load-bearing part of the claim (k=1 vs k=2 over the whole corpus) is unaffected
and still holds: 7 label sets reach the comparison, k=1 and k=2 both fire on 6
of 15 inputs.

**The equivalence check agreed with production on one fixture.** It now runs
over every golden fixture plus every fuzz seed, and asserts per-input equality
rather than an aggregate, so a helper that fires once too often on one series and
once too rarely on another cannot report a matching total. It also fails if
*neither* side fires anywhere, since two functions that both return zero
everywhere are trivially equal.

**A corpus that could not tell a correct formula from one missing a factor.**
Every other input fires by such a margin (the overflow fixture's sum is 0.11
against a minimum of 495) that dropping the `topBound` term from invariant (6)
still rejects it — the equivalence check was passing for the wrong reason. A seed
was added whose verdict depends on the `topBound` term itself: 1 observation
above `le="45"` with `_sum` 40, which is impossible with the term and looks fine
at 40 against a bound of 1*1 without it. Dropping `topBound` from both the
production guard and the helper now fails the test.

**Documentation that overstated its evidence.** The `valid-histograms.prom`
header claimed "17 label evictions", but the
`airouter_metrics_label_{overflow,evictions}_total` counters that would report
that are declared in the fixture and carry no sample line, even though
`metrics.go:978,988` emit them unconditionally — the capture is missing them. The
header now states what the file actually shows (15 per-endpoint label sets, with
`fake-fast-1` evicted), and `testdata/metrics/README.md` records the discrepancy
rather than papering over it. The real eviction evidence is the missing
`fake-fast-1` label and the zeroed `*_evicted_attempts_latency_*` gauges, not
the counter.

Verification for this round:

  `go test -race -count=1 ./...` → ok, 0 failed (2026-09-29)
  `gofmt -l .` → clean; `go vet ./...` → no issues; `go build ./...` → success
  `python3 scripts/sse-check.py` → 4/4 streams satisfy the termination contract
  `scripts/sse-negative-check.sh` → PASS (guard demonstrably has teeth; proxy.go
    byte-identical to HEAD afterwards)
  `scripts/secret-scan.sh` → clean (tracked content, history, .git/config)
  `scripts/audit-drift-check.py` → 53 anchors checked, all resolve
  CI YAML parses; exactly one fuzz-gate step, replacing three hand-named ones
  `scripts/fuzz-gate.sh 1` → PASS, 2 targets, 113K execs in the 1s window
  `FROZEN=1 scripts/fuzz-gate.sh` → PASS (was exit 1 with a fabricated
    counterexample)
  Four negative controls on the gate's new checks, each verified to fail: a
    committed corpus file, an uncommitted crasher, an untracked `*_test.go`, and
    a non-integer duration argument. NOTE: the `*_test.go` and crasher controls
    were only ever run in a one-package repository, where the round-9 defects
    below cannot manifest — see Round 9 for what re-testing them in the
    configuration they were written for turned up.

Tests added this round: `TestInvariantSixThresholdMatchesProductionCheck`
(extended to the whole corpus), `TestInvariantSixThresholdHasNoHeadroom`
(extended to assert eligibility and record the tightest k), plus
`goldenMetricsFixtures`, `invariantSixEligibility`, `tightestKThatFiresOnReal`
and `truncate`. No production code changed this round — every finding was in the
test and gate layer that was supposed to be providing the coverage.

### Round 9 (2026-09-29) — the recursive discovery that could never have worked

Round 8 made `scripts/fuzz-gate.sh` discover fuzz targets recursively. That
change was the right idea and it was not tested in the situation it existed for:
the repository has exactly one package, so every check of the script ran in the
one configuration where the new code path could not fail. Three defects were
sitting in it, all of which activate the moment a second package exists — which
is precisely the case the recursion was added to handle.

**1. `go test -fuzz ./...` cannot work in a multi-package module.** The fuzz
phase still passed `./...` after discovery became recursive. Go rejects that
combination outright:

    cannot use -fuzz flag with multiple packages

The gate reported it as a counterexample, so the failure pointed at
`testdata/fuzz/FuzzHistogramInvariantsSurviveGarbage/` — a file that does not
exist — and told the reader to commit it. The seed-replay phase uses
`go test -run ... ./...`, which *is* correct across packages, so the two phases
disagreed about what `./...` means and only the one that could break was tested.
Fixed by recording each target together with the directory that owns it
(`TARGET_PAIRS`, `"<dir><TAB><Target>"`) and fuzzing one package per invocation,
which is also what Go documents. The flat `TARGETS` list is kept for the summary
lines only; the paired list is what the loops consume, so there is no longer a
way to invoke a target without naming its package.

**2. `dirname` output is a package path, not a filesystem path.** The first
version of the fix passed `$(dirname ...)` straight to `go test`, producing
`go test probesub`, which Go reads as a stdlib import:

    package probesub is not in std (/usr/lib/golang/src/probesub)

That is a build failure reported as a counterexample — the failure mode this
file exists to prevent, reintroduced inside the fix for finding 1. The
directory is now spelled `./probesub` (and `.` at the root, which resolves as
both). Also added: a tracked-but-missing test file is skipped during discovery,
because otherwise sed's own `No such file` reached the log before the clean
error that the set diff produces.

**3. The untracked-crasher check was scoped to the repository root.** Go writes
a crasher beside the package that produced it, so a target in `./probesub`
lands in `./probesub/testdata/fuzz/` — which `git ls-files -- testdata/fuzz`
never sees. Confirmed directly: with a crasher sitting in the subpackage, the
root-only check returned empty and the new per-package check returned it. The
check now iterates the same `TARGET_PAIRS` the fuzz phase uses, so the two can
not drift apart again.

**4. The untracked-test-file cross-check compared lengths, not sets.** It
fired only when the count of on-disk test files differed from the count git
tracks. Deleting one tracked test file while adding one untracked test file
keeps the counts equal and passes, on a tree carrying precisely the gap the
check exists to detect. Replaced with `comm` in both directions, which also
covers the previously unhandled "tracked but deleted" direction: such a file
keeps its target in the list, and the fuzz phase then fails on a package that
no longer builds, far from the cause. Verified by deleting
`probesub/probe_test.go` and adding `untracked_new_test.go` at the same time:
old logic passed, new logic reported both.

No Go code changed. Every finding is in the gate that was supposed to be
providing the coverage — the third consecutive round in which the test and gate
layer, not the code, held the defect.

Verification this round (all re-run on the committed tree):

  `go test -race -count=1 ./...` → ok, 0 failed
  `gofmt -l .` → clean; `go vet ./...` → no issues
  `scripts/sse-negative-check.sh` → PASS (guard has teeth; proxy.go
    byte-identical to HEAD afterwards)
  `scripts/sse-check.py --rounds 3` → PASS, 12/12 streams
  `scripts/secret-scan.sh` → clean
  `scripts/audit-drift-check.py` → 53 anchors checked, all resolve
  CI YAML parses; four jobs, none `continue-on-error`
  `FROZEN=1 scripts/fuzz-gate.sh` → PASS, 2 targets
  `scripts/fuzz-gate.sh 2` → PASS, 2 targets, both fuzzed in `.`

Negative controls for this round, each verified to fail against the
unfixed gate on a deliberately added second package:

  - old gate + second package → `cannot use -fuzz flag with multiple
    packages`, reported as a counterexample (finding 1)
  - `dirname`-spelled package path → `package probesub is not in std`,
    reported as a counterexample (finding 2)
  - crasher in `./probesub/testdata/fuzz/` → old root-only check returns
    empty and passes; new check names the file (finding 3)
  - one deleted tracked test file + one untracked test file → old
    count-based check passes; new set diff reports both (finding 4)
  - `FROZEN=1` / `30s` / `20x` / `-5` as a duration → exit 2, usage error,
    no fabricated counterexample

The probe package used for these controls was removed afterwards; the tree is
byte-identical to `ba4a6c4` apart from the three files this round changed.

### Round 10 (2026-09-29) — making the gate's blind spot permanent

Round 9's four defects shared one cause: the repository is a single Go package,
so the code paths that handle a second one were never executed by any check.
Fixing them left that unchanged. A gate that "passes" while covering one
package looks exactly like one covering two, which is the failure mode the whole
script exists to detect — applied to the script itself.

**1. The same bug class, in `scripts/audit-drift-check.py`.** It collected
sources with `ROOT.glob("*.go")` — root-only, for the same reason the fuzz
gate's discovery had been. Verified by moving `router.go` into `./subpkg`: the
check failed with `summarizeError: not found in any .go file` for symbols
sitting in a subdirectory, and the remediation it printed is to add an entry to
`ALLOWLIST` — a wrong fix, and a permanent one, for a symbol that was never
deleted. Fixed in `56dfbdf`, not in this round — see "a claim that outlived its
commit" below, which is why it is named here rather than folded in. The fix is
`rglob`, skipping `.git` and `vendor`, sorted because the sources are joined
into one corpus and `rglob`'s ordering is filesystem-dependent.

**2. `scripts/fuzz-gate-selftest.sh`.** Builds throwaway two-package
repositories with a stubbed `go` and asserts the gate's behaviour in them, so
the configuration that hid round 9's defects is now reproduced on every CI run.
It needs no Go toolchain and no network; the stub records its arguments and
enforces the one real constraint that matters (`-fuzz` takes a single package).
Seven cases: multi-package discovery, usage errors, the set-difference check,
a subdirectory crasher, a healthy-repo liveness check, the empty-discovery
error, and a negative control that runs the `cb4e826` gate against the same
fixtures and requires it to fail.

The liveness case (5) is the one that keeps the rest honest. Every other case
asserts that the gate *complains*, which a gate that refuses to do anything
would also satisfy. Case 5 requires it to discover both targets, fuzz both, and
exit 0 — so "it passes" cannot be achieved by doing nothing.

The self-test was verified to fail against each of the four round-9 defects
reintroduced one at a time: reverting the fuzz phase to `./...` (5 failures),
dropping the `./` on the subpackage path (2), a root-only crasher check (2),
and a count in place of the set diff (3). A test that has never been seen to
fail is indistinguishable from one that cannot.

Building it also turned up three defects in the test itself, each of which had
produced a *passing* case for the wrong reason:

  - `local name="$1" dir="$TMPROOT/$name"` expanded `$name` before the
    assignment took effect, so every repo path was `<TMPROOT>/` and each case
    failed on `cd: null directory` — a failure about the harness, reported as a
    failure about the gate. Declarations are now split.
  - the `go` stub rejected a bare `.` as a multi-package pattern. `.` names
    exactly one package and is the *correct* argument for a root target, so the
    stub was stricter than the tool it models, and it was the stub under test
    rather than the gate. Only `./...` is refused now.
  - two cases shared one repository, so case 4 asserted against leftovers from
    case 3: the tracked-but-missing file fired first and the gate exited before
    reaching the crasher check, and case 4 passed without testing anything. Each
    case builds its own tree.

No production code changed. As with the three previous rounds, every finding is
in the test and gate layer.

### A claim that outlived its commit

The `audit-drift-check.py` fix above was written, verified, and then lost. A
`git reset --hard` run while probing a different script reverted it, and the
round's summary and this document both went on describing it as done. It was
caught by review — the file at `HEAD` still read `ROOT.glob("*.go")`, `grep -rn
rglob scripts/ .github/` returned nothing, and the file's last commit was three
rounds old.

Two things are worth recording, because both are the same failure wearing
different clothes.

The first is that `git reset --hard` destroys uncommitted work, including work
that has already been verified. This repository's own operating rule forbids it
while uncommitted work exists, and the rule was broken anyway — twice earlier
in the same session, and once more here. Every reset since has been preceded by
copying the file to `/tmp` first. A constraint that is written down but not
enforced is worth exactly as much as the memory of whoever is holding it.

The second is more specific to this document. A finding recorded in past tense
is indistinguishable from a finding that shipped. Nothing in the doc asserted
whether the code at that commit contained the fix, and `audit-drift-check.py`
— the one script with no self-test — could not catch it either. The anchor
drift check validates that *symbols named in findings* still exist; it says
nothing about whether a *fix* a finding describes was applied.

So the remaining gap is named rather than glossed: `scripts/fuzz-gate-selftest.sh`
guards the fuzz gate against this round's bug class, and
`scripts/audit-drift-check.py` has no equivalent. The natural follow-up is to
extend that harness to build a two-package layout and assert the drift check
behaves correctly in it — the same technique, pointed at the other script. Until
then, this class can recur there undetected, exactly as it did.

Verification this round:

  `go test -race -count=1 ./...` → ok, 0 failed
  `gofmt -l .` → clean; `go vet ./...` → no issues
  `scripts/fuzz-gate-selftest.sh` → 31 passed, 0 failed
  `scripts/fuzz-gate.sh 20` → PASS, 2 targets
  `scripts/audit-drift-check.py` → 53 anchors checked, all resolve
  `scripts/secret-scan.sh` → clean
  CI YAML parses; 4 jobs, none `continue-on-error`
  `scripts/sse-check.py --rounds 3` → PASS, 12/12 streams
  `scripts/sse-negative-check.sh` → PASS

Tests added this round: `TestEveryExportedHistogramIsWellFormed`,
`TestEachHistogramInvariantHasTeeth`, plus `checkHistogramInvariants`,
`histogramIndex`, `countHistogramBuckets` and `renderLabelSet`. Both were
verified to fail against the pre-fix exporter, and every individual invariant
was verified to fire against a synthetic violation of itself.

Tests added this round:
TestOverflowBucketIsNotExposedAsHistogram,
TestEveryLabelKeyMapIsBoundedAndRenders,
TestReferenceEscapeMatchesPrometheusRules,
TestLabelEscapeIsNotIdempotentOnEscapedInput, plus the `unpushed-export` CI job.
Each Go test was verified to fail against the unfixed code first; the CI
overflow check was verified to reject the pre-fix output, and the export job
against the pre-fix script.

Tests added this round: TestEveryBoundedMapHasAWellFormedKeyShape,
TestFullExpositionParsesUnderOverflow, TestStatusMapsAreBounded,
TestConcurrentDistinctLabelsNeverExceedTheCap, plus `referenceEscape` as the
fuzz oracle. Each was verified to fail against the unfixed code.

Tests added last round: TestMetricsEvictionPreventsLabelStarvation,
TestMetricsEvictionIsFIFONotRandom, TestMetricsEvictionNeverDisplacesTheOverflowBucket,
TestMetricsEvictedCounterIsExported, TestMetricsSetLabelCap,
TestMetricsCardinalityBoundaryIsExact, TestMetricsOverflowNeverEmitsMalformedSamples,
TestConfigValidationRejectsOutOfRangeLabelCap, TestValidateConfigRangeMatchesGo,
FuzzExpositionRoundTrip.

### Round 11 (2026-09-29) — the two gaps the last round named

Round 10 ended by naming what it had not fixed: `scripts/audit-drift-check.py`
had no self-test, and the anchor check could not tell a finding that shipped from
a finding that was merely described in the past tense. Both are closed here. No
production code changed; for the fourth round running, every finding is in the
test and gate layer.

**1. `scripts/audit-drift-selftest.sh`** — the extension round 10 specified,
built the same way as the fuzz-gate self-test: throwaway two-package
repositories, no Go toolchain, no network. Eleven assertions across seven cases —
recursive discovery in a two-package fixture, a negative control against a copy
of the checker reverted to the root-only glob, a bogus anchor, a deleted
subpackage function, a missing audit document, an empty audit document, and a
liveness case against the real repository.

Two properties are load-bearing:

- **Negative controls run against copies, never the real script.** The suite
  needs a broken checker to assert against. A harness that edited
  `scripts/audit-drift-check.py` in place could destroy the thing it is
  testing — which is the exact class of accident that cost round 10 its fix
  (`git reset --hard` while probing a different script). The fixture copies
  the script into a temp tree and breaks the copy.
- **A liveness case.** Every other case asserts that the checker *complains*,
  which a checker that does nothing would also satisfy. The real-repository
  case requires it to resolve all 53 anchors and exit 0, so "it passes" cannot
  be reached by doing nothing.

Verified to fail when the fix is undone: reverting `rglob` to the root-only glob
produces 3 failures including the behavioural one; making the checker vacuous
(nothing ever reported) produces 4.

**2. `scripts/audit-attribution-check.py`** — the second gap. It checks the
commit-level claim the anchor check cannot:

- every commit hash cited in `docs/audit.md` or `dist/README.md` resolves to a
  real commit. A hash that does not exist is a citation written from memory
  rather than from the history, which is precisely the shape a lost fix takes;
- `dist/README.md`'s unpushed-commit list matches `origin/master..HEAD` as a
  **set**, not a count — the same substitution that fixed the fuzz gate, for
  the same reason: a count is satisfied by any equally-wrong list.

It found real drift immediately. `979dcf6` and `ee4bf69` were unpushed and
missing from the handoff list, so a handoff built from that document would not
have carried them. Both were added.

**The self-reference, and why the check still converges.** Writing the list
creates a commit, which makes the list wrong, which requires rewriting it. This
is the loop `dist/README.md` already documents for the bundle and for the count
in miniature, and left unchecked because it looked unsatisfiable. The
resolution is to exempt the commit that DELIVERS the list — a commit cannot
contain its own hash, so requiring it to is not a constraint but a paradox.

That exemption is where the interesting bug was, and it is worth being precise
about because the first version shipped broken. It read "exempt a commit whose
diff touches `dist/README.md`", with a comment claiming a real change could not
hide behind it. That claim is false, and trivially so: attach a one-line README
tweak to a commit carrying a `main.go` change and the whole commit becomes
exempt. A code change could be dropped from the handoff list by editing a
sentence.

The shipped rule is **confinement, not presence** — a commit is exempt only
when every path in its diff is a tracked handoff artifact under `dist/`. Touch
one code, test, script, CI or audit file and the commit must be listed like any
other. It is still decided from the commit's own diff, so the prose cannot widen
it. Case 4 of `scripts/audit-attribution-selftest.sh` is the exploit itself, run
as a positive assertion: a code commit with a README tweak must be reported.
Both rules were run against the same fixture, and the old one passes it while
the new one catches it.

Two real commits were hiding under the loose rule. `b950b3a` and `6ca056f` both
touched `dist/README.md` alongside real script changes, and neither appeared in
the handoff list — a handoff built from that document would have missed both.
Narrowing the rule is what surfaced them, which is the strongest evidence that
the narrow rule is the correct one.

**3. `--recovered` mode.** The paragraph above records the citation check as
"expected to fail" in a `git am` tree, and documents why. That is true but it
leaves a check that can only ever fail there, which is not a useful thing to
hand the next person. The mode matches each cited hash to the commit carrying
the same CHANGE, using `git patch-id --stable`, which fingerprints a commit's
diff rather than its metadata and therefore survives re-hashing. Verified on
this repository: all 77 content fingerprints are identical between `HEAD` and
its `git am` replay, so every honest citation resolves in the recovered tree —
and a fabricated one still matches nothing and still fails, which is what keeps
the relaxed path from becoming a blanket pass.

Two implementation details are load-bearing and both were got wrong first:

- `git log -p | git patch-id` truncates catastrophically on a large history.
  Piped directly it reported **1** fingerprint instead of 77 on this
  repository, and a map that is silently 98% empty looks exactly like "these
  commits genuinely have no counterpart". The `git log` output is captured
  into memory and fed to `git patch-id` over a pipe instead.
- the content check needs a reference repository, because the cited hashes
  live in the original history that the recovered clone does not have.
  `--recovered` without `--against` exits 2 rather than guessing.

Wiring the attribution check into CI required `fetch-depth: 0` on the `go` job.
`actions/checkout@v4` defaults to a shallow single-ref checkout, which has no
`origin/master`, so the unpushed-list half would have skipped and still exited 0
— a guard passing because it could not see what it guards. That failure is
worse than having no guard, and it is the reason the skip path prints a notice
under `--verbose` rather than failing silently.

**4. `scripts/audit-attribution-selftest.sh`.** Fourteen assertions across seven
cases, built like the other two self-tests: throwaway repositories, negative
controls against copies, a liveness case. Unlike the drift self-test this one
needs a real git repository, because the checker reads history, refs and
per-commit diffs and there is nothing to stub.

It earns its keep twice over, because two of its cases reproduce the exact
vacuity that has cost this repository work. The fixtures must carry a
`refs/remotes/origin/master`, since without one the checker skips the whole
unpushed-list half and exits 0 — the first version of this harness did exactly
that and three cases "passed" while asserting nothing. And case 5 builds a
genuine `git am` replay to exercise `--recovered`, which required fetching the
base commit by SHA: a clone of the local path tracks this checkout's own master,
which already contains every unpushed commit, so replaying onto it applies the
patch on top of commits that are already there and the case then passes for the
wrong reason. The harness asserts the cited hashes really are unresolvable in
the replay before relying on them being matched by content.

Building it also exposed a mistake worth recording. The first attempt to
negative-control `--recovered` — making it accept any content — passed 14/14 and
so proved nothing. The edit had not applied: the replacement string was indented
differently from the line in the file, and a Python `str.replace` that matches
nothing fails silently. Replacing the whole function body fixed it, and the
control then failed case 6 as it should. A negative control that cannot fail is
indistinguishable from a passing test, which is the entire reason this repository
keeps writing them down.

Verification this round:

`go test -race -count=1 ./...` → ok, 0 failed
`gofmt -l .` → clean; `go vet ./...` → no issues
`scripts/audit-drift-selftest.sh` → 11 passed, 0 failed
`scripts/audit-attribution-selftest.sh` → 14 passed, 0 failed
`scripts/audit-attribution-check.py` → all citations resolve, 33 unpushed
  commit(s) listed accurately
`scripts/audit-drift-check.py` → 53 anchors checked, all resolve
`scripts/fuzz-gate-selftest.sh` → 31 passed, 0 failed
`scripts/fuzz-gate.sh 20` → PASS, 2 targets
`scripts/secret-scan.sh` → clean
CI YAML parses; 4 jobs, none `continue-on-error`
`scripts/sse-check.py --rounds 3` → PASS, 12/12 streams
`scripts/sse-negative-check.sh` → PASS
`python3 -m pytest scripts/test_regenerate_config.py -q` → 11 passed

Negative controls for the new checker, each verified to fail first: dropping a
real unpushed commit from the list (exit 1, 23 missing reported), a fabricated
hash cited in `docs/audit.md` (reported), and a phantom commit listed in
`dist/README.md` (reported twice — once as a citation, once as a listed-but-
nonexistent commit). The fabricated hashes are written here without backticks
deliberately: the checker treats a backticked hex token as a citation, so
documenting its own negative control in the document it guards would trip it.

Negative controls for `scripts/audit-attribution-selftest.sh`, each verified to
make it fail: reverting the exemption to "touches `dist/README.md`" fails the
gaming case (12/14), and replacing `_content_present` with an unconditional
`True` fails the fabrication case (13/14).

Tests added this round: none in Go. `scripts/audit-drift-selftest.sh`,
`scripts/audit-attribution-selftest.sh` and `scripts/audit-attribution-check.py`,
all wired into the `go` CI job.

### Round 12 (2026-09-29) — the handoff artifact could go stale unnoticed

Round 11 left one instruction unenforced. `dist/README.md` told the next
session to check that the bundle's tip matches `HEAD` before trusting it. That
was correct, and it was also a thing a session had to remember to do — and it
had been done by hand after every commit, three cycles running.

The failure it guards against is the quietest one available here. A *missing*
bundle is obvious. A *stale* bundle still exists, still passes `git bundle
verify`, is non-empty, and carries a plausible set of commits; it simply
describes an older `HEAD`, so a handoff built from it omits every commit made
since, silently.

**1. `scripts/dist-freshness-check.sh`.** Compares three things against the
repository: the bundle's recorded tip, the patch's last commit, and the patch's
commit count. The count matters because a tip-only comparison would pass for a
bundle that carried the right commit and dropped others.

It reports rather than regenerating, deliberately. A check that silently
rewrites the artifacts reports success for work it never verified, and a
generator that is itself broken stays broken and invisible. The fix is one
command, printed on failure.

It treats "nothing unpushed" as **clean, not stale** and exits 0. A checkout
with no unpushed commits needs no artifacts, and reporting that as a failure
would push the next person towards committing them — the exact self-referential
loop `export-unpushed.sh` exists to prevent. It exits 2 when it cannot run at
all (no upstream ref, not a repository) rather than reporting success it did not
earn.

**2. `scripts/dist-freshness-selftest.sh`.** Sixteen assertions, eight cases, on
throwaway repositories where HEAD, the artifacts and the unpushed set are set
deliberately and then broken.

Building it caught a hole in the test itself, which is the part worth writing
down. Cases 2 and 7 both exercise the bundle tip, and both fail on more than
the tip — the patch end and the patch count disagree there too. So disabling
the tip comparison entirely still left all thirteen assertions green: the other
checks were masking it. Case 8 holds the patch correct and makes **only** the
tip wrong, and asserts the patch is not reported, so nothing can absorb the
failure. With case 8 added, disabling the tip check fails two assertions.

Verified to fail: tip comparison disabled (14/16), patch count check removed
(15/16), "nothing unpushed" treated as stale (15/16).

**3. The skip contract is now pinned.** `audit-attribution-selftest.sh` skips
two assertions in a bare clone — the generated patch is absent, and there are no
unpushed commits — and the passing total legitimately varies by environment
(14 in a full checkout, 9 in a clone). That variability is exactly the shape a
slow loss of coverage takes: a case starts skipping for a new reason, the number
goes down, and the run still looks green. Both totals are now declared
(`EXPECTED_FULL`, `EXPECTED_CLONE`) and the run fails if `PASSED` matches
neither. Verified: removing one assertion turns the run red at 13/14.

This immediately caught a stray empty commit, left behind by a
`git commit --allow-empty` used while negative-controlling the freshness check,
which the attribution check had been reporting and which would otherwise have
been documented as real history. It was removed rather than described — and the
citation that described it had to be written without its hash, because that same
commit is unreachable from `HEAD` and therefore absent from any clone. The check
passes in this working tree and fails in a fresh one, which is precisely the
"claim written from memory of work rather than from the history" shape it exists
to catch, committed here by accident.

**4. CI wiring, and where the freshness check deliberately is not run.** The
`unpushed-export` job now ends by asserting freshness, because it is the one
place where the artifacts genuinely exist: it just generated them, so "fresh" is
a real assertion. Every other assertion in that job would be satisfied by a
generator that silently wrote yesterday's state.

The check is *not* run in the `go` job, and the omission is annotated there. The
artifacts are generated, gitignored, and local to an unpushed history; a CI
checkout has none, so the check would correctly report them missing and the job
would be permanently red. There is nothing for CI to verify about another
machine's unpushed history. What CI does gate is the logic, via the self-test.

Verification this round:

  `go test -race -count=1 ./...` → 329 passed
  `gofmt -l .` → clean; `go vet ./...` → no issues; `go build ./...` → success
  `scripts/dist-freshness-selftest.sh` → 16 passed, 0 failed
  `scripts/dist-freshness-check.sh --verbose` → artifacts match HEAD, 37 covered
  `scripts/audit-attribution-selftest.sh` → 14 passed, 0 failed
    (full run: expected 14)
  `scripts/audit-attribution-selftest.sh` in a fresh clone → 9 passed
    (reduced run: expected 9)
  `scripts/audit-drift-selftest.sh` → 11 passed, 0 failed
  `scripts/fuzz-gate-selftest.sh` → 31 passed, 0 failed
  `scripts/audit-drift-check.py` → 53 anchors checked, all resolve
  `scripts/audit-attribution-check.py` → all citations resolve
  `scripts/secret-scan.sh` → clean
  `scripts/sse-check.py --rounds 3` → PASS, 12/12 streams
  `scripts/sse-negative-check.sh` → PASS
  `python3 -m pytest scripts/test_regenerate_config.py -q` → 11 passed
  CI YAML parses; 4 jobs, none `continue-on-error`

Tests added this round: `scripts/dist-freshness-check.sh` and
`scripts/dist-freshness-selftest.sh`, the latter wired into the `go` job and the
former into the `unpushed-export` job. No production code changed.

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
  `go test -race ./...` → 288 passed, 0 failed (2026-09-29, round 4)
  Negative controls added this round (each verified to FAIL against the
  unfixed code before being accepted):
  • TestOverflowBucketIsNotExposedAsHistogram — reverting the fix restores the
    corrupt histogram AND double-counts (83 vs 49 observations);
  • TestEveryLabelKeyMapIsBoundedAndRenders — an added ninth, unbounded map
    fails it;
  • TestReferenceEscapeMatchesPrometheusRules — dropping backslash handling from
    the oracle fails it;
  • CI `unpushed-export` job — the pre-fix script's bundle cannot be fetched.
  CI `metrics output parses and labels are not forged` — the pre-fix overflow
  output is rejected by the histogram-shape check.

  `go test -race ./...` → 250 passed, 0 failed (2026-09-29)
  `gofmt -l .` → clean; `go vet ./...` → no issues
  `python3 scripts/validate-config.py` → OK: 4 profiles validated
  `scripts/audit-drift-check.py` → all anchors resolve
  `FuzzExpositionRoundTrip` → 60s, 174M execs, no counterexample
  Negative controls (each verified to FAIL against the unfixed code before
  being accepted): TestMetricsOverflowNeverEmitsMalformedSamples against the
  `{__overflow__}` sample; TestValidateConfigRangeMatchesGo against a script
  whose ceiling no longer matches Go.

  Earlier: `go test -race ./...` → 159 passed, 0 failed (2026-09-28)
  Both previously-flaky tests now stable:
  - TestHandleStream_ResourceCleanup — passes in full suite
  - TestConcurrentMidStreamErrorRecovery — fixed to only assert the replay
    invariant for sessions that actually hit backend1 (was asserting it for
    every request, including ones that legitimately skip the cooled backend)
  `go vet ./...` → no issues
  `go build ./...` → success
  `python3 scripts/validate-config.py` → OK: 4 profiles validated

### Round 13 (2026-09-29) — the handoff had never actually been tested

Round 12 proved the artifacts were *current*. It did not prove they were
*usable*, and those are different claims. A bundle whose tip matches `HEAD` can
still be unfetchable. A patch can apply cleanly and produce a tree that does not
build. The recovery procedure had been verified by hand — once, in a session,
against one specific commit. That is the weakest form of verification available:
true of the commit it was run against and of no other, with nothing to notice
when it stops being true.

**1. `scripts/recovery-check.sh`.** Proves both artifacts recover, on demand, by
actually doing it: it fetches the bundle into a throwaway repository, replays
the patch with `git am --3way`, and asserts each recovered tree is byte-identical
to the working tree. It then builds and tests the recovered tree and runs the
audit self-tests inside it, so "it recovered" means "the recovered tree works",
not "the files arrived". Trees are compared rather than commit hashes, because
`git am` re-creates every commit and always will.

Two exit codes matter. Exit 1 is a broken recovery. Exit 2 is "could not run" —
no artifacts, no upstream ref — and the ordering is deliberate: "nothing
unpushed" is checked *before* the artifacts are required, so a fully-pushed
checkout exits 0 without being told it is missing files. Demanding artifacts
first would report a clean tree as broken and push the next person towards
committing the very artifacts that are meant to stay local.

**2. The base is fetched by SHA, and the check asserts it did.** A clone of this
repository's path tracks the local `master`, which already contains every
unpushed commit. Replaying the patch onto that applies it on top of commits that
are already present, reproduces nothing, and every assertion afterwards passes
for the wrong reason. This is the third time that exact trap has cost time here.
`recovery-check.sh` therefore takes the base from `refs/remotes/origin/master`
and fetches it by SHA — and does not merely do so, it *asserts* the base is not
`HEAD`, and asserts that the patch is rejected when replayed onto the wrong base.
Both assertions are new in the attribution self-test too (case 5), so the
guarantee is tested in the place that made the same mistake.

**3. `scripts/recovery-check-selftest.sh`.** Twenty assertions, ten cases,
against throwaway fixtures. Fixtures copy the whole `scripts/` directory rather
than a hand-picked subset: `recovery-check.sh` runs the real self-tests in the
recovered tree, so a fixture missing `fuzz-gate.sh` or a realistic
`docs/audit.md` fails for reasons unrelated to recovery. A check that failed
because the tree it was handed had nothing to check in is now reported as a
*skip*, with the reason printed — but only for "nothing to check" failures.
Anything that ran and failed is still a failure.

Two negative controls, each asserted to have *applied* before its result is
trusted, because a control that silently did nothing is a test that passes for
the wrong reason:

  - **Control A** removes both staleness detectors (the tree comparison and the
    bundle-tip comparison) and requires a stale artifact to then pass with exit
    0. Removing only one proves nothing: the other still catches it, which is
    exactly what happened on the first attempt and looked like a control
    failure. Verified to apply: the neutered copy is re-parsed with `bash -n`
    before use, and both removals are grepped for.
  - **Control B** removes the `--recovered` verification and requires the
    re-hashed `git am` tree to then report that it cannot resolve the re-hashed
    citations.

Verified to fail: control A (19/20 before the second detector was also removed),
control B (18/20 before the message assertion was corrected to the one the
neutered path actually emits).

**4. The count contract caught the count contract.** The attribution
self-test's `EXPECTED_FULL` went 16 → 18 when the two base-discovery assertions
were added, and the run reported `18 passed, 0 failed (full run: expected 16)` as
a failure rather than quietly accepting a smaller number. That is the mechanism
working as intended: a case that stops running changes the total, and the total is
a claim, not a summary. Both expected values now carry the arithmetic that
produces them, so the next person adding a case knows the number to update.

Verification this round:

  `go test -race -count=1 ./...` → 329 passed
  `scripts/recovery-check.sh` → 15 passed, 0 failed (full, incl. build + tests)
  `scripts/recovery-check.sh --quick` → 13 passed, 0 failed
  `scripts/recovery-check-selftest.sh` → 20 passed, 0 failed
  `scripts/audit-attribution-selftest.sh` → 18 passed, 0 failed (full: expected 18)
  `scripts/audit-attribution-selftest.sh` in a fresh clone → 11 passed (reduced: expected 11)
  `scripts/dist-freshness-selftest.sh` → 16 passed, 0 failed
  `scripts/audit-drift-selftest.sh` → 11 passed, 0 failed
  `scripts/fuzz-gate-selftest.sh` → 31 passed, 0 failed
  Negative control on the new base assertion: `git fetch origin HEAD` instead of
    the base SHA → the new assertions fail (15/18), naming the symptom exactly

### Round 14 (2026-09-29) — the check ran in CI, and immediately found two real bugs

Round 13 added `scripts/recovery-check.sh` and its self-test, and wired the
self-test into CI. What it did *not* do was ever run the real check in CI, on
the theory that CI has no generated artifacts to check. That theory was half
right and the omission was a hole: the `unpushed-export` job generates exactly
those artifacts, has a real `base` ref to replay onto, and was therefore able to
run the whole fifteen-assertion path automatically. It does now.

**1. Running the real check in CI found a bug in the CI fixture itself.** The
`unpushed-export` job built its fixture with `git commit --allow-empty`. An empty
commit produces a patch containing no diff at all, and `git am` refuses one:

    Patch is empty.

So the job had been generating a handoff no reader could apply — and every other
assertion in the job still passed, because they all check the *export*, never
the *replay*. This is precisely the claim round 13 said needed proving, and the
only way it could be found was by proving it. The fixture now commits real files.

**2. `recovery-check.sh` now explains that failure instead of shrugging.** The
underlying `git am` output was being discarded, so an empty patch and a genuine
context conflict produced the same one-line "did not apply cleanly". It now
captures the output, and names the empty-patch case specifically with the fix
(`git am --3way --allow-empty`). A check that reports a single cause for two
different problems sends the reader looking for the wrong one.

**3. The second bug was an environment leak, in a self-test that was "green".**
`dist-freshness-selftest.sh` pins its fixtures' upstream at
`refs/remotes/origin/master` but did not clear the `UPSTREAM` environment
variable when invoking the checker. Invoked as `UPSTREAM=base bash
scripts/recovery-check.sh` — which is exactly how the new CI step calls it —
`UPSTREAM=base` propagated into the fixtures, named a ref that does not exist
there, and produced **twelve failures reading like a broken checker**. The
self-test was only ever run in a shell where `UPSTREAM` happened to be unset,
so it had never been wrong in the one way that mattered.

This is the same class of bug the repo has hit repeatedly: a check that is
correct in isolation and wrong in composition, because nothing ran it in the
context it actually ships in. `run_check` now sets `UPSTREAM` explicitly. It is
asserted directly: `UPSTREAM=base bash scripts/dist-freshness-selftest.sh` now
passes 16/16, and failed 4/16 before.

**4. A new check was added, and the diagnosis behind it was wrong first.** Round
13 recorded a memory note claiming this host's bash misparses `while [ ... ];
then` *inside a function body* while accepting it at top level. That was wrong.
Minimal reproduction — four lines, no repository involved — showed `while [
... ]; then` is a syntax error at **every** scope. It is plain POSIX: `while`
and `until` take `do`; `if` and `case` take `then`. The bad diagnosis came from
a `head`-based bisect that truncated mid-function and manufactured an
"unexpected end of file" that was an artifact of the extraction, not the file.

The same round established that `printf '# ...'` is **not** a bug in either
scope; an earlier draft of the linter rejected it, and its own control suite
caught the false positive by asserting that flagged code must genuinely fail
`bash -n`. The rule was removed rather than kept, because a linter that fires
on valid code is worse than none.

`scripts/shell-lint.sh` and `scripts/shell_lint_rules.py` enforce the real rule
against the *tracked* script set. `scripts/shell-lint-control.sh` proves it can
fire and can stay quiet: seventeen cases, each asserting against `bash -n`
itself — flagged code must genuinely fail to parse, unflagged code must
genuinely parse. `if ... then` and `then` inside strings and comments are all
explicit near-misses.

Verified to fail: reintroducing `; then` into
`scripts/dist-freshness-selftest.sh` is caught by the linter, at the same line
`bash -n` reports (line 90), and turns the control suite red.

**5. A reduced run now says so.** `recovery-check.sh` reports its skip count in
the summary. A green line reading "13 passed, 0 failed" does not reveal whether
four checks were skipped for lack of anything to check, and that is the exact
shape a slow loss of coverage takes. The self-test asserts both directions: a
synthetic fixture run *must* report skips, and the real repository run *must
not* — otherwise a check that always printed "SKIPPED" would pass.

**6. Coverage added.** `recovery-check-selftest.sh` is now 29 assertions. Two
new negative controls, each verified to fail:
  - removing the empty-patch diagnosis → 24/25
  - removing the skip reporting → 28/29

Verification this round:

  `go build ./...`, `go vet ./...`, `gofmt -l .` → clean
  `go test -race -count=1 ./...` → 329 passed; `go test -count=1 ./...` → 329 passed
  `scripts/recovery-check.sh --quick` → 13 passed, 0 failed (no skips)
  `scripts/recovery-check-selftest.sh` → 29 passed, 0 failed
  `scripts/shell-lint.sh` → clean (15 scripts)
  `scripts/shell-lint-control.sh` → 17 passed, 0 failed
  `UPSTREAM=base bash scripts/dist-freshness-selftest.sh` → 16 passed, 0 failed
  `scripts/audit-attribution-selftest.sh` → 18 passed, 0 failed
  `scripts/audit-drift-selftest.sh` → 11 passed, 0 failed
  `scripts/fuzz-gate-selftest.sh` → 31 passed, 0 failed
  CI YAML parses; 4 jobs, none `continue-on-error`; the `unpushed-export` job now
  runs the real `recovery-check.sh`, then re-runs the freshness check to prove
  the recovery process did not modify the artifacts it read


### Round 15 (2026-09-29) — a hardcoded ref name, and a test that proved nothing

Round 14 left the recovery tooling green. Green is only worth what it covers, so
this round went looking for the places where a check could pass without the
property it claims to test. It found one real defect and one test of its own.

**1. `recovery-check.sh` assumed a ref name it had no way to know.**
The bundle fetch hardcoded `refs/heads/airouter-unpushed-export`. But
`export-unpushed.sh` honours an `EXPORT_REF` override (its own line 27 default),
so a bundle exported under any other name is a bundle a reader can legitimately
hold — and this check refused it with `bundle could not be fetched`, a message
pointing at corruption rather than at a name. The same assumption is what made
an ad-hoc probe of the real bundle report a false "MISMATCH" this session: the
tip genuinely sits at `refs/heads/airouter-unpushed-export`, the probe looked
for `refs/heads/master`, and the empty result read as staleness.

FIXED. The tip ref is now discovered from the bundle itself via
`git bundle list-heads`, which reports the names the bundle actually contains.
`dist-freshness-check.sh` was already doing this (line 89); the two scripts had
drifted apart. Verified end to end: a bundle exported under
`EXPORT_REF=my-handoff-ref` now recovers (9 passed, 0 failed, exit 0), where the
old code produced `fatal: couldn't find remote ref`.

**2. A corrupted bundle was misdiagnosed by that fix, and the count contract
caught it.** The first version of the discovery pre-check collapsed two
different faults into one message. `git bundle list-heads` exits non-zero on a
bundle it cannot read at all, and exits zero with empty output for a readable
bundle carrying no head. Treating both as "advertises no head ref" told a reader
their corrupt bundle had a naming problem. The assertion count caught it: case 3
(29 → 31 passed, 1 failed) because the corrupted-bundle run no longer produced
`could not be fetched`. A check that only compares totals would have shipped it.

FIXED. The exit status is captured and the two faults are reported separately —
an unreadable bundle falls through to the fetch, which produces the canonical
diagnosis, and a readable-but-empty one is named for what it is. Both paths
verified.

**3. A new case that could not fail was deleted rather than shipped.** Case 12 was
first written to assert that `recovery-check.sh` pins `UPSTREAM` before running
self-tests in the recovered tree. Removing the pin left the run **passing**:
`dist-freshness-selftest.sh` already pins `UPSTREAM` on its own command line
(line 120, the round-12 fix), so the boundary pin was redundant
defense-in-depth, not the fix it was presented as. The assertion was vacuous, and
a vacuous assertion is worse than none — it reports coverage that does not exist.

REPLACED. The honest, load-bearing contract is narrower: the check honours the
caller's `UPSTREAM`. That is asserted by running, and the falsifiable half is the
exit-2 case — a script that ignored `UPSTREAM` and fell back to its own default
returns 0 where the assertion requires 2. Confirmed against a mutant that
hardcodes `UPSTREAM=origin/master`. The reduced-environment assertion
(`env -i`) covers the same refusal without a caller's shell exports. The pin
itself was kept, and is now commented as defense-in-depth rather than as a fix.

**4. The `unpushed-export` job was replayed locally, step by step.** The job had
never been run outside CI. Two steps initially reported failure — and the cause
was the replay harness, not the workflow: it fetched into an empty repository,
where a bundle legitimately fails with "Repository lacks these prerequisite
commits". CI seeds the base first (`git clone -b base .`). Corrected to match,
every step passes and none degrades to a no-op. Notably the recovered-tree
self-tests ran with **zero skips** in a real clone, which a synthetic fixture
cannot show.

**5. Negative controls re-verified independently.** Rather than trusting each
self-test's report of its own controls, eight were reproduced from scratch: a
stale artifact, an empty (`--allow-empty`) patch, cleanup-on-failure, the
`while...then` lint rejection, its `while...do` acceptance, and the exit-2
`UPSTREAM` refusal. All eight fail as they should. (Two initial failures were
inverted logic in the probe, not defects in the code.)

Coverage: `recovery-check-selftest.sh` is now **37 assertions** (was 29),
including 3 for the custom-ref case and 5 for the `UPSTREAM` contract.

**6. The handoff document's own recovery command did not work.** `dist/README.md`
told the reader to run
`git fetch .../airouter-unpushed.bundle 'HEAD:refs/heads/frombundle'`. That is
precisely the command Round 13 identified as broken — `git bundle create A..HEAD`
stores the tip under the ref `HEAD`, and `git fetch` rejects it with `fatal:
couldn't find remote ref HEAD`. The export script was fixed in Round 13; the
document that a human actually reads was not, so the one artifact written *for
the next person* still carried the failure mode that round was about.

FIXED. Both fetch examples now name `refs/heads/airouter-unpushed-export`, with
an explanation of why the name is load-bearing, a note to substitute a custom
`EXPORT_REF` if one was used, and the always-correct `git bundle list-heads` as
the way to ask. Verified by running both the old and new commands: the old one
fails with `couldn't find remote ref HEAD`, the new one fetches successfully.

**7. The live SSE contract was re-verified end to end.** `sse-contract` is the one
job here that cannot be reduced to a self-test: it needs a real gateway, a real
fake upstream, and real HTTP. It was replayed locally — build, start, readiness
probe, `scripts/sse-check.py` across all four models, then the no-comments
assertion. Result: 4/4 streams terminated exactly once, last, with no stray
comments, and the stream still produced data events.

Two rounds of harness failure preceded that result and are recorded because both
looked like contract violations. The gateway does not read `AIROUTER_API_KEY` for
its own auth (that is the *client* key for `sse-check.py`); it takes `-api-key`,
and the upstream key comes from `FAKE_KEY`. And the replay initially probed port
9090, which another process on the machine already held — so the health check
passed against a stranger's listener while the real gateway had died on
`address already in use`, producing four 401s. The harness now uses a dedicated
gateway port and asserts its own process is alive before trusting a probe. Worth
recording as a general shape: a readiness probe that succeeds against a port you
did not bind is not a readiness probe.

### Round 16 (2026-09-29) — a check that never ran, and a doc nobody executed

Round 15 fixed a real defect and left two things unexamined. Both turned out to
be worse than the defect.

**1. The `UPSTREAM` pin was dead code.** Round 15 kept it, honestly labelled as
defense-in-depth. It should have been deleted, and measuring it is what showed
why. The recovered tree contains exactly one ref — `refs/heads/recovered` — so
`origin/master` never resolves there, the guard's condition was never true, and
the branch always fell through to the unpinned path. Its only observable effect
was a note on *every* run reading "recovered tree has no origin/master", which
is a confusing line to print on every successful run.

REMOVED, with the reasoning recorded in place so nobody re-adds it. A mechanism
that cannot execute, guarding a leak that cannot occur, is cost without
benefit. The self-tests own their environment: `dist-freshness-selftest.sh`
pins `UPSTREAM` on its own command line, and the other three never read it.

**2. The handoff document was still unverified, and the fix is now executable.**
The `dist/README.md` repair from Round 15 was itself never *run*. It was a grep
of my own editing, which is the same class of check that let the drift survive in
the first place. `scripts/doc-verify.sh` now extracts the refspecs from the
document's own fenced `sh` blocks and **executes** each against a freshly
generated bundle. It is verified falsifiable: reverting the doc to the broken
`'HEAD:refs/heads/frombundle'` makes it fail on two independent counts (the
execution check and the explicit `HEAD:` check) and name the offending block.

Two details the implementation had to get right, both of which produced false
results first:

  - Only fenced ```sh blocks are scanned. The prose deliberately *quotes* the
    old broken command to explain why it is wrong, so a text-wide scan flags
    the explanation as though it were an instruction.
  - Shell line continuations are joined first, or a refspec written across two
    lines is invisible and the check reports "no fetch command found" against a
    document full of them.

Wired into the `unpushed-export` job, which is the one place that already
generates real artifacts.

**3. Both fixes were confirmed against mutants, not by reading the diff.** The
Round-15 ref-discovery fix had been verified by hand, so case 11 was checked the
same way: appending `BUNDLE_REF="refs/heads/airouter-unpushed-export"` after the
discovery line restores the exact defect, and both custom-ref assertions then
fail (exit 1) and name the missing message. Recorded in the self-test, because an
assertion nobody has ever seen fail is a claim, not a check.

**4. The doc was missing a commit from its own handoff list — and the checker
that noticed was already in the suite.** `audit-attribution-check` failed with
"commit ebdd8d5 is unpushed but missing from dist/README.md's list". That is the
attribution checker working exactly as designed on the file it exists to police.
It is recorded here because the alternative — loosening the checker to make the
gate green — is the failure mode this repository has hit before.

**5. A readiness probe that succeeds against a port you did not bind is a false
positive.** The `sse-contract` replay reported four `401 Unauthorized` and
appeared to be a broken termination contract. It was neither: the gateway had
died on `address already in use` — a different process on the machine held 9090
— and the harness's readiness probe passed against *that* process's `/health`.
The harness never verified it had bound the port it was probing. It now uses a
dedicated port and asserts its own PID is alive before trusting any probe. This
belongs in the record because the symptom pointed squarely at the product, and
the harness was the bug; a probe that reports "the service is up" while the
service is down produces exactly the kind of confident wrong answer that costs
the most time. Ownership of the bound port, not mere acceptance of TCP, is what
makes a readiness check mean anything.

**6. A repo-wide sweep for the same class of defect.** Every hardcoded
`refs/heads/airouter-unpushed-export` and `EXPORT_REF` reference was
reconciled. Results: `dist-freshness-check.sh` reads only the SHA from
`list-heads`, never the ref name, and was confirmed ref-agnostic by running it
against a bundle exported as `totally-different-name` (passes). The CI job's
hardcoded ref is legitimate — that job generates with the default ref and
deliberately exercises the documented reader path. The remaining occurrences are
prose explaining the ref, or the `export-unpushed.sh` default itself.
