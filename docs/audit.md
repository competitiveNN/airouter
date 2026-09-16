CRITICAL (data loss / undefined behavior)

• router.go:96 — isAvailableLocked calls delete(r.cooldowns, ...) while the caller (IsAvailable) only holds RLock. Reader-vs-writer map race; race-detector will flag it.
• api.go:549-557 — ReloadConfig swaps g.config, g.router.config, g.proxy.config with no synchronization. In-flight requests can observe a mix of old + new pointers.
• proxy.go:300-301 — idleTimeoutReader.Read calls i.timer.Reset after the underlying Read returns. Go docs forbid this without explicitly draining the channel; can fire spuriously or fail to arm.
• api.go:380-436 — handleStream has an unbounded for {} with no max-iteration cap and no ctx.Done() check (only inside the wait branch). Combined with the resume semantics, request body grows on every retry.
• proxy.go:462-489 + api.go:285-317 — accumulateContent only extracts Delta.Content; appendAssistantMessage only injects content. tool_calls, role, name, refusal, logprobs are all silently lost on mid-stream resume. Function-calling workflows break across fallbacks. (ChatCompletionMessage has no ToolCalls field at all — api.go:31-35.)

HIGH (user-visible correctness)

• api.go:432 + proxy.go:466 — Resume injects the full accumulated partial, not the new delta. Each fallback multiplies prior content into the assistant message, so the user sees duplicated text.
• router.go:191-198 — SelectNext scans the chain from index 0 after current, so it can reselect a model that already failed this request. Only mitigated by it now being in its own cooldown.
• api.go:330-378 — handleCompletion never calls RecordSuccess. Non-stream cooldowns are sticky: a model escalates on failures and never clears until a 4xx/5xx triggers another ApplyCooldown.
• api.go:41-59 — Vision array-of-parts content is stored as the literal string "[{...}]". estimateTokens then over-counts and requestTimeout returns a much-too-large budget for vision requests.
• ~~api.go:171-173 — generateSessionID uses two time.Now() calls; collidable.~~ Removed in body-session-id refactor; sessionID is now content-addressed via bodySessionID().
• api.go:145-158 — checkAuth returns true when g.gatewayAPIKey == "". If the operator never sets GATEWAY_API_KEY/-api-key, every endpoint (including admin) is unauthenticated.
• api.go:237 — io.ReadAll(r.Body) with no http.MaxBytesReader. Memory-exhaustion DoS.
• router.go:69-88 — cooldowns.json is rewritten in place under the global write lock. Crash mid-write truncates the file; on next start loadCooldowns silently drops all state.
• main.go:99-126 + api.go:560-593 — Watcher and admin-POST race on the same file (double reload, no fsync, no atomic rename).

MEDIUM

• proxy.go:231-251 — firstByteReader leaks one goroutine per timed-out first byte. The released goroutine is only unblocked if Close() is called, and StreamToClient only defers idle.Close(), not first.Close().
• proxy.go:347-348 — bufio.Scanner is capped at 1 MB; a single larger SSE event (e.g. huge tool_calls argument) returns bufio.ErrTooLong and the model gets a 30 s cooldown for a healthy response.
• proxy.go:424-448 — sseEventIsRelease returns true on unparseable JSON ("fail open"). Malformed lines get flushed to the client.
• proxy.go:344-422 — Final SSE event without a trailing blank line is in eventBuf (not buffered); on EOF only flushBuffered runs, so the unterminated event is silently dropped.
• api.go:266-275 + 321-328 — requestTimeout comment says "1s for each additional 10000 tokens"; code does 2s per 5000.
• api.go:278-283 — requestHasVision does bytes.Contains(body, "image_url"); a text message mentioning the literal field name is mis-routed.
• proxy.go:268 — streamIdleTimeout = 60s is hard-coded; a legitimate slow generation (>60s between chunks) triggers a false fallback → duplicated text.
• proxy.go:344-422 — Client disconnects cause w.Write errors that flow into the cooldown path. Needs errors.Is(err, net.ErrClosed)/http.ErrAbortHandler classification.
• api.go:367-376 — Non-stream path copies upstream Content-Encoding/Content-Length to the client. Upstream gzip gets advertised to a client that didn't request it.
• api.go:343, api.go:404 — time.After in select is not stopped on ctx.Done(). Goroutine + channel leak per cancelled request.
• router.go:259-274 — 404/401/403 all get 300 s base cooldown; a permanently misconfigured model cycles forever.
• router.go:283-298 — cooldownForError escalates on consecutive failures since last success ever, not since last cooldown expiry. A permanently broken model is retried at 10 min intervals indefinitely.
• router.go:115-126 — minCooldownWait ignores noVision; if every model is either cooled or noVision for a vision request, it returns wait=0.

LOW (cleanup / hardening)

• router.go:25, 36, 387-388 — rrCounters, ErrNoModelsAvailable, ErrInvalidModel declared but unused.
• router.go:169-199 — SelectNext only used by tests; production path calls SelectEndpoint again (which is why SelectNext "restarts the chain").
• ~~api.go:673-676 — sessionIDMu/sessionIDCounter declared but never referenced.~~ Removed in body-session-id refactor.
• api.go:438-449 — /health is unauthenticated.
• api.go:157 — API-key comparison via == is not constant-time.
• api.go:595-671 — Admin dashboard served without CSP/X-Frame-Options/X-Content-Type-Options.
• ~~api.go:160-169 — X-Session-ID is unvalidated; any caller can pin a victim's session.~~ Header removed; session ID is now content-addressed and cannot be injected by clients.
• router.go:223-236 — summarizeError truncates at a byte boundary, can split UTF-8.
• router.go:56-58 — Cooldown file parse errors are only logged at debug; thundering-herd on next start.
• router.go:373-381 — 4 KB upstream error bodies get persisted to cooldowns.json on every retry.
• main.go:99-126 — watchConfig has no shutdown path; leaks goroutine on graceful shutdown.
• main.go:128-134 — Whole-file SHA-256 every 3s; fsnotify or mtime would be cheaper.

---

RESOURCE LEAK AUDIT (2026-09-07)

Scope: HTTP response bodies, goroutines, file descriptors, timers/contexts.

HTTP Response Bodies — ✅ CLEAN
• proxy.go:177-193 (Forward) — returns nil response on error; caller must not close. Non-stream path in handleCompletion reads and closes body on all paths (200, non-200, error).
• proxy.go:268-316 (StreamToClient) — `defer resp.Body.Close()` at line 286 covers all paths. Explicit Close at line 299 is redundant but harmless.
• proxy.go:238-265 (Warmup) — response bodies read and closed at lines 254-255.

Goroutines — ✅ CLEAN
• proxy.go:337-340 (firstByteReader.Read) — spawns goroutine per first-byte read. On context timeout, closes underlying reader to unblock. Latent risk: if f.r doesn't implement io.Closer, goroutine leaks until connection close. SAFETY: f.r is always *http.Response.Body which implements io.Closer.
• proxy.go:397-421 (idleTimeoutReader.Read) — no goroutine spawned; uses time.AfterFunc. onIdle callback closes underlying reader.
• proxy.go:241-263 (Warmup) — goroutines bounded by wg.Wait(); exit on ctx.Err().
• main.go:88-97 (signal handler) — goroutine exits on signal.
• main.go:134-163 (watchConfig) — goroutine exits on ctx.Done().

File Descriptors — ✅ CLEAN
• router.go:154-202 (saveCooldowns) — atomic write via tmp+rename; temp file removed on any error.
• router.go:128-152 (loadCooldowns) — os.ReadFile closed by GC; no explicit close needed.

Timers/Contexts — ✅ CLEAN
• proxy.go:309 (StreamToClient) — `defer firstCancel()` releases timeout context.
• proxy.go:398 (idleTimeoutReader) — timer stopped in Close(); onIdle sets closed flag.
• api.go:641 (handleCompletion) — `defer cancel()` not used; cancel() called explicitly on all paths.
• api.go:785 (handleStream wait timer) — timer.Stop() called on ctx.Done() path.

Loop Bounds — ✅ CLEAN
• api.go:709 (handleStream) — maxAttempts = chainLen*3 + 1 bounds the loop.
• api.go:566 (handleCompletion) — same bound via maxAttempts.

Safe-in-Practice Sites (latent risks, safe by construction)
1. proxy.go:347-350 — firstByteReader goroutine leak if f.r not io.Closer. Safe because f.r is always *http.Response.Body.
2. api.go:843 — body grows by one assistant message per failed stream attempt. Bounded by maxAttempts (≤10 for 3-element chain).
3. proxy.go:405-410 — race between timer firing and onIdle execution. Safe because onIdle closes the underlying reader, unblocking any in-flight Read.

Regression Tests Added
• TestStreamSSE_FirstByteReaderNoGoroutineLeak — verifies goroutine count returns to baseline after timeout.
• TestStreamSSE_IdleTimeoutClosesReader — verifies idle timeout unblocks a stalled Read.
• TestStreamSSE_BoundedBodyGrowth — verifies body growth is bounded and produces valid JSON.
• TestHandleStream_ResourceCleanup — verifies handleStream cleans up goroutines and timers when context is cancelled mid-stream.

Top fixes, priority order

1. Cap handleStream/handleCompletion iterations and ctx.Done()-check inside the loop — fixes #4 and #6 in the HIGH list, prevents the body-growth runaway.
2. Resume should send only the delta since the last attempt, or strip prior partial from the messages — fixes duplicated text.
3. Persist tool_calls/full delta — extend ChatCompletionMessage + accumulateContent + appendAssistantMessage so function-calling works across fallbacks.
4. Synchronize ReloadConfig with atomic.Pointer[Config] or a dedicated mutex.
5. Call RecordSuccess in handleCompletion and reset cooldowns properly.
6. Fix IsAvailable to take a write lock when expiring cooldowns.
7. Drain timer channel before Reset in idleTimeoutReader, or move to a time.Ticker-driven goroutine.
8. Require GATEWAY_API_KEY (or fail to start) and add subtle.ConstantTimeCompare.
9. Wrap r.Body in http.MaxBytesReader and validate provider URLs in Config.validate.
10. Make cooldowns.json write atomic (tmp + rename), and hard-ban on 404/401/403 after N escalations.

## Prioritized findings — concurrency / session routing
- router.go:512 — shared sticky session update during fallback interferes with concurrent same-session requests
- router.go:522 — retry-exhausted path also wrote session unconditionally
Fix: guard both with `len(tried) == 0` (only update on initial selection per request).
Suggested regression tests: TestConcurrentMidStreamErrorRecovery (already exists), TestConcurrentStreamingSameSession.
Status: fix applied; `go test -race ./...` 99 passed.

## Proxy stream-error recovery paths (additional)
- proxy.go:527-799 streamSSE — error events returned as err, partial content in acc; handled by handleStream replay (api.go:853-888)
- proxy.go:280-329 StreamToClient — first-byte timeout + idle timeout guard mid-stream stalls; errors propagate to handleStream fallback
- Recommended: verify streamSSE never leaks partial errors to client when released==true; test with synthetic SSE error after content release.
Cooldown/priority consistency: cooldowns.json exists; cooldowns.priority.json updated in git (restored to original); no stale cooldown entries for removed endpoints (router cleanupStaleEntries handles).
