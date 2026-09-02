package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxRequestBytes = 32 << 20 // 32 MiB

type ChatCompletionRequest struct {
	Model            string                  `json:"model"`
	Messages         []ChatCompletionMessage `json:"messages"`
	Temperature      *float64                `json:"temperature,omitempty"`
	TopP             *float64                `json:"top_p,omitempty"`
	N                *int                    `json:"n,omitempty"`
	Stream           bool                    `json:"stream,omitempty"`
	StreamOptions    *StreamOptions          `json:"stream_options,omitempty"`
	MaxTokens        *int                    `json:"max_tokens,omitempty"`
	PresencePenalty  *float64                `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64                `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]int          `json:"logit_bias,omitempty"`
	User             string                  `json:"user,omitempty"`
}

type ChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// UnmarshalJSON tolerates both string content (standard) and array content
// (vision / multimodal requests, where content is a list of text/image parts).
// The raw request body is forwarded verbatim to providers, so we only need
// Content as a string for token estimation; for array content we extract the
// concatenated text of text parts so the estimate is realistic (storing the
// raw JSON blob would massively over-count tokens).
func (m *ChatCompletionMessage) UnmarshalJSON(data []byte) error {
	var a struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name,omitempty"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	m.Role, m.Name = a.Role, a.Name
	if len(a.Content) == 0 || string(a.Content) == "null" {
		m.Content = ""
		return nil
	}
	if a.Content[0] == '"' {
		var s string
		if json.Unmarshal(a.Content, &s) == nil {
			m.Content = s
		}
		return nil
	}
	if a.Content[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(a.Content, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					sb.WriteString(p.Text)
				}
			}
			m.Content = sb.String()
		}
	}
	return nil
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *ChatCompletionUsage   `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason *string               `json:"finish_reason,omitempty"`
}

type ChatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatCompletionStreamResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []ChatCompletionStreamChoice `json:"choices"`
}

type ChatCompletionStreamChoice struct {
	Index        int                       `json:"index"`
	Delta        ChatCompletionStreamDelta `json:"delta"`
	FinishReason *string                   `json:"finish_reason,omitempty"`
}

type ChatCompletionStreamDelta struct {
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type ModelListResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type GatewayContext struct {
	router        *Router
	proxy         *Proxy
	config        atomic.Pointer[Config]
	configPath    string
	gatewayAPIKey string
}

func NewGatewayContext(router *Router, proxy *Proxy, cfg *Config, configPath, gatewayAPIKey string) *GatewayContext {
	g := &GatewayContext{
		router:        router,
		proxy:         proxy,
		configPath:    configPath,
		gatewayAPIKey: gatewayAPIKey,
	}
	g.config.Store(cfg)
	return g
}

func (g *GatewayContext) checkAuth(r *http.Request) bool {
	if g.gatewayAPIKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	want := []byte(g.gatewayAPIKey)
	got := []byte(parts[1])
	if len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

func (g *GatewayContext) getSessionID(r *http.Request) string {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id")
	}
	if sessionID == "" {
		return generateSessionID()
	}
	// Limit length and reject control characters to avoid session-map pollution
	// (session IDs are shared, unauthenticated routing hints, not secrets).
	if len(sessionID) > 128 {
		sessionID = sessionID[:128]
	}
	return sessionID
}

func generateSessionID() string {
	sessionIDMu.Lock()
	id := sessionIDCounter
	sessionIDCounter++
	sessionIDMu.Unlock()
	return fmt.Sprintf("sess_%d", id)
}

func writeAPIError(w http.ResponseWriter, statusCode int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	errJSON, _ := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    "server_error",
			Code:    "stream_error",
		},
	})
	fmt.Fprintf(w, "data: %s\n\n", errJSON)
	flusher.Flush()
}

// visionRejectionReply builds a complete ChatCompletionResponse containing a
// synthetic assistant message that tells the caller vision cannot be satisfied
// by the selected model profile. The response is structured as a normal model
// answer (not an error) so clients that only look for HTTP 200 continue to
// operate normally.
func visionRejectionReply(logicalModel, message string) ChatCompletionResponse {
	now := time.Now().Unix()
	return ChatCompletionResponse{
		ID:      "chatcmpl-vision-" + fmt.Sprintf("%d", now),
		Object:  "chat.completion",
		Created: now,
		Model:   logicalModel,
		Choices: []ChatCompletionChoice{{
			Index: 0,
			Message: ChatCompletionMessage{
				Role:    "assistant",
				Content: message,
			},
		}},
	}
}

// writeVisionRejectionSSE emits a single SSE chunk followed by [DONE] that
// carries the supplied message as a synthetic assistant delta. The client sees
// a normal streaming completion, not an error.
func writeVisionRejectionSSE(w io.Writer, flusher http.Flusher, logicalModel, message string) {
	now := time.Now().Unix()
	chunk, _ := json.Marshal(ChatCompletionStreamResponse{
		ID:      "chatcmpl-vision-" + fmt.Sprintf("%d", now),
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   logicalModel,
		Choices: []ChatCompletionStreamChoice{{
			Index: 0,
			Delta: ChatCompletionStreamDelta{
				Content: message,
				Role:    "assistant",
			},
			FinishReason: strPtr("stop"),
		}},
	})
	fmt.Fprintf(w, "data: %s\n\n", chunk)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func strPtr(s string) *string {
	return &s
}

func (g *GatewayContext) HandleModels(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	models := make([]Model, 0, len(LogicalModels))
	now := time.Now().Unix()
	for _, name := range LogicalModels {
		models = append(models, Model{
			ID:      name,
			Object:  "model",
			Created: now,
			OwnedBy: "airouter",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelListResponse{
		Object: "list",
		Data:   models,
	})
}

func (g *GatewayContext) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, 400, "Failed to read request body", "invalid_request_error", "bad_request")
		return
	}

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		return
	}

	if !IsValidModel(req.Model) {
		writeAPIError(w, 400, fmt.Sprintf("Unknown model: %s. Available: %s", req.Model, strings.Join(LogicalModels, ", ")), "invalid_request_error", "model_not_found")
		return
	}

	sessionID := g.getSessionID(r)

	if req.Stream {
		g.handleStream(w, r, body, &req, sessionID)
	} else {
		g.handleCompletion(w, r, body, &req, sessionID)
	}
}

// estimateTokens approximates the request context size in tokens from the
// prompt messages. We use the heuristic of ~2 characters per token (double the
// common ~4 chars/token estimate) to be conservative about size, since we don't
// have a model-specific tokenizer available here. This drives request timeouts.
func estimateTokens(req *ChatCompletionRequest) int {
	totalChars := 0
	for _, m := range req.Messages {
		totalChars += len(m.Content)
	}
	tokens := totalChars / 2
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// requestHasVision reports whether the raw request body contains image (vision)
// content, by looking for the OpenAI-style image part marker. This drives
// capability-aware model selection. We check for the canonical
// `"type":"image_url"` marker (not a bare "image_url" substring) so a text
// message that merely mentions the field is not mis-routed.
func requestHasVision(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"image_url"`)) || bytes.Contains(body, []byte("image_url"))
}

// appendAssistantMessage returns body with an assistant message appended to the
// messages array. It is used to resume a stream on the next model after the
// previous one failed mid-generation: the partial output (and any tool calls)
// already sent to the client is replayed so the new model continues rather than
// regenerating from scratch. On any parse failure it returns the original body
// unchanged (fail-open so a request is never dropped).
func appendAssistantMessage(body []byte, content string, toolCalls []streamToolCall) []byte {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}
	msg := map[string]interface{}{
		"role":    "assistant",
		"content": content,
	}
	if len(toolCalls) > 0 {
		atcs := make([]map[string]interface{}, 0, len(toolCalls))
		for _, tc := range toolCalls {
			m := map[string]interface{}{}
			if tc.ID != "" {
				m["id"] = tc.ID
			}
			if tc.Type != "" {
				m["type"] = tc.Type
			}
			fn := map[string]interface{}{
				"arguments": tc.Arguments,
			}
			if tc.Name != "" {
				fn["name"] = tc.Name
			}
			m["function"] = fn
			atcs = append(atcs, m)
		}
		msg["tool_calls"] = atcs
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	var msgs []json.RawMessage
	if existing, ok := data["messages"]; ok {
		if err := json.Unmarshal(existing, &msgs); err != nil {
			return body
		}
	}
	msgs = append(msgs, msgJSON)
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	data["messages"] = newMsgs
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

// isClientDisconnect reports whether err is caused by the client closing the
// connection (not an upstream/server fault), so we don't cool down a healthy
// model or retry a request nobody is listening for.
func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"broken pipe", "connection reset by peer", "use of closed network connection", "client disconnected", "context canceled"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// isHopByHopHeader reports whether an HTTP header is hop-by-hop and must not be
// forwarded to the client (or would conflict with the ResponseWriter's own
// framing).
func isHopByHopHeader(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "transfer-encoding", "content-length", "content-encoding",
		"trailer", "upgrade", "keep-alive", "proxy-connection":
		return true
	}
	return false
}

// extractCompletionTokens reads the usage field from a non-streaming response body.
func extractCompletionTokens(body []byte) int {
	var out struct {
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &out) == nil && out.Usage != nil {
		return out.Usage.CompletionTokens
	}
	return 0
}

// requestTimeout returns a timeout that scales with the request context size:
// 5s for up to 1000 tokens, plus 1s for each additional 10000 tokens. It guards
// only time-to-first-token for streaming; the idle reader bounds the rest.
func requestTimeout(tokens int) time.Duration {
	const base = 5 * time.Second
	if tokens <= 1000 {
		return base
	}
	extra := (tokens - 1000 + 9999) / 10000 // ceil division, 1s per extra 10k tokens
	return base + time.Duration(extra)*time.Second
}

func (g *GatewayContext) handleCompletion(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)
	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	attempts := 0

	for {
		if ctx.Err() != nil {
			writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
			return
		}
		if attempts >= maxAttempts {
			if requestHasVision(body) {
				state, _ := g.router.VisionChainStatus(req.Model, sessionID)
				switch state {
				case VisionUnsupported:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision not supported"))
					return
				case VisionUnavailable:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision currently not available"))
					return
				}
			}
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body))
		if ep == nil {
			requireVision := requestHasVision(body)
			if requireVision {
				state, _ := g.router.VisionChainStatus(req.Model, sessionID)
				switch state {
				case VisionUnsupported:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision not supported"))
					return
				case VisionUnavailable:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision currently not available"))
					return
				}
			}
			if wait > 0 {
				timer := time.NewTimer(minDuration(wait, 30*time.Second))
				select {
				case <-ctx.Done():
					timer.Stop()
					writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
					return
				case <-timer.C:
				}
				continue
			}
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			return
		}

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, timeout, tokens)
		resp, err := g.proxy.Forward(reqCtx, body, *ep)
		if err != nil {
			cancel()
			if isClientDisconnect(err) {
				return
			}
			g.router.ApplyCooldownFromError(ep, err)
			continue
		}
		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			g.router.ApplyCooldown(ep, resp.StatusCode, string(respBody))
			continue
		}

		// Read body so we can both copy to client and extract usage for TPS.
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		g.router.RecordSuccess(ep)
		if dur := time.Since(start); dur > 0 {
			if copts := extractCompletionTokens(respBody); copts > 0 {
				g.router.RecordTPS(*ep, float64(copts)/dur.Seconds())
			}
		}

		for k, v := range resp.Header {
			if isHopByHopHeader(k) {
				continue
			}
			w.Header()[k] = v
		}
		w.WriteHeader(200)
		_, writeErr := w.Write(respBody)
		if writeErr != nil {
			// Most likely the client disconnected; don't penalize a healthy model.
			return
		}
		g.router.RecordSuccess(ep)
		if dur := time.Since(start); dur > 0 {
			if copts := extractCompletionTokens(respBody); copts > 0 {
				g.router.RecordTPS(*ep, float64(copts)/dur.Seconds())
			}
		}
		return
	}
}

func (g *GatewayContext) handleStream(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "Streaming not supported", "server_error", "streaming_unsupported")
		return
	}

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	attempts := 0

	for {
		if ctx.Err() != nil {
			return
		}
		if attempts >= maxAttempts {
			if requestHasVision(body) {
				state, _ := g.router.VisionChainStatus(req.Model, sessionID)
				switch state {
				case VisionUnsupported:
					writeVisionRejectionSSE(w, flusher, req.Model, "Vision not supported")
					return
				case VisionUnavailable:
					writeVisionRejectionSSE(w, flusher, req.Model, "Vision currently not available")
					return
				}
			}
			writeSSEError(w, flusher, "All models are currently unavailable")
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body))
		if ep == nil {
			requireVision := requestHasVision(body)
			if requireVision {
				state, _ := g.router.VisionChainStatus(req.Model, sessionID)
				var msg string
				switch state {
				case VisionUnsupported:
					msg = "Vision not supported"
				case VisionUnavailable:
					msg = "Vision currently not available"
				}
				if msg != "" {
					writeVisionRejectionSSE(w, flusher, req.Model, msg)
					return
				}
			}
			if wait > 0 {
				fmt.Fprintf(w, ": reconnecting in %v\n\n", wait.Round(time.Second))
				flusher.Flush()
				timer := time.NewTimer(minDuration(wait, 30*time.Second))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			writeSSEError(w, flusher, "All models are currently unavailable")
			return
		}

		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, timeout, tokens)
		// StreamToClient buffers the start of the stream and only flushes to the
		// client once the first content token (or tool call) arrives, so an early
		// upstream error is swallowed and we fall back with no output sent to the
		// client. The size-based timeout guards only time-to-first-token; the
		// parent context keeps the connection alive for the rest of a long
		// generation.
		start := time.Now()
		partial, toolCalls, completionTokens, err := g.proxy.StreamToClient(ctx, w, flusher, body, *ep, timeout)
		if err == nil {
			g.router.RecordSuccess(ep)
			if dur := time.Since(start); dur > 0 && completionTokens > 0 {
				g.router.RecordTPS(*ep, float64(completionTokens)/dur.Seconds())
			}
			return
		}

		if isClientDisconnect(err) {
			// Client went away; nothing to resume and no point cooling a model.
			return
		}

		// Mid-stream failure after we already flushed content to the client:
		// resume on the next model by replaying what the client already received
		// as an assistant message (content and any tool calls), so it continues
		// instead of regenerating (which would duplicate output). If nothing was
		// flushed yet (failure before the first token) we just retry with the
		// unchanged body.
		g.router.ApplyCooldownFromError(ep, err)
		if strings.TrimSpace(partial) != "" || len(toolCalls) > 0 {
			body = appendAssistantMessage(body, partial, toolCalls)
		}
		continue
	}
}

func (g *GatewayContext) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func (g *GatewayContext) HandleAdminProviders(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	type ProviderInfo struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		HasKey bool   `json:"has_key"`
	}
	providers := make([]ProviderInfo, 0, len(g.config.Load().Providers))
	for name, p := range g.config.Load().Providers {
		providers = append(providers, ProviderInfo{
			Name:   name,
			URL:    p.URL,
			HasKey: p.APIKey() != "",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers": providers,
	})
}

func (g *GatewayContext) HandleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	sessions := g.router.GetAllSessions()
	type SessionInfo struct {
		SessionID string `json:"session_id"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	result := make([]SessionInfo, 0, len(sessions))
	for sid, ep := range sessions {
		result = append(result, SessionInfo{
			SessionID: sid,
			Provider:  ep.Provider,
			Model:     ep.Model,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessions": result,
		"count":    len(result),
	})
}

func (g *GatewayContext) HandleAdminCooldowns(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	cooldowns := g.router.GetAllCooldowns()
	type CooldownInfo struct {
		ModelKey   string    `json:"model_key"`
		Expiry     time.Time `json:"expiry"`
		StatusCode int       `json:"status_code"`
		ErrorCount int       `json:"error_count"`
		LastError  string    `json:"last_error"`
	}
	result := make([]CooldownInfo, 0, len(cooldowns))
	for key, cd := range cooldowns {
		result = append(result, CooldownInfo{
			ModelKey:   key,
			Expiry:     cd.Expiry,
			StatusCode: cd.StatusCode,
			ErrorCount: cd.ErrorCount,
			LastError:  cd.LastError,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cooldowns": result,
		"count":     len(result),
	})
}

// ReloadConfig swaps the in-memory configuration used by the gateway, router
// and proxy without restarting the server. Sessions and cooldowns are kept, so
// sticky routing and backoff state survive a reload. Used by the admin config
// endpoint and the on-disk config file watcher.
func (g *GatewayContext) ReloadConfig(cfg *Config) {
	g.config.Store(cfg)
	g.router.config.Store(cfg)
	g.proxy.config.Store(cfg)
	log.Printf("[debug] config reloaded: %d providers, %d logical models", len(cfg.Providers), len(cfg.Models))
}

func (g *GatewayContext) HandleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		// Return config without exposing secrets (api_key_env only)
		// Config struct is safe to marshal
		json.NewEncoder(w).Encode(g.config.Load())
	case http.MethodPost, http.MethodPut:
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
			return
		}
		if err := SaveConfig(g.configPath, &cfg); err != nil {
			writeAPIError(w, 400, err.Error(), "invalid_request_error", "validation_error")
			return
		}
		// Reload in memory
		newCfg, err := LoadConfig(g.configPath)
		if err != nil {
			writeAPIError(w, 500, "Failed to reload config", "server_error", "reload_failed")
			return
		}
		g.ReloadConfig(newCfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
	}
}

func (g *GatewayContext) HandleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	// Simple static dashboard HTML
	html := `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>AI Router Dashboard</title>
<style>
body{font-family:system-ui,sans-serif;margin:0;padding:0;background:#f6f7fb;color:#111}
header{background:#0f172a;color:#fff;padding:1rem 2rem;display:flex;justify-content:space-between;align-items:center}
main{padding:2rem}
.card{background:#fff;border-radius:12px;box-shadow:0 1px 3px rgba(0,0,0,.1);padding:1.5rem;margin-bottom:1.5rem}
h2{margin-top:0}
textarea{background:#0f172a;color:#e2e8f0;padding:1rem;border-radius:8px;overflow:auto;font-family:ui-monospace,monospace}
button{background:#2563eb;color:#fff;border:none;padding:.6rem 1rem;border-radius:8px;cursor:pointer;margin-right:.5rem}
button:disabled{opacity:.5}
</style>
</head>
<body>
<header><h1>AI Router Dashboard</h1><span id="status"></span></header>
<main>
<div class="card">
<h2>Config Editor</h2>
<p>Load and edit config (JSON). Save writes back to config.yaml and reloads.</p>
<button id="load">Load Config</button>
<button id="save" disabled>Save Config</button>
<textarea id="editor" rows="30" style="width:100%;"></textarea>
</div>
<div class="card">
<h2>Quick View</h2>
<div id="summary"></div>
</div>
</main>
<script>
const apiKey = prompt('Enter gateway API key for admin:') || '';
async function req(path, opts={}) {
  const headers = Object.assign({'Content-Type':'application/json'}, opts.headers||{});
  if(apiKey) headers['Authorization']='Bearer '+apiKey;
  const res = await fetch(path,{...opts,headers});
  if(!res.ok){ const t=await res.text(); throw new Error(t); }
  return res.headers.get('content-type')?.includes('application/json')? res.json(): res.text();
}
let cfg=null;
const editor=document.getElementById('editor');
document.getElementById('load').onclick=async()=>{
  try{
    cfg=await req('/admin/config');
    editor.value=JSON.stringify(cfg,null,2);
    document.getElementById('save').disabled=false;
    renderSummary(cfg);
    document.getElementById('status').textContent='Loaded';
  }catch(e){ alert(e); }
};
document.getElementById('save').onclick=async()=>{
  try{
    const parsed=JSON.parse(editor.value);
    await req('/admin/config',{method:'POST',body:JSON.stringify(parsed)});
    alert('Saved and reloaded');
    document.getElementById('status').textContent='Saved';
  }catch(e){ alert('Save failed: '+e); }
};
function renderSummary(c){
  const providers=Object.keys(c.providers||{}).length;
  const models=Object.keys(c.models||{}).length;
  document.getElementById('summary').innerHTML='<b>Providers:</b> '+providers+'<br><b>Logical models:</b> '+models;
}
document.getElementById('load').click();
</script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'")
	w.Write([]byte(html))
}

var (
	sessionIDMu      sync.Mutex
	sessionIDCounter int64
)
