package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// HandleResponses serves POST /v1/responses, the Open Responses / OpenAI
// Responses surface.
//
// The request is translated to the Chat Completions shape the upstream
// providers understand, then routed through the same fallback chain,
// cooldown and session machinery as /v1/chat/completions. A client on either
// surface therefore shares one session per conversation, and a cooldown
// triggered by one is observed by the other.
//
// The response (or SSE stream) is then re-shaped into the Responses envelope
// the client expects.
func (g *GatewayContext) HandleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		g.recordRequest("", 401, time.Since(start))
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		g.recordRequest("", 405, time.Since(start))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLargeRequestBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, 400, "Failed to read request body", "invalid_request_error", "bad_request")
		g.recordRequest("", 400, time.Since(start))
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		g.recordRequest("", 400, time.Since(start))
		return
	}
	if !IsValidModel(req.Model) {
		writeAPIError(w, 400, fmt.Sprintf("Unknown model: %s. Available: %s", req.Model, strings.Join(LogicalModels, ", ")), "invalid_request_error", "model_not_found")
		g.recordRequest("", 400, time.Since(start))
		return
	}
	if len(raw) > requestBytesForModel(req.Model) {
		writeAPIError(w, 413, "Request body too large", "invalid_request_error", "request_too_large")
		g.recordRequest(req.Model, 413, time.Since(start))
		return
	}

	chatReq, err := toChatCompletionRequest(&req)
	if err != nil {
		writeAPIError(w, 400, "Invalid input items", "invalid_request_error", "bad_input")
		g.recordRequest("", 400, time.Since(start))
		return
	}
	// body is the Chat Completions JSON actually forwarded upstream. The
	// proxy reads the model out of it and replaces it with the endpoint's
	// concrete model id, so it must be the translated form, not the original.
	body, err := buildChatBody(chatReq)
	if err != nil {
		writeAPIError(w, 500, "Failed to encode upstream request", "server_error", "encode_failed")
		g.recordRequest(req.Model, 500, time.Since(start))
		return
	}

	// Default max_tokens if the client omitted it (same rule as chat
	// completions). chatReq.MaxTokens mirrors req.MaxOutputTokens, so the
	// parsed check is correct for the Responses surface too.
	if chatReq.MaxTokens == nil {
		if def := g.defaultMaxTokensForModel(req.Model); def > 0 {
			body = injectMaxTokens(body, def)
		}
	}

	// Vision is detected on the translated chat body, since that is what the
	// provider will see. The Responses input form (input_image) is translated
	// to an image_url chat part, so the standard chat marker is present.
	sessionID := bodySessionID(body)

	// Same up-front context check as the chat surface: a prompt no reachable
	// model can hold would otherwise spend the whole interruption budget on
	// attempts that cannot produce anything. See HandleChatCompletions.
	if tokens := estimateTokens(chatReq); tokens > 0 {
		if fits, largest := g.router.ContextCapacity(req.Model, tokens); !fits {
			msg := fmt.Sprintf("Request is about %d tokens, which exceeds every %s model's context window (largest advertised: %d tokens). Reduce the conversation, or use a model whose window fits it.", tokens, req.Model, largest)
			writeAPIError(w, 400, msg, "invalid_request_error", "context_length_exceeded")
			g.recordRequest(req.Model, 400, time.Since(start))
			return
		}
	}

	if req.Stream {
		g.handleResponsesStream(w, r, body, chatReq, sessionID)
		return
	}
	g.handleResponsesCompletion(w, r, body, chatReq, sessionID)
}

// handleResponsesCompletion drives the non-streaming /v1/responses fallback
// loop. It mirrors handleCompletion: select an endpoint, forward, and on
// failure mark it tried, apply a cooldown, and move to the next in the chain.
func (g *GatewayContext) handleResponsesCompletion(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	start := time.Now()
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)
	// Wall-clock budget for this request's fallback walk. Shared policy with
	// the chat loops: this path was the one that shipped unbounded for a
	// while, so it must use the same bound rather than its own copy.
	walk := newBudgetState(start, timeout)

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	tried := map[string]bool{}

	for {
		if ctx.Err() != nil {
			writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		if walk.exhaustedNow() {
			log.Printf("[debug] session=%s model=%s -> responses fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, walk.attempts, walk.elapsed().Round(time.Millisecond), walk.budget)
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		if walk.attempts >= maxAttempts {
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		walk.next()

		ep, wait := g.router.SelectEndpointForTokens(req.Model, sessionID, requestHasVision(body), tried, tokens)
		if ep == nil {
			// Every endpoint in the chain is on cooldown (or otherwise
			// ineligible). Wait out the SHORTEST remaining cooldown and
			// cycle back: SelectEndpoint restarts its walk at the head of
			// the chain, so the last model in the chain leads back to the
			// first, and a model that recovers mid-request gives this
			// request another chance instead of a guaranteed 503. The
			// wait is capped at the provider's own interruption ceiling,
			// and the loop stays bounded by the wall-clock budget and the
			// attempts counter above, so this cannot spin forever. A zero
			// wait means no cooldown is pending anywhere in the chain;
			// there is nothing to wait for, so fail fast.
			if wait > 0 {
				sleep, ok := cycleBackSleep(wait, walk, g.router.MaxInterruptionFor(req.Model))
				if !ok {
					// The shortest cooldown in the chain outlasts the budget
					// this request has left, so waiting would only delay the
					// 503 that the budget is about to force anyway.
					log.Printf("[debug] session=%s model=%s -> responses cycle-back wait %v outlasts the remaining fallback budget %v; returning 503 now", sessionID, req.Model, wait.Round(time.Millisecond), walk.remaining().Round(time.Millisecond))
					writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
					g.recordRequest(req.Model, 503, time.Since(start))
					return
				}
				timer := time.NewTimer(sleep)
				select {
				case <-ctx.Done():
					timer.Stop()
					writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
					g.recordRequest(req.Model, 503, time.Since(start))
					return
				case <-timer.C:
				}
				continue
			}
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}

		// attemptStart times this single upstream call; `start` above is the
		// request-level clock every recordRequest below measures against.
		attemptStart := time.Now()
		// The provider's own interruption ceiling bounds how much silence
		// this endpoint may put in front of the client. It is the smaller
		// of the ceiling and what the fallback walk has left, so a provider
		// configured for a short window (a local endpoint that answers in
		// milliseconds) does not hold a client open for 30s on its cooldown.
		ceiling := g.providerMaxInterruption(ep.Provider)
		attemptTimeout := walk.attemptTimeout(minDuration(timeout, ceiling))
		if attemptTimeout <= 0 {
			log.Printf("[debug] session=%s model=%s -> responses fallback budget exhausted before attempt %d in %v (budget %v)", sessionID, req.Model, walk.attempts+1, walk.elapsed().Round(time.Millisecond), walk.budget)
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		var lastErr error
		var resp *http.Response
		gotResp := false
		maxProxyTries := g.proxy.proxyTries(ep)
		if maxProxyTries <= 0 {
			maxProxyTries = 1
		}
		for proxyTry := 0; proxyTry < maxProxyTries; proxyTry++ {
			r, err := g.proxy.Forward(reqCtx, body, *ep, sessionID)
			if err != nil {
				var pErr *ProxyError
				if errors.As(err, &pErr) {
					// Proxy failure: mark the proxy down and try again with a
					// different proxy on the same endpoint.
					g.proxy.MarkProxyDown(pErr.Proxy.URL, proxyCooldown)
					lastErr = err
					continue
				}
				lastErr = err
				break
			}
			resp = r
			gotResp = true
			break
		}
		if !gotResp {
			cancel()
			g.recordAttempt(ep, time.Since(attemptStart), false)
			var pErr *ProxyError
			if !errors.As(lastErr, &pErr) && isClientDisconnect(lastErr) {
				writeAPIError(w, 499, "Client disconnected", "server_error", "client_disconnected")
				g.recordTransportDrop()
				g.recordRequest(req.Model, 499, time.Since(start))
				return
			}
			tried[ep.Key()] = true
			g.recordFallback(ep)
			if !errors.As(lastErr, &pErr) {
				// Only cool down the endpoint for non-proxy errors; a proxy
				// fault already landed on the proxy via MarkProxyDown above.
				g.router.ApplyCooldownFromErrorForSession(ep, lastErr, sessionID)
			}
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, lastErr.Error(), sessionID, g.testCooldown)
			}
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBodyBytes+1))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			g.recordAttempt(ep, time.Since(attemptStart), false)
			tried[ep.Key()] = true
			g.recordFallback(ep)
			g.router.ApplyCooldownFromErrorForSession(ep, readErr, sessionID)
			g.recordCooldown()
			g.recordRequest(req.Model, 502, time.Since(start))
			continue
		}
		if len(respBody) > maxUpstreamBodyBytes {
			readErr = fmt.Errorf("upstream body exceeds %d bytes", maxUpstreamBodyBytes)
			g.recordAttempt(ep, time.Since(attemptStart), false)
			tried[ep.Key()] = true
			g.recordFallback(ep)
			g.router.ApplyCooldownFromErrorForSession(ep, readErr, sessionID)
			g.recordCooldown()
			g.recordRequest(req.Model, 502, time.Since(start))
			continue
		}

		if resp.StatusCode != 200 {
			g.recordAttempt(ep, time.Since(attemptStart), false)
			tried[ep.Key()] = true
			g.recordFallback(ep)
			// ApplyCooldownForSession already advances the circuit breaker
			// state machine via recordCircuitFailureLocked, so do NOT also
			// call RecordFailure here — that would double-count the failure
			// against a separate counter and trip the circuit at half the
			// configured threshold.
			g.router.ApplyCooldownForSession(ep, resp.StatusCode, string(respBody), sessionID, 0)
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, resp.StatusCode, string(respBody), sessionID, g.testCooldown)
			}
			g.recordRequest(req.Model, resp.StatusCode, time.Since(start))
			continue
		}

		// Model patches rewrite provider-specific output artifacts
		// (e.g. tool calls a model traps inside its thinking
		// block) before the body is parsed, so the parsed
		// completion carries the rescued tool calls.
		if patch := modelPatchFor(ep.Model); patch != nil {
			respBody = patch.TransformCompletion(respBody)
		}

		chat, err := parseChatResponse(respBody)
		if err != nil {
			// A 200 that isn't a usable completion is a provider fault, not a
			// client error: mark it and fall through to the next model.
			g.recordAttempt(ep, time.Since(attemptStart), false)
			tried[ep.Key()] = true
			g.recordFallback(ep)
			g.router.ApplyCooldownForSession(ep, 502, err.Error(), sessionID, 0)
			g.recordCooldown()
			g.recordRequest(req.Model, 502, time.Since(start))
			continue
		}

		g.router.RecordSuccess(ep)
		g.recordAttempt(ep, time.Since(attemptStart), true)
		if chat.Usage != nil && chat.Usage.CompletionTokens > 0 {
			g.router.RecordTPS(*ep, float64(chat.Usage.CompletionTokens)/time.Since(attemptStart).Seconds())
			if g.metrics != nil {
				g.metrics.RecordDashboardTokens(ep.Key(), int64(chat.Usage.CompletionTokens))
			}
		}
		env := buildResponseEnvelope(chat, req.Model)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(env)
		g.recordRequest(req.Model, 200, time.Since(start))
		return
	}
}

// handleResponsesStream drives the streaming /v1/responses fallback loop.
//
// Unlike the chat stream path, it cannot forward raw upstream SSE: the client
// expects Responses events, so every event must be re-shaped. The chat path's
// "replay partial output on fallback" trick therefore does not apply here --
// we buffer the first token and, on a mid-stream failure, fall back to the
// next model and re-emit that model's output as the continuation of the same
// response. Any text already emitted to the client is carried into the
// retry as prior assistant context so the new model continues rather than
// restarting.
func (g *GatewayContext) handleResponsesStream(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	start := time.Now()
	ctx := r.Context()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "Streaming not supported", "server_error", "streaming_unsupported")
		g.recordRequest(req.Model, 500, time.Since(start))
		return
	}

	timeout := requestTimeout(estimateTokens(req))
	// Wall-clock budget for this request's fallback walk. Shared policy with
	// the chat loops: this path was the one that shipped unbounded for a
	// while, so it must use the same bound rather than its own copy.
	walk := newBudgetState(start, timeout)

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	tried := map[string]bool{}
	tokens := estimateTokens(req)

	// retryBody is the upstream body for the current attempt. After a
	// mid-stream failure it is rebuilt with the partial assistant output
	// appended, so the next model continues the answer.
	retryBody := body

	for {
		if ctx.Err() != nil {
			if cause := context.Cause(ctx); cause != nil {
				log.Printf("[debug] session=%s model=%s -> responses stream cancelled: %v", sessionID, req.Model, cause)
			}
			g.recordRequest(req.Model, 499, time.Since(start))
			return
		}
		// Wall-clock guard, mirroring the chat streaming path. Every attempt
		// here is fully buffered and only replayed to the client on success
		// (see below), so an error written here cannot corrupt a stream that
		// has already started — the buffer is discarded, not flushed.
		if walk.exhaustedNow() {
			log.Printf("[debug] session=%s model=%s -> responses stream fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, walk.attempts, walk.elapsed().Round(time.Millisecond), walk.budget)
			writeSSEError(w, flusher, "All models are currently unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		if walk.attempts >= maxAttempts {
			writeSSEError(w, flusher, "All models are currently unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		walk.next()

		ep, wait := g.router.SelectEndpointForTokens(req.Model, sessionID, requestHasVision(retryBody), tried, tokens)
		if ep == nil {
			if wait > 0 {
				sleep, ok := cycleBackSleep(wait, walk, g.router.MaxInterruptionFor(req.Model))
				if !ok {
					log.Printf("[debug] session=%s model=%s -> responses stream cycle-back wait %v outlasts the remaining fallback budget %v; returning 503 now", sessionID, req.Model, wait.Round(time.Millisecond), walk.remaining().Round(time.Millisecond))
					writeSSEError(w, flusher, "All models are currently unavailable")
					g.recordRequest(req.Model, 503, time.Since(start))
					return
				}
				timer := time.NewTimer(sleep)
				select {
				case <-ctx.Done():
					timer.Stop()
					g.recordRequest(req.Model, 499, time.Since(start))
					return
				case <-timer.C:
				}
				continue
			}
			writeSSEError(w, flusher, "All models are currently unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}

		// The Responses event surface is written directly to the client, so
		// we cannot inspect the upstream stream before committing to it.
		// Instead we translate into an in-memory buffer first: if the
		// attempt fails before producing anything, nothing has been sent and
		// we can fall back cleanly.
		var buf strings.Builder
		var attempt *responsesStreamWriter
		// attemptStart times this single upstream attempt; `start` above is
		// the request-level clock. Sharing them would make the TPS figure a
		// per-request rather than a per-attempt measurement.
		attemptStart := time.Now()

		// A proxy fault is the proxy's doing, not the endpoint's, so retry
		// with a different proxy on the same endpoint before giving up on it,
		// and never charge the endpoint for the failure.
		maxProxyTries := g.proxy.proxyTries(ep)
		if maxProxyTries <= 0 {
			maxProxyTries = 1
		}
		var text string
		var toolCalls []streamToolCall
		var err error
		// The provider's own interruption ceiling bounds how much silence
		// this endpoint may put in front of the client. It is the smaller
		// of the ceiling and what the fallback walk has left, so a provider
		// configured for a short window (a local endpoint that answers in
		// milliseconds) does not hold a client open for 30s on its cooldown.
		ceiling := g.providerMaxInterruption(ep.Provider)
		for proxyTry := 0; proxyTry < maxProxyTries; proxyTry++ {
			proxyTimeout := walk.attemptTimeout(minDuration(timeout, ceiling))
			if proxyTimeout <= 0 {
				err = context.DeadlineExceeded
				break
			}
			buf.Reset()
			attempt = newResponsesStreamWriter(&buf, nopFlusher{}, req.Model)
			text, toolCalls, err = g.streamResponsesAttempt(ctx, retryBody, *ep, sessionID, attempt, proxyTimeout)
			if err == nil {
				break
			}
			var pErr *ProxyError
			if !errors.As(err, &pErr) {
				break // endpoint failure: another proxy would not help
			}
			g.proxy.MarkProxyDown(pErr.Proxy.URL, proxyCooldown)
		}
		if err != nil {
			tried[ep.Key()] = true
			g.recordFallback(ep)
			var pErr *ProxyError
			// ApplyCooldownFromErrorForSession already advances the circuit
			// breaker state machine; the redundant RecordFailure call would
			// double-count this failure and trip the circuit at half the
			// configured threshold.
			if !errors.As(err, &pErr) {
				// Only the endpoint is at fault when no proxy was in the path.
				g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
			}
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
			}
			continue
		}

		// Success: replay the buffered lifecycle + deltas to the client.
		// The response id and sequence numbers were generated against the
		// buffer, so replaying verbatim keeps them self-consistent.
		fmt.Fprint(w, buf.String())
		flusher.Flush()
		g.router.RecordSuccess(ep)
		attemptDur := time.Since(attemptStart)
		g.recordAttempt(ep, attemptDur, true)
		// The speed leaderboard is driven by the ring, which needs the token
		// count. The usage event arrives at the end of the stream — often
		// after [DONE] — so it is only available now, once the attempt has
		// succeeded and its usage belongs to output the client received.
		if n := attempt.usage; n > 0 && attemptDur > 0 {
			g.router.RecordTPS(*ep, float64(n)/attemptDur.Seconds())
			if g.metrics != nil {
				g.metrics.RecordDashboardTokens(ep.Key(), int64(n))
			}
		}
		log.Printf("[debug] session=%s model=%s -> responses stream -> %s/%s (%d chars)", sessionID, req.Model, ep.Provider, ep.Model, tokens)
		_ = text
		_ = toolCalls
		g.recordRequest(req.Model, 200, time.Since(start))
		return
	}
}

// streamResponsesAttempt runs one upstream attempt through the Responses
// translator, writing into sw. It returns the accumulated text and tool calls
// so a failure can carry them into the retry.
//
// Like every other proxy surface, a proxied endpoint routes through the pool: a
// transport-layer failure with a proxy in the path becomes a ProxyError, so the
// caller cools the proxy rather than the endpoint. Dialing direct here would
// both silently bypass the operator's proxy setting and mis-attribute the fault
// to a healthy endpoint.
//
// timeout bounds this attempt by the request's own size-adjusted budget: it
// caps the connection phases (dial, TLS, response headers) through the
// per-attempt transport, and the time-to-first-token through firstByteReader.
// The idle watchdog below then bounds the rest of the stream.
func (g *GatewayContext) streamResponsesAttempt(ctx context.Context, body []byte, ep ModelEndpoint, sessionID string, sw *responsesStreamWriter, timeout time.Duration) (string, []streamToolCall, error) {
	providerCfg, ok := g.proxy.config.Load().Providers[ep.Provider]
	if !ok {
		return "", nil, fmt.Errorf("unknown provider: %s", ep.Provider)
	}
	// This surface buffers the whole attempt and replays it only on success,
	// so the client sees nothing until the attempt completes. The timeout is
	// therefore an end-to-end interruption bound for this attempt, not just a
	// time-to-first-byte bound.
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := g.proxy.buildRequest(attemptCtx, body, ep, &providerCfg, true, sessionID)
	if err != nil {
		return "", nil, err
	}
	client, usedProxy := g.proxy.attemptClient(&ep, timeout)
	resp, err := client.Do(req)
	if err != nil {
		// proxyAttribution, not proxyFault: a bare CONNECT status-text rejection
		// has no wrapper for proxyFault to match, so it used to land on the
		// endpoint's cooldown instead of on the proxy.
		if proxyAttribution(err, usedProxy != nil) {
			return "", nil, &ProxyError{Proxy: usedProxy, Err: err}
		}
		return "", nil, &ProviderError{Err: err}
	}
	if usedProxy != nil {
		// The proxy carried this request as far as a response header, which is
		// the strongest evidence of usability available, so renew its mark.
		g.proxy.MarkProxyReady(usedProxy.URL)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes))
		return "", nil, &ProviderError{StatusCode: resp.StatusCode, Body: b, RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes))
		resp.Body.Close()
		return "", nil, &ProviderError{StatusCode: resp.StatusCode, Body: b, RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	// Bound the body exactly like the chat streaming path does. Without this
	// the Responses surfaces had no watchdog at all: translateChatSSE read the
	// upstream body through a bare bufio.Scanner, so an upstream that stalled
	// after the headers — emitting neither a chunk nor an SSE error, and never
	// closing the connection — held the client (or the WebSocket turn) open
	// forever, with no error to fall back on. The response header is not the
	// first byte of CONTENT either: a large prompt makes the provider prefill
	// before it emits anything, so the time-to-first-token needs its own
	// bound.
	firstCtx, firstCancel := context.WithTimeout(attemptCtx, timeout)
	defer firstCancel()
	guarded := &firstByteReader{r: resp.Body, firstCtx: firstCtx}
	idle := newIdleTimeoutReader(guarded, minDuration(g.proxy.streamIdleTimeout, timeout))
	defer idle.Close()

	return translateChatSSE(idle, sw, ep.Model)
}

// nopFlusher satisfies flushWriter while we buffer. The real flush happens
// once, when the buffered attempt is replayed to the client.
type nopFlusher struct{}

func (nopFlusher) Flush() {}
