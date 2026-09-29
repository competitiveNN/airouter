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
  6.9 MB scrape response. FIXED: every label map is capped at `maxLabelValues`
  (512, ~8x the 60 endpoint keys in the live config) and unknown values
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

Tests added this round: TestMetricsEvictionPreventsLabelStarvation,
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
