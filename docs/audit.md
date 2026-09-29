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
