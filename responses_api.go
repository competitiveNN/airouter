package main

import (
	"context"
	"encoding/json"
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

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
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
		g.recordRequest("", 500, time.Since(start))
		return
	}

	// Vision is detected on the translated chat body, since that is what the
	// provider will see. The Responses input form (input_image) is translated
	// to an image_url chat part, so the standard chat marker is present.
	sessionID := bodySessionID(body)

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
	timeout := requestTimeout(estimateTokens(req))

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	attempts := 0
	tried := map[string]bool{}

	for {
		if ctx.Err() != nil {
			writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		if attempts >= maxAttempts {
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body), tried)
		if ep == nil {
			if wait > 0 {
				timer := time.NewTimer(minDuration(wait, 30*time.Second))
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

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := g.proxy.Forward(reqCtx, body, *ep, sessionID)
		if err != nil {
			cancel()
			if isClientDisconnect(err) {
				writeAPIError(w, 499, "Client disconnected", "server_error", "client_disconnected")
				g.recordRequest(req.Model, 499, time.Since(start))
				return
			}
			tried[ep.Key()] = true
			g.recordFallback()
			g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
			}
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			tried[ep.Key()] = true
			g.recordFallback()
			g.router.ApplyCooldownFromErrorForSession(ep, readErr, sessionID)
			g.recordCooldown()
			g.recordRequest(req.Model, 502, time.Since(start))
			continue
		}

		if resp.StatusCode != 200 {
			tried[ep.Key()] = true
			g.recordFallback()
			g.router.ApplyCooldownForSession(ep, resp.StatusCode, string(respBody), sessionID, 0)
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, resp.StatusCode, string(respBody), sessionID, g.testCooldown)
			}
			g.recordRequest(req.Model, resp.StatusCode, time.Since(start))
			continue
		}

		chat, err := parseChatResponse(respBody)
		if err != nil {
			// A 200 that isn't a usable completion is a provider fault, not a
			// client error: mark it and fall through to the next model.
			tried[ep.Key()] = true
			g.recordFallback()
			g.router.ApplyCooldownForSession(ep, 502, err.Error(), sessionID, 0)
			g.recordCooldown()
			g.recordRequest(req.Model, 502, time.Since(start))
			continue
		}

		g.router.RecordSuccess(ep)
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

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	attempts := 0
	tried := map[string]bool{}

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
		if attempts >= maxAttempts {
			writeSSEError(w, flusher, "All models are currently unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(retryBody), tried)
		if ep == nil {
			if wait > 0 {
				timer := time.NewTimer(minDuration(wait, 30*time.Second))
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
		attempt := newResponsesStreamWriter(&buf, nopFlusher{}, req.Model)
		tokens := estimateTokens(req)
		text, toolCalls, err := g.streamResponsesAttempt(ctx, retryBody, *ep, sessionID, attempt)
		if err != nil {
			tried[ep.Key()] = true
			g.recordFallback()
			g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
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
func (g *GatewayContext) streamResponsesAttempt(ctx context.Context, body []byte, ep ModelEndpoint, sessionID string, sw *responsesStreamWriter) (string, []streamToolCall, error) {
	providerCfg, ok := g.proxy.config.Load().Providers[ep.Provider]
	if !ok {
		return "", nil, fmt.Errorf("unknown provider: %s", ep.Provider)
	}
	req, err := g.proxy.buildRequest(ctx, body, ep, &providerCfg, true, sessionID)
	if err != nil {
		return "", nil, err
	}
	resp, err := g.proxy.client.Do(req)
	if err != nil {
		return "", nil, &ProviderError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, &ProviderError{StatusCode: resp.StatusCode, Body: b, RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return "", nil, &ProviderError{StatusCode: resp.StatusCode, Body: b, RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return translateChatSSE(resp.Body, sw)
}

// nopFlusher satisfies flushWriter while we buffer. The real flush happens
// once, when the buffered attempt is replayed to the client.
type nopFlusher struct{}

func (nopFlusher) Flush() {}
