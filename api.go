package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
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
	Role      string                   `json:"role"`
	Content   string                   `json:"content"`
	Name      string                   `json:"name,omitempty"`
	ToolCalls []ChatCompletionToolCall `json:"tool_calls,omitempty"`

	// ImageURLs holds image references for a multimodal message. When it is
	// non-empty MarshalJSON emits content as the array form
	// ([{"type":"text",...},{"type":"image_url",...}]) instead of a bare
	// string, and drops the separate Content string, since the two forms are
	// mutually exclusive on the wire. It is populated by the Responses
	// translation, which is the only surface that produces images.
	ImageURLs []string `json:"-"`
}

type ChatCompletionToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// UnmarshalJSON tolerates both string content (standard) and array content
// (vision / multimodal requests, where content is a list of text/image parts).
// The raw request body is forwarded verbatim to providers, so we only need
// Content as a string for token estimation; for array content we extract the
// concatenated text of text parts so the estimate is realistic (storing the
// raw JSON blob would massively over-count tokens).
func (m *ChatCompletionMessage) UnmarshalJSON(data []byte) error {
	var a struct {
		Role      string                   `json:"role"`
		Content   json.RawMessage          `json:"content"`
		Name      string                   `json:"name,omitempty"`
		ToolCalls []ChatCompletionToolCall `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	m.Role, m.Name = a.Role, a.Name
	// ToolCalls must be carried through: a custom UnmarshalJSON that ignores
	// them silently drops every tool call in a decoded response, which breaks
	// tool-result round trips on both the chat and Responses surfaces.
	m.ToolCalls = a.ToolCalls
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

// MarshalJSON emits the multimodal array form of content when ImageURLs is
// populated, and the ordinary string form otherwise.
func (m ChatCompletionMessage) MarshalJSON() ([]byte, error) {
	if len(m.ImageURLs) == 0 {
		return json.Marshal(struct {
			Role      string                   `json:"role"`
			Content   string                   `json:"content"`
			Name      string                   `json:"name,omitempty"`
			ToolCalls []ChatCompletionToolCall `json:"tool_calls,omitempty"`
		}{m.Role, m.Content, m.Name, m.ToolCalls})
	}
	parts := make([]map[string]any, 0, len(m.ImageURLs)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, u := range m.ImageURLs {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": u},
		})
	}
	return json.Marshal(struct {
		Role      string                   `json:"role"`
		Content   []map[string]any         `json:"content"`
		Name      string                   `json:"name,omitempty"`
		ToolCalls []ChatCompletionToolCall `json:"tool_calls,omitempty"`
	}{m.Role, parts, m.Name, m.ToolCalls})
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
	ID           string `json:"id"`
	Object       string `json:"object"`
	Created      int64  `json:"created"`
	OwnedBy      string `json:"owned_by"`
	// MaxContextTokens is the advertised context window (input capacity) of
	// this logical model, in tokens. It is derived from the smallest
	// context_length in the fallback chain, so it is safe for every backend
	// the profile might relay to, including fallbacks after a failure.
	//
	// It is deliberately NOT named max_tokens: in OpenAI's chat-completions
	// API max_tokens means the OUTPUT ceiling, and we have no output-token
	// data for any backend (the upstream model lists don't publish one, and
	// neither does OpenAI's own /v1/models schema). Putting the context
	// window under max_tokens would be a lie the first time a client used it
	// to bound its generation.
	MaxContextTokens *int `json:"max_context_tokens,omitempty"`
}

type GatewayContext struct {
	router        *Router
	proxy         *Proxy
	config        *atomic.Pointer[Config]
	configPath    string
	gatewayAPIKey string
	allowNoAuth   bool          // explicit opt-in to unauthenticated mode (-allow-no-auth)
	testCooldown  time.Duration // override for tests: forces all cooldowns to this duration
	metrics       *Metrics      // Prometheus-style metrics; nil in tests that don't need it
}

func NewGatewayContext(router *Router, proxy *Proxy, cfg *Config, configPath, gatewayAPIKey string, allowNoAuth ...bool) *GatewayContext {
	g := &GatewayContext{
		router:        router,
		proxy:         proxy,
		configPath:    configPath,
		gatewayAPIKey: gatewayAPIKey,
		metrics:       NewMetrics(),
	}
	if len(allowNoAuth) > 0 {
		g.allowNoAuth = allowNoAuth[0]
	}
	// Share ONE atomic.Pointer[Config] across gateway, router and proxy so a
	// config reload is a single atomic store that all three observe together.
	// Without this, ReloadConfig would swap three independent pointers with no
	// synchronization, and an in-flight request could observe a mix (e.g.
	// routing from the new chain while forwarding through old provider URLs),
	// which is undefined and can misroute or 404.
	shared := &atomic.Pointer[Config]{}
	shared.Store(cfg)
	g.config = shared
	g.router.config = shared
	g.proxy.config = shared
	return g
}

// SetTestCooldown overrides the cooldown duration returned by
// ApplyCooldownForSession for testing. Zero disables the override.
func (g *GatewayContext) SetTestCooldown(d time.Duration) {
	g.testCooldown = d
}

// recordRequest records a request with a known model name. It is a no-op when
// metrics is nil (test contexts that never construct a real gateway).
func (g *GatewayContext) recordRequest(model string, status int, latency time.Duration) {
	if g.metrics != nil {
		g.metrics.Request(model, status, latency)
	}
}

// recordFallback increments the fallback counter. The optional from
// endpoint is the model that failed and triggered the fail-over; when
// provided it is recorded per-endpoint so operators can identify the
// weakest link in a chain.
func (g *GatewayContext) recordFallback(ep ...*ModelEndpoint) {
	if g.metrics != nil {
		if len(ep) > 0 && ep[0] != nil {
			g.metrics.Fallback(ep[0].Key())
		} else {
			g.metrics.Fallback("")
		}
	}
}

// recordCooldown increments the cooldown counter. It is a no-op when
// metrics is nil.
func (g *GatewayContext) recordCooldown() {
	if g.metrics != nil {
		g.metrics.Cooldown()
	}
}

// HandleMetrics serves the Prometheus text-format metrics endpoint.
func (g *GatewayContext) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if g.metrics == nil {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	g.metrics.ServeHTTP(w, r)
}

func (g *GatewayContext) checkAuth(r *http.Request) bool {
	// Fail closed unless the operator explicitly opted into unauthenticated
	// mode with -allow-no-auth. An empty key with allowNoAuth=false means
	// auth was never configured correctly, so every request (including
	// admin) must be rejected. Production is additionally guarded by main.go,
	// which refuses to start without a key unless the flag is passed — this
	// is defense-in-depth so a misconfigured instance can't silently serve
	// everything unauthenticated even if it somehow reaches a handler.
	if g.gatewayAPIKey == "" {
		return g.allowNoAuth
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
	// ConstantTimeCompare requires equal-length inputs; always run it to
	// avoid leaking the key length via timing. Pad the shorter input to
	// match the longer one (the comparison will fail, but in constant time).
	if len(want) == len(got) {
		return subtle.ConstantTimeCompare(want, got) == 1
	}
	// Lengths differ: run ConstantTimeCompare on equal-length slices so the
	// timing doesn't reveal the key length. Result is always false.
	if len(got) < len(want) {
		got = append(got, make([]byte, len(want)-len(got))...)
	} else {
		want = append(want, make([]byte, len(got)-len(want))...)
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

// bodySessionID derives a stable session identifier from a request body by
// hashing the logical model and the full conversation messages array (role +
// content). Two requests with the same model and identical message history
// will hash to the same ID, which means:
//
//   - A client re-sending the same request (e.g. after a network timeout
//     before any flush reached the client) gets the same session and the
//     same sticky model choice, so the request is idempotent from the
//     gateway's perspective.
//   - Mid-stream failures that the gateway retries by re-issuing the request
//     with an appended assistant message continue the session, because the
//     retry loop captures sessionID once before the loop and never recomputes
//     it from the mutated body.
//   - Each distinct conversation (different messages or model) maps to its
//     own session, so different users and conversations never share a
//     session or a model choice.
//
// Fields that don't affect the conversation identity (temperature, stream,
// max_tokens, metadata, etc.) are intentionally NOT part of the fingerprint,
// so a client that re-sends the same conversation with minor knobs changed
// still gets the same session.
//
// The hash is content-addressed, so there's no risk of an unauthenticated
// client injecting a chosen ID to manipulate routing.
func bodySessionID(body []byte) string {
	// Only look at fields that identify the conversation: model + messages.
	// We re-parse just those two fields rather than stripping the rest, so
	// the result is well-defined regardless of other fields' presence or
	// ordering.
	var partial struct {
		Model    string                   `json:"model"`
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(body, &partial); err != nil || partial.Model == "" {
		// Malformed body or missing model: fall back to a content hash of
		// the whole body so the request still gets routed (the chat handler
		// will return 400 anyway, but routing must not panic).
		sum := sha256.Sum256(body)
		return "body:" + hex.EncodeToString(sum[:])
	}

	// Build a deterministic byte stream from model + messages. We include
	// content so different conversations with the same role layout don't
	// collide.
	h := sha256.New()
	h.Write([]byte(partial.Model))
	h.Write([]byte{0})
	if err := json.NewEncoder(h).Encode(partial.Messages); err != nil {
		// json.Encoder.Encode on a []map can't fail in practice, but fall
		// back to a full-body hash so routing never panics.
		sum := sha256.Sum256(body)
		return "body:" + hex.EncodeToString(sum[:])
	}
	return "ctx:" + hex.EncodeToString(h.Sum(nil)[:16])
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
	// Sanitize: SSE events are delimited by newlines, so a multi-line
	// message would break framing. Collapse to a single line.
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
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

	cfg := g.config.Load()
	models := make([]Model, 0, len(LogicalModels))
	now := time.Now().Unix()
	for _, name := range LogicalModels {
		m := Model{
			ID:      name,
			Object:  "model",
			Created: now,
			OwnedBy: "airouter",
		}
		// Advertise the conservative context window: the smallest
		// context_length in the fallback chain. This is the value that is
		// safe for EVERY backend the profile might relay to, including
		// fallbacks after a failure. It is exposed as
		// max_context_tokens (not max_tokens) because we have no
		// output-token data for any backend — the upstream model lists
		// don't publish one, and neither does OpenAI's own /v1/models
		// schema.
		chain, ok := cfg.Models[name]
		if ok {
			var minCtx int
			for _, ep := range chain.Chain {
				w := ep.ContextWindow()
				if w <= 0 {
					continue
				}
				if minCtx == 0 || w < minCtx {
					minCtx = w
				}
			}
			if minCtx > 0 {
				m.MaxContextTokens = &minCtx
			}
		}
		models = append(models, m)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelListResponse{
		Object: "list",
		Data:   models,
	})
}

func (g *GatewayContext) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, 400, "Failed to read request body", "invalid_request_error", "bad_request")
		g.recordRequest("", 400, time.Since(start))
		return
	}

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		g.recordRequest("", 400, time.Since(start))
		return
	}

	if !IsValidModel(req.Model) {
		writeAPIError(w, 400, fmt.Sprintf("Unknown model: %s. Available: %s", req.Model, strings.Join(LogicalModels, ", ")), "invalid_request_error", "model_not_found")
		g.recordRequest("", 400, time.Since(start))
		return
	}

	sessionID := bodySessionID(body)

	// Inject default max_tokens if the client omitted it. The default is
	// the smaller of: (profile's min context window / 2), or a positive
	// value from config.DEFAULT_MAX_TOKENS. A value of 0 means "never
	// default" — let the provider pick. We check req.MaxTokens (parsed)
	// since that also handles the Responses API's max_output_tokens.
	if req.MaxTokens == nil {
		if def := g.defaultMaxTokensForModel(req.Model); def > 0 {
			body = injectMaxTokens(body, def)
		}
	}

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
return bytes.Contains(body, []byte(`"type":"image_url"`))
}

// defaultMaxTokensForModel returns the output-token ceiling the gateway
// applies to requests that omit max_tokens. Returns 0 if the client should
// be left alone (no default configured, or explicit 0 = never default).
func (g *GatewayContext) defaultMaxTokensForModel(model string) int {
prefs := g.config.Load().Preferences
def := prefs.DefaultMaxTokensValue()
if def < 0 {
	// auto: half the profile's advertised context window
	chain, ok := g.config.Load().Models[model]
	if !ok {
		return 0
	}
	var minCtx int
	for _, ep := range chain.Chain {
		w := ep.ContextWindow()
		if w <= 0 {
			continue
		}
		if minCtx == 0 || w < minCtx {
			minCtx = w
		}
	}
	if minCtx <= 0 {
		return 0
	}
	return minCtx / 2
}
return def
}

// injectMaxTokens sets max_tokens on the raw request body. It preserves all
// other fields and their ordering, and returns the original body unchanged
// on any parse failure (fail-open so a request is never dropped).
func injectMaxTokens(body []byte, maxTokens int) []byte {
var data map[string]json.RawMessage
if err := json.Unmarshal(body, &data); err != nil {
	return body
}
if _, ok := data["max_tokens"]; ok {
	return body
}
val, err := json.Marshal(maxTokens)
if err != nil {
	return body
}
data["max_tokens"] = val
out, err := json.Marshal(data)
if err != nil {
	return body
}
return out
}

// appendAssistantMessage returns body with an assistant message appended to the
// messages array. It is used to resume a stream on the next model after the
// previous one failed mid-generation: the partial output (and any tool calls)
// already sent to the client is replayed so the new model continues rather than
// regenerating from scratch. On any parse failure it returns the original body
// unchanged (fail-open so a request is never dropped).
//
// Incomplete tool calls (those with no function name yet) are dropped: they
// represent deltas where only the id arrived but the name never did, and
// replaying them as `{"function":{"arguments":...}}` with no name would produce
// a malformed tool call that breaks the next model's parser.
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
			// Skip incomplete tool calls: a name with no arguments (or no name
			// at all) produces an invalid tool call event. The streamer only
			// emits complete tool calls to the client, but the fallback
			// accumulator also holds partial deltas for replay. Replaying an
			// incomplete one would break the next model's parser.
			if tc.Name == "" {
				continue
			}
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
		if len(atcs) > 0 {
			msg["tool_calls"] = atcs
		}
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
// 5s for up to 1000 tokens, plus 1s for each additional 10000 tokens (ceil
// division). It guards only time-to-first-token for streaming; the idle reader
// bounds the rest.
func requestTimeout(tokens int) time.Duration {
	const base = 5 * time.Second
	if tokens <= 1000 {
		return base
	}
	extra := (tokens - 1000 + 9999) / 10000 // ceil division, 1s per extra 10k tokens
	return base + time.Duration(extra)*time.Second
}

// maxFallbackWallClock bounds the total time a single request may spend
// walking its chain, independent of how many endpoints the chain contains.
//
// The per-attempt loop is bounded by maxAttempts = chainLen*3+1, which for the
// shipped config is 82 (smart), 145 (work), 115 (large). Combined with a 5s
// per-attempt timeout for a small request, that is a theoretical 7 minutes of
// retrying before a client sees anything. Measured on 2026-09-29: a 4-token
// `smart` request whose providers were all timing out took 62.6s, walking 20+
// endpoints at 5s apiece. The client just sees a hung request.
//
// Cooldowns normally mask this — a timing-out endpoint is skipped on the next
// request — but the very first request after a restart or a bulk cooldown
// clear pays the full walk, which is exactly when a user is watching.
//
// Attempts are also skipped entirely once this budget is spent, so the
// fallback still degrades gracefully; it just stops grinding.
const maxFallbackWallClock = 45 * time.Second

// fallbackBudget returns the wall-clock ceiling for one request's fallback
// walk, scaled to the size of the request.
//
// The 45s ceiling above is the worst case, appropriate for a large-context
// request where each attempt legitimately gets a 17s+ per-attempt timeout. A
// small request is different: requestTimeout() gives it only 5s per attempt, so
// walking a long chain is 5s of *dead time* per endpoint. That is the case that
// was measured at 62.6s (4-token `smart` request, 20+ nvidia endpoints) and
// then 22.9s after the flat bound was added. For a small request 22.9s is still
// a user staring at a spinner, and it buys very little: the endpoints that
// matter for a short prompt are near the head of the chain.
//
// So scale the budget to what the request can actually justify. A small
// request gets a short one, and because the per-attempt timeout is also small,
// the walk still covers many endpoints — just not dozens.
//
// The budget always allows at least two full attempts where that fits inside
// the ceiling, so a single transient failure never ends a request. It is
// clamped to maxFallbackWallClock: for a very large context a single attempt
// can already approach 45s on its own, and letting 2*timeout through would put
// the effective ceiling back at minutes — the exact failure the ceiling exists
// to prevent.
func fallbackBudget(timeout time.Duration) time.Duration {
	budget := maxFallbackWallClock
	switch {
	case timeout <= 5*time.Second:
		// Small request: 12s buys ~2 attempts at 5s plus the fast successes
		// that usually come back in well under a second.
		budget = 12 * time.Second
	case timeout <= 8*time.Second:
		budget = 20 * time.Second
	}
	if min := 2 * timeout; budget < min && min <= maxFallbackWallClock {
		budget = min
	}
	return budget
}

func (g *GatewayContext) handleCompletion(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	start := time.Now()
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)
	// Wall-clock budget for this request's fallback walk, scaled to the
	// per-attempt timeout (small requests get a short budget).
	budget := fallbackBudget(timeout)
	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	attempts := 0

	// tried tracks endpoints that have failed in THIS request's fallback loop.
	// It is local to this goroutine so concurrent requests with the same
	// session ID don't interfere with each other's endpoint selection.
	tried := map[string]bool{}

	for {
		if ctx.Err() != nil {
			writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		// Wall-clock guard: stop grinding through the chain once this request
		// has spent its whole fallback budget, so a client never waits
		// minutes on a chain of failing providers.
		if attempts > 0 && time.Since(start) > budget {
			log.Printf("[debug] session=%s model=%s -> fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, attempts, time.Since(start).Round(time.Millisecond), budget)
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
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
					g.recordRequest(req.Model, 200, time.Since(start))
					return
				case VisionUnavailable:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision currently not available"))
					g.recordRequest(req.Model, 200, time.Since(start))
					return
				}
			}
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body), tried)
		if ep == nil {
			requireVision := requestHasVision(body)
			if requireVision {
				state, _ := g.router.VisionChainStatus(req.Model, sessionID)
				switch state {
				case VisionUnsupported:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision not supported"))
					g.recordRequest(req.Model, 200, time.Since(start))
					return
				case VisionUnavailable:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(visionRejectionReply(req.Model, "Vision currently not available"))
					g.recordRequest(req.Model, 200, time.Since(start))
					return
				}
			}
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
		start := time.Now()
		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, timeout, tokens)
		resp, err := g.proxy.Forward(reqCtx, body, *ep, sessionID)
		if 		err != nil {
			cancel()
			if isClientDisconnect(err) {
				writeAPIError(w, 499, "Client disconnected", "server_error", "client_disconnected")
				g.recordRequest(req.Model, 499, time.Since(start))
				return
			}
			tried[ep.Key()] = true
			g.recordFallback(ep)
			g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
			}
			continue
		}
		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			tried[ep.Key()] = true
			g.recordFallback(ep)
			g.router.ApplyCooldownForSession(ep, resp.StatusCode, string(respBody), sessionID, ParseRetryAfter(resp.Header.Get("Retry-After")))
			g.recordCooldown()
			if g.testCooldown > 0 {
				// Override the cooldown for testing: force the
				// endpoint's cooldown expiry to now+testCooldown
				// so the handler doesn't sleep for hours.
				g.router.ApplyCooldownWithDuration(ep, resp.StatusCode, string(respBody), sessionID, g.testCooldown)
			}
			g.recordRequest(req.Model, resp.StatusCode, time.Since(start))
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
		if _, writeErr := w.Write(respBody); writeErr != nil {
			// Most likely the client disconnected; log for observability but
			// don't penalize a healthy model.
			log.Printf("[debug] session=%s model=%s -> write error (client likely disconnected): %v", sessionID, req.Model, writeErr)
			g.recordRequest(req.Model, 499, time.Since(start))
			return
		}
		g.recordRequest(req.Model, 200, time.Since(start))
		return
	}
}

func (g *GatewayContext) handleStream(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
start := time.Now()
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)
	// Wall-clock budget for this request's fallback walk, scaled to the
	// per-attempt timeout (small requests get a short budget).
	budget := fallbackBudget(timeout)

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

	// tried tracks endpoints that have failed in THIS request's fallback loop.
	// It is local to this goroutine so concurrent requests with the same
	// session ID (e.g. duplicate client retries) don't interfere with each
	// other's endpoint selection through a shared tried set.
	tried := map[string]bool{}

	// doneSent tracks whether we've already written the [DONE] sentinel to
	// the client. We must always send it before returning so the client
	// parser doesn't hang waiting for stream termination.
	doneSent := false

	// accumulatedContent tracks the total content already replayed across
	// fallback attempts. On each failed attempt, partial contains the full
	// output from that model (which includes prior replayed context). We
	// only append the delta (new content not yet in the body) to prevent
	// duplicated text from accumulating across fallbacks.
	var accumulatedContent string

	// accumulatedTCs tracks tool call state across fallbacks so that
	// partially-received tool calls persist across model switches.
	var accumulatedTCs []streamToolCall
	sendDone := func() {
		if doneSent {
			return
		}
		doneSent = true
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
	defer sendDone()

	for {
		// Wall-clock guard, mirroring the non-streaming path: a streaming
		// request walking a long failing chain must not hold the client open
		// for minutes. accumulatedContent is non-empty once we have replayed
		// anything to the client, in which case writing a fresh error into the
		// stream body would corrupt an already-started response — so just
		// finish the stream cleanly instead.
		if attempts > 0 && time.Since(start) > budget {
			log.Printf("[debug] session=%s model=%s -> stream fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, attempts, time.Since(start).Round(time.Millisecond), budget)
			if accumulatedContent == "" {
				writeSSEError(w, flusher, "All models are currently unavailable")
			}
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		if ctx.Err() != nil {
			// Context cancelled (client disconnect or timeout). Log the cause
			// for debugging; the deferred sendDone() will terminate the stream.
			g.recordRequest(req.Model, 499, time.Since(start))
			return
		}
		if attempts >= maxAttempts {
			if requestHasVision(body) {
				state, _ := g.router.VisionChainStatus(req.Model, sessionID)
				switch state {
				case VisionUnsupported:
					writeVisionRejectionSSE(w, flusher, req.Model, "Vision not supported")
					doneSent = true
					g.recordRequest(req.Model, 200, time.Since(start))
					return
				case VisionUnavailable:
					writeVisionRejectionSSE(w, flusher, req.Model, "Vision currently not available")
					doneSent = true
					g.recordRequest(req.Model, 200, time.Since(start))
					return
				}
			}
			writeSSEError(w, flusher, "All models are currently unavailable")
			// sendDone fires via defer
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body), tried)
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
					doneSent = true
					g.recordRequest(req.Model, 200, time.Since(start))
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

		// StreamToClient buffers the start of the stream and only flushes to the
		// client once the first content token (or tool call) arrives, so an early
		// upstream error is swallowed and we fall back with no output sent to the
		// client. The size-based timeout guards only time-to-first-token; the
		// parent context keeps the connection alive for the rest of a long
		// generation.
		start := time.Now()
		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, timeout, tokens)
		partial, toolCalls, completionTokens, err := g.proxy.StreamToClient(ctx, w, flusher, body, *ep, timeout, sessionID)
		if err == nil {
			g.router.RecordSuccess(ep)
			if dur := time.Since(start); dur > 0 && completionTokens > 0 {
				g.router.RecordTPS(*ep, float64(completionTokens)/dur.Seconds())
			}
			// StreamToClient emits [DONE] on success; mark as sent so the
			// deferred sendDone() doesn't emit a duplicate.
			doneSent = true
			g.recordRequest(req.Model, 200, time.Since(start))
			return
		}

		if isClientDisconnect(err) {
			// Client went away; nothing to resume and no point cooling a model.
			g.recordRequest(req.Model, 499, time.Since(start))
			return
		}

		// Mark this endpoint as tried in THIS request's local fallback set so
		// we don't retry it within the same loop. Unlike the old shared
		// triedKeys approach, this doesn't leak state to other concurrent
		// requests that share the same session ID.
		tried[ep.Key()] = true
		g.recordFallback(ep)
		if g.testCooldown > 0 {
			g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
		} else {
			g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
		}
		g.recordCooldown()
		// Mid-stream failure after we already flushed content to the client:
		// resume on the next model by replaying what the client already received
		// as an assistant message (content and any tool calls), so it continues
		// instead of regenerating (which would duplicate output). If nothing was
		// flushed yet (failure before the first token) we just retry with the
		// unchanged body.
		//
		// SAFETY: body grows by one assistant message per failed attempt. This
		// is bounded by maxAttempts (chainLen*3 + 1, set above). For a typical
		// 3-element chain that's 10 attempts max. Each assistant message is
		// small (partial output from one attempt), so memory stays bounded.
		if strings.TrimSpace(partial) != "" || len(toolCalls) > 0 {
			// Compute the content delta: only the new content not yet replayed.
			// If the model continued from the replayed context, partial will
			// start with accumulatedContent and we strip that prefix.
			contentDelta := partial
			if strings.HasPrefix(partial, accumulatedContent) {
				contentDelta = partial[len(accumulatedContent):]
			}
			// Accumulate tool calls across fallbacks. Merge new tool calls
			// into the accumulator so partial tool call deltas survive
			// across model switches.
			for _, tc := range toolCalls {
				found := false
				for i := range accumulatedTCs {
					if accumulatedTCs[i].Index == tc.Index {
						if tc.ID != "" {
							accumulatedTCs[i].ID = tc.ID
						}
						if tc.Type != "" {
							accumulatedTCs[i].Type = tc.Type
						}
						if tc.Name != "" {
							accumulatedTCs[i].Name = tc.Name
						}
						accumulatedTCs[i].Arguments += tc.Arguments
						found = true
						break
					}
				}
				if !found {
					accumulatedTCs = append(accumulatedTCs, tc)
				}
			}
			body = appendAssistantMessage(body, contentDelta, accumulatedTCs)
			accumulatedContent = partial
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
	// DELETE clears backoff state. Without it the only way to clear a
	// cooldown is to stop the daemon, edit cooldowns.json by hand, and start
	// it again — and that hand-edit is silently reverted by the running
	// router, which rewrites the file from its in-memory copy.
	//
	// Targets come from the query string so a mistaken bulk clear is at least
	// explicit:   DELETE /admin/cooldowns?model=<provider:model>
	//              DELETE /admin/cooldowns            (clears everything)
	if r.Method == http.MethodDelete {
		modelKey := r.URL.Query().Get("model")
		removed := g.router.ClearCooldowns(modelKey)
		if modelKey != "" && removed == 0 {
			writeAPIError(w, 404, "No cooldown for model "+modelKey, "not_found", "cooldown_not_found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"cleared": removed,
			"model":   modelKey,
		})
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	cooldowns := g.router.GetAllCooldowns()
	circuits := g.router.GetAllCircuits()
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
	type CircuitInfo struct {
		ModelKey   string       `json:"model_key"`
		State      string       `json:"state"`
		OpenedAt   time.Time    `json:"opened_at"`
		ProbesSent int          `json:"probes_sent"`
	}
	circuitsResult := make([]CircuitInfo, 0, len(circuits))
	for key, cb := range circuits {
		circuitsResult = append(circuitsResult, CircuitInfo{
			ModelKey:   key,
			State:      cb.State.String(),
			OpenedAt:   cb.OpenedAt,
			ProbesSent: cb.ProbesSent,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cooldowns":  result,
		"circuits":   circuitsResult,
		"count":      len(result),
	})
}

// HandleAirouterState returns a unified snapshot of every endpoint's circuit
// breaker state, cooldown backoff window, and remaining cooldown duration,
// computed against the current wall clock. Unlike /admin/cooldowns (raw
// cooldown entries) and /admin/sessions (sticky routing), this endpoint
// answers "is this model currently failing and how long until it recovers?"
// in a single call.
//
// This is an unauthenticated convenience endpoint (like /health) because it
// carries no secrets — only operational state. Operators that need auth on
// every endpoint should sit a reverse proxy in front.
func (g *GatewayContext) HandleAirouterState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"circuits": g.router.GetAllCircuitsState(),
		"count":    len(g.router.GetAllCircuitsState()),
	})
}

// ReloadConfig swaps the in-memory configuration used by the gateway, router
// and proxy without restarting the server. Sessions and cooldowns are kept, so
// sticky routing and backoff state survive a reload. Used by the admin config
// endpoint and the on-disk config file watcher.
func (g *GatewayContext) ReloadConfig(cfg *Config) {
	g.config.Store(cfg)
	// Jitter is a Router-level knob, not part of the shared config pointer, so
	// push it across explicitly on every reload. Without this a config change
	// to cooldown_jitter would silently no-op until restart.
	g.router.SetCooldownJitter(cfg.Preferences.CooldownJitterFraction())
	providers := 0
	models := 0
	if cfg != nil {
		providers = len(cfg.Providers)
		models = len(cfg.Models)
	}
	log.Printf("[debug] config reloaded: %d providers, %d logical models", providers, models)
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
