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
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const maxRequestBytes = 32 << 20 // 32 MiB

// proxyCooldown is how long a proxy is cooled down after a transport-layer
// failure. The endpoint is not penalized for proxy errors -- only the proxy is.
const proxyCooldown = 30 * time.Second

// maxSessionInterruption is the contract this gateway makes to a client: once
// a session has stopped receiving output, it either gets resumed output from
// another model in the fallback chain or gets a terminal failure within 30s.
// It is the ceiling applied to every failure/cooldown wait, and every attempt
// is clamped to the interruption budget left, so the cumulative walk cannot
// exceed it either.
//
// It is also the hard ceiling on the per-provider `max_interruption` setting:
// a provider may shorten the window, never lengthen it past this.
const maxSessionInterruption = 30 * time.Second

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
	ReasoningEffort  *string                 `json:"reasoning_effort,omitempty"`
}

type ChatCompletionMessage struct {
	Role             string                   `json:"role"`
	Content          string                   `json:"content"`
	ReasoningContent string                   `json:"reasoning_content,omitempty"`
	Name             string                   `json:"name,omitempty"`
	ToolCalls        []ChatCompletionToolCall `json:"tool_calls,omitempty"`

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
		Role             string                   `json:"role"`
		Content          json.RawMessage          `json:"content"`
		ReasoningContent string                   `json:"reasoning_content,omitempty"`
		Name             string                   `json:"name,omitempty"`
		ToolCalls        []ChatCompletionToolCall `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	m.Role, m.Name, m.ReasoningContent = a.Role, a.Name, a.ReasoningContent
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
			Role             string                   `json:"role"`
			Content          string                   `json:"content"`
			ReasoningContent string                   `json:"reasoning_content,omitempty"`
			Name             string                   `json:"name,omitempty"`
			ToolCalls        []ChatCompletionToolCall `json:"tool_calls,omitempty"`
		}{m.Role, m.Content, m.ReasoningContent, m.Name, m.ToolCalls})
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
		Role             string                   `json:"role"`
		Content          []map[string]any         `json:"content"`
		ReasoningContent string                   `json:"reasoning_content,omitempty"`
		Name             string                   `json:"name,omitempty"`
		ToolCalls        []ChatCompletionToolCall `json:"tool_calls,omitempty"`
	}{m.Role, parts, m.ReasoningContent, m.Name, m.ToolCalls})
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
	inflight      *inflightTracker
}

func NewGatewayContext(router *Router, proxy *Proxy, cfg *Config, configPath, gatewayAPIKey string, allowNoAuth ...bool) *GatewayContext {
	g := &GatewayContext{
		router:        router,
		proxy:         proxy,
		configPath:    configPath,
		gatewayAPIKey: gatewayAPIKey,
		metrics:       NewMetrics(),
		inflight:      newInflightTracker(),
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
	// Apply the configured label cap at construction, not only on reload. The
	// collector is created here, so this is the first chance to honour
	// max_label_cardinality; ReloadConfig keeps it in sync afterwards.
	if g.metrics != nil {
		g.metrics.SetLabelCap(cfg.Preferences.MaxLabelCardinalityValue())
	}
	return g
}

// SetTestCooldown overrides the cooldown duration returned by
// ApplyCooldownForSession for testing. Zero disables the override.
func (g *GatewayContext) SetTestCooldown(d time.Duration) {
	g.testCooldown = d
}

// providerMaxInterruption returns the interruption ceiling for the provider
// behind the named endpoint, read from the live config and clamped to the
// global maxSessionInterruption.
//
// It is the value that bounds this provider's cycle-back waits and its share
// of the fallback budget. Every fallback loop asks the endpoint it is about
// to use rather than a single global constant, so a provider configured for a
// short interruption window (a local ollama endpoint that answers in
// milliseconds) does not hold a client open for 30s on its cooldown.
func (g *GatewayContext) providerMaxInterruption(provider string) time.Duration {
	return ProviderMaxInterruption(g.config.Load(), provider)
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

// recordAttempt records one upstream attempt against an endpoint: how long it
// took and whether it succeeded.
//
// Every fallback loop calls this around its per-endpoint call. Together these
// series answer what the request-level metrics cannot: which endpoints the
// router actually reaches, how often those reaches fail, and how much
// wall-clock each one costs. That is the data needed to decide whether an
// endpoint should be demoted in the chain, or whether the fallback budget is
// set too high for the chain as it stands.
func (g *GatewayContext) recordAttempt(ep *ModelEndpoint, d time.Duration, ok bool) {
	if g.metrics != nil && ep != nil {
		g.metrics.Attempt(ep.Key(), d, ok)
	}
}

// HandleMetrics serves the Prometheus text-format metrics endpoint.
//
// Authenticated like the admin surface, not like the health check. The metric
// series are labelled by provider and model and describe routing behaviour
// (attempt counts, failure rates, which endpoint the chain is demoting to),
// which is operator telemetry rather than public health data. Serving it
// unauthenticated would let anyone on the network enumerate the configured
// upstream providers and watch them fail.
func (g *GatewayContext) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
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

	// The header is the primary form and is what every programmatic client
	// uses. It is tried first so an existing API caller is unaffected by the
	// query form below, and so a correct key in the header still wins when a
	// wrong one is lip-synced into the query string.
	auth := r.Header.Get("Authorization")
	if auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return apiKeysEqual(g.gatewayAPIKey, parts[1])
		}
		// A malformed header must not fall through to the query form: a client
		// that sent "Basic ..." and an api_key= of the wrong key would then be
		// judged on the query alone, which is the wrong answer to give for a
		// request that was already malformed.
		return false
	}

	// `?api_key=`, so the dashboard can be opened in a browser.
	//
	// A browser cannot attach an Authorization header to the URL you type or
	// to a bookmark, so without this the admin surface — roster, cooldowns,
	// metrics, dashboard — is reachable only from curl. That is a real
	// usability wall on a tool whose whole purpose is being looked at.
	//
	// The cost is stated plainly rather than buried: a query parameter shows
	// up in the browser's history, in any access log that records the request
	// line, and in the Referer of a page it navigates to. The credential is
	// therefore easier to leak than a header is, which is why the header
	// stays the primary form. It is not a weaker check — the comparison is
	// the same constant-time one — it is a credential with a wider footprint,
	// and an operator who cares should keep using curl.
	//
	// Both spellings are accepted because both are common in the wild;
	// `api_key` is the one the dashboard page documents.
	if q := r.URL.Query(); q.Has("api_key") {
		return apiKeysEqual(g.gatewayAPIKey, q.Get("api_key"))
	}
	return false
}

// apiKeysEqual compares a configured key against a presented one in constant
// time. ConstantTimeCompare requires equal-length inputs, so the shorter value
// is padded rather than rejected up front: an early length check would answer
// instantly for a wrong-length key and leak the key's length through timing,
// which is the leak this exists to avoid. The comparison always runs to
// completion.
func apiKeysEqual(want, got string) bool {
	w := []byte(want)
	x := []byte(got)
	if len(w) == len(x) {
		return subtle.ConstantTimeCompare(w, x) == 1
	}
	if len(x) < len(w) {
		x = append(x, make([]byte, len(w)-len(x))...)
	} else {
		w = append(w, make([]byte, len(x)-len(w))...)
	}
	return subtle.ConstantTimeCompare(w, x) == 1
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
	// Both are hashed where they lie in the raw body, so the fingerprint
	// costs one pass over the bytes instead of a decode-and-re-encode round
	// trip that copied the whole conversation twice. That round trip was
	// the single biggest source of garbage in the gateway: every request
	// paid it, and a long body paid it again for every fallback attempt.
	if obj, ok := scanJSONObject(body); ok {
		modelSpan, hasModel := findJSONMember(obj, "model")
		model, isString := jsonStringValue(body, modelSpan, hasModel)
		if isString && model != "" {
			// Build a deterministic byte stream from model + messages. We
			// include content so different conversations with the same role
			// layout don't collide.
			h := sha256.New()
			h.Write([]byte(model))
			h.Write([]byte{0})
			msgSpan, hasMessages := findJSONMember(obj, "messages")
			if hasMessages {
				hashJSONValue(h, body[msgSpan.Start:msgSpan.End])
			} else {
				// Keep the shape the decoder produced for a body with no
				// messages, so such a body fingerprints the same way no
				// matter which path computed it.
				h.Write([]byte("null\n"))
			}
			return "ctx:" + hex.EncodeToString(h.Sum(nil)[:16])
		}
	}

	// Malformed body or missing model: fall back to a content hash of
	// the whole body so the request still gets routed (the chat handler
	// will return 400 anyway, but routing must not panic).
	sum := sha256.Sum256(body)
	return "body:" + hex.EncodeToString(sum[:])
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
	body, err := readRequestBody(r)
	if err != nil {
		writeAPIError(w, 400, "Failed to read request body", "invalid_request_error", "bad_request")
		g.recordRequest("", 400, time.Since(start))
		return
	}

	// Only the routing knobs are decoded here. The conversation is already in
	// the raw body, and decoding it copied every message's content (the
	// message unmarshaler allocates the string) purely so the timeout
	// estimate could measure it -- about a megabyte of garbage per request on
	// a long conversation. The estimate is taken from the raw bytes instead
	// (see estimateTokensFromBody) and the body is forwarded as it arrived.
	var head struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens *int   `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		g.recordRequest("", 400, time.Since(start))
		return
	}
	req := &ChatCompletionRequest{Model: head.Model, Stream: head.Stream, MaxTokens: head.MaxTokens}

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

	tokens := estimateTokensFromBody(body)

	// A prompt larger than every reachable model's context window cannot be
	// served by any walk: the upstreams accept the connection and then produce
	// nothing until the attempt timeout fires, so the client spends the whole
	// interruption budget and gets an error anyway. Reject it up front and name
	// the largest window on offer. Endpoints with an unknown window count as
	// fitting (see contextFits), so this can only fire when every endpoint
	// advertises a window too small for the request.
	if fits, largest := g.router.ContextCapacity(req.Model, tokens); !fits {
		msg := fmt.Sprintf("Request is about %d tokens, which exceeds every %s model's context window (largest advertised: %d tokens). Reduce the conversation, or use a model whose window fits it.", tokens, req.Model, largest)
		writeAPIError(w, 400, msg, "invalid_request_error", "context_length_exceeded")
		g.recordRequest(req.Model, 400, time.Since(start))
		return
	}

	if req.Stream {
		g.handleStream(w, r, body, req, sessionID, tokens)
	} else {
		g.handleCompletion(w, r, body, req, sessionID, tokens)
	}
}

// requestBodyHeadroom is spare capacity in the request buffer. The gateway
// injects one short member (max_tokens) into bodies that omit it, and writing
// that member into the headroom avoids a second copy of the body.
const requestBodyHeadroom = 64

// readRequestBody reads the request body into a single buffer.
//
// When the client sent a Content-Length, the buffer is sized for it up front.
// io.ReadAll grows its buffer by doubling, so a 500 KB body costs about a
// megabyte of garbage -- and the gateway sees bodies that size on every
// request, from every client.
func readRequestBody(r *http.Request) ([]byte, error) {
	if r.ContentLength <= 0 {
		// Chunked or unknown length: nothing to size the buffer from.
		return io.ReadAll(r.Body)
	}
	buf := make([]byte, 0, r.ContentLength+1+requestBodyHeadroom)
	for {
		if len(buf) == cap(buf) {
			// Grow, keeping the length: the extra capacity is the room the
			// next Read fills.
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if err == io.EOF {
				return buf, nil
			}
			return nil, err
		}
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

// estimateTokensFromBody approximates the request context size in tokens from
// the raw request bytes. It measures the "messages" value when the body has one
// and falls back to the whole body otherwise, then applies the same conservative
// two-characters-per-token ratio as estimateTokens. Only bytes are measured, so
// no message content is copied; the JSON structural overhead inside the span is
// counted too, which can only raise the estimate -- and with it the timeout --
// never shrink it below the text it encloses.
func estimateTokensFromBody(body []byte) int {
	chars := len(body)
	if obj, ok := scanJSONObject(body); ok {
		if span, found := findJSONMember(obj, "messages"); found {
			chars = span.End - span.Start
		}
	}
	tokens := chars / 2
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
	obj, ok := scanJSONObject(body)
	if !ok {
		return body
	}
	if _, found := findJSONMember(obj, "max_tokens"); found {
		return body
	}
	val, err := json.Marshal(maxTokens)
	if err != nil {
		return body
	}
	return insertJSONMemberReusing(body, obj.End, "max_tokens", val)
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
	obj, ok := scanJSONObject(body)
	if !ok {
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

	// The new message is spliced into the messages array in place: the
	// conversation is forwarded verbatim, and re-encoding it would copy
	// every message again for every failover.
	span, found := findJSONMember(obj, "messages")
	if !found {
		arr := make([]byte, 0, len(msgJSON)+2)
		arr = append(arr, '[')
		arr = append(arr, msgJSON...)
		arr = append(arr, ']')
		return insertJSONMember(body, obj.End, "messages", arr)
	}
	existing := body[span.Start:span.End]
	if len(existing) == 0 {
		return body
	}
	if existing[0] == 'n' {
		// A null messages array carried no messages, so it becomes the
		// one-message array.
		arr := make([]byte, 0, len(msgJSON)+2)
		arr = append(arr, '[')
		arr = append(arr, msgJSON...)
		arr = append(arr, ']')
		return replaceJSONValue(body, span, arr)
	}
	if existing[0] != '[' {
		// Not an array: the old decoder rejected it too, and the body is
		// forwarded unchanged.
		return body
	}
	closing := span.End - 1
	empty := skipJSONSpace(existing[1:len(existing)-1], 0) == len(existing)-2
	out := make([]byte, 0, len(body)+len(msgJSON)+1)
	out = append(out, body[:closing]...)
	if !empty {
		out = append(out, ',')
	}
	out = append(out, msgJSON...)
	out = append(out, body[closing:]...)
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
// maxRequestAttemptTimeout is the largest connection/first-output budget one
// upstream attempt may receive. It is deliberately five seconds shorter than
// maxSessionInterruption: even when the largest attempt fails at the very end
// of its budget, the fallback walk still has time to try another endpoint
// instead of consuming the client's whole interruption allowance in one call.
//
// A context-size timeout must never be allowed to grow past this ceiling. A
// 500k-token prompt used to produce a 55s attempt timeout, which by itself was
// longer than the maximum interruption the client was promised.
const maxRequestAttemptTimeout = maxSessionInterruption - 5*time.Second

// requestTimeout returns a timeout that scales with the request context size:
// 5s for up to 1000 tokens, plus 1s for each additional 10000 tokens (ceil
// division), capped at maxRequestAttemptTimeout.
//
// The cap is part of the timeout calculation, not a separate transport limit:
// a larger prompt still gets a larger allowance up to the cap, but no single
// attempt may consume the complete 30s interruption budget. The fallback
// walk clamps each later attempt to the budget that is actually left.
func requestTimeout(tokens int) time.Duration {
	const base = 5 * time.Second
	if tokens <= 1000 {
		return base
	}
	extra := (tokens - 1000 + 9999) / 10000 // ceil division, 1s per extra 10k tokens
	timeout := base + time.Duration(extra)*time.Second
	if timeout > maxRequestAttemptTimeout {
		return maxRequestAttemptTimeout
	}
	return timeout
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
const maxFallbackWallClock = maxSessionInterruption

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

// budgetState tracks a request's fallback walk against its wall-clock budget.
//
// Every fallback loop in the gateway uses this rather than repeating
// `attempts > 0 && time.Since(start) > budget`. The duplication is not
// cosmetic: when the budget was first added to the chat path, the two
// Responses loops in responses_api.go were simply missed, and they happily
// held a client open for 2m30s (measured 2026-09-29) while the chat path was
// capped at 12s for a small request. Copy-paste across a fourth and fifth
// loop is exactly how that happened, so the check lives here once and each
// loop constructs a state and asks it a question.
//
// It also owns the attempt counter, so `attempts` and the budget check can
// never disagree about how many attempts have been made.
type budgetState struct {
	start     time.Time
	budget    time.Duration
	attempts  int
	exhausted bool
	// lastProgress starts at the request start and moves forward every time
	// output reaches the client. Before the first output it is the start of
	// the interruption; after the first output it is the start of the current
	// gap. A successful long generation that then fails mid-stream is
	// therefore still allowed a full failover budget for the RESUME, while the
	// time it spent producing output is not counted as interruption.
	lastProgress time.Time
}

// newBudgetState starts a budget for one request's fallback walk. timeout is
// the per-attempt timeout; the budget is scaled from it by fallbackBudget.
func newBudgetState(start time.Time, timeout time.Duration) *budgetState {
	return &budgetState{start: start, budget: fallbackBudget(timeout), lastProgress: start}
}

// next records that another attempt is about to be made.
func (b *budgetState) next() { b.attempts++ }

// markProgress records that output reached the client, which starts a new
// interruption window. The fallback loops call it from the streaming write
// path, so a long generation is not charged against the resume budget.
func (b *budgetState) markProgress() { b.lastProgress = time.Now() }

// exhausted reports whether the walk should stop. The first attempt is always
// allowed, so a request is never failed before it has been tried once.
func (b *budgetState) exhaustedNow() bool {
	if b.attempts > 0 && b.elapsedSinceProgress() > b.budget {
		b.exhausted = true
		return true
	}
	return false
}

// elapsed is the wall-clock time spent so far, for logging.
func (b *budgetState) elapsed() time.Duration { return time.Since(b.start) }

// elapsedSinceProgress is the length of the current interruption: how long the
// client has now been without new output. It is the interval the 30s contract
// bounds.
func (b *budgetState) elapsedSinceProgress() time.Duration {
	return time.Since(b.lastProgress)
}

// remaining is the wall-clock time this request's fallback walk has left in
// the current interruption window before the budget forces a 503. It goes
// negative once the budget is spent, which is exactly what the cycle-back
// waiter compares against: a wait that outlasts the remaining budget cannot
// help, because the loop re-checks the budget the instant the wait ends.
func (b *budgetState) remaining() time.Duration {
	return b.budget - b.elapsedSinceProgress()
}

// attemptTimeout returns the timeout ONE upstream attempt may use. It is the
// smaller of the request's size-derived allowance and the interruption budget
// that is left, so a first long attempt does not let a second long attempt run
// and put a double-length interruption in front of a client.
//
// A non-positive result means there is no budget left; the caller must give up
// now rather than start an attempt whose promised bound is already spent.
func (b *budgetState) attemptTimeout(preferred time.Duration) time.Duration {
	if preferred <= 0 {
		return 0
	}
	remaining := b.remaining()
	if remaining <= 0 {
		return 0
	}
	if preferred > remaining {
		return remaining
	}
	return preferred
}

// cycleBackSleep returns how long the cycle-back waiter may sleep, and false
// when the request must give up NOW instead of waiting.
//
// The wait is capped at the provider's own interruption ceiling (30s by
// default), so a long cooldown cannot hold a request open indefinitely. The
// cap is not the whole story: a wait that outlasts the budget the request has
// left is pure latency, because the loop re-checks the budget the instant the
// wait ends and returns the 503 the budget was going to force anyway.
//
// Measured on 2026-10-20 against a 30s transport cooldown and a 12s
// small-request budget: the request was held open for 30.2s and then returned
// exactly the 503 it would have returned immediately. The client saw a hung
// request for half a minute and got the same answer.
func cycleBackSleep(wait time.Duration, walk *budgetState, ceiling time.Duration) (time.Duration, bool) {
	if ceiling <= 0 {
		ceiling = maxSessionInterruption
	}
	if ceiling > maxSessionInterruption {
		ceiling = maxSessionInterruption
	}
	sleep := minDuration(wait, ceiling)
	if remaining := walk.remaining(); sleep >= remaining {
		return 0, false
	}
	return sleep, true
}

func (g *GatewayContext) handleCompletion(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string, tokens int) {
	start := time.Now()
	ctx := r.Context()
	timeout := requestTimeout(tokens)
	// Wall-clock budget for this request's fallback walk. Shared by all four
	// fallback loops so none can ship without the bound.
	walk := newBudgetState(start, timeout)
	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}

	// tried tracks endpoints that have failed in THIS request's fallback loop.
	// It is local to this goroutine so concurrent requests with the same
	// session ID don't interfere with each other's endpoint selection.
	tried := map[string]bool{}
	// protocolFlips bounds protocol switching to ONE flip per endpoint per
	// request. Without it, an endpoint that refuses both shapes would bounce
	// between them forever inside the fallback budget.
	protocolFlips := map[string]bool{}

	// The session is inflight for the lifetime of the request:
	// the dashboard counts the sessions each model is serving
	// right now, and a request walking its fallback chain is
	// still being served by its model. The deferred untrack
	// covers every exit path below — success, upstream error,
	// client disconnect, budget exhaustion — so a session can
	// never linger in the inflight set after its request is
	// done.
	g.inflight.Track(sessionID, req.Model, "")
	defer g.inflight.Untrack(sessionID)

	for {
		if ctx.Err() != nil {
			writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		// Wall-clock guard: stop grinding through the chain once this request
		// has spent its whole fallback budget, so a client never waits
		// minutes on a chain of failing providers.
		if walk.exhaustedNow() {
			log.Printf("[debug] session=%s model=%s -> fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, walk.attempts, walk.elapsed().Round(time.Millisecond), walk.budget)
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		if walk.attempts >= maxAttempts {
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
		walk.next()

		ep, wait := g.router.SelectEndpointForTokens(req.Model, sessionID, requestHasVision(body), tried, tokens)
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
			// Every endpoint in the chain is on cooldown (or otherwise
			// ineligible). Wait out the SHORTEST remaining cooldown and
			// cycle back: SelectEndpoint restarts its walk at the head of
			// the chain, so the last model in the chain leads back to the
			// first, and a model that recovers mid-request gives this
			// request another chance instead of a guaranteed 503. The
			// wait is capped at the provider's own interruption
			// ceiling, and the loop stays bounded by the wall-clock
			// budget and the attempts counter above, so this cannot
			// spin forever. A zero wait means no cooldown is pending
			// anywhere in the chain; there is nothing to wait for, so
			// fail fast.
			if wait > 0 {
				sleep, ok := cycleBackSleep(wait, walk, g.router.MaxInterruptionFor(req.Model))
				if !ok {
					log.Printf("[debug] session=%s model=%s -> cycle-back wait %v outlasts the remaining fallback budget %v; returning 503 now", sessionID, req.Model, wait.Round(time.Millisecond), walk.remaining().Round(time.Millisecond))
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

		// Which upstream endpoint is generating for this
		// session right now. A mid-request fallback moves the
		// session to the next endpoint in the chain, and the
		// dashboard should show where it is being served now,
		// not where it started.
		g.inflight.UpdateEndpoint(sessionID, ep.Key())

		// The provider's own interruption ceiling bounds how much silence
		// this endpoint may put in front of the client. It is the smaller of
		// the ceiling and what the fallback walk has left, so a provider
		// configured for a short window (a local endpoint that answers in
		// milliseconds) does not hold a client open for 30s on its cooldown.
		attemptTimeout := walk.attemptTimeout(minDuration(timeout, g.providerMaxInterruption(ep.Provider)))
		if attemptTimeout <= 0 {
			log.Printf("[debug] session=%s model=%s -> fallback budget exhausted before attempt %d in %v (budget %v)", sessionID, req.Model, walk.attempts+1, walk.elapsed().Round(time.Millisecond), walk.budget)
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		start := time.Now()
		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, attemptTimeout, tokens)
		// attemptStart times this single upstream call, distinct from the
		// request-level `start` above. Shadowing it would silently turn every
		// time.Since(start) below into a per-attempt measurement, which is how
		// the request-level latency metric would stop meaning request latency.
		attemptStart := time.Now()
		// For proxied endpoints, retry with different proxies before giving up
		// on the endpoint. A proxy transport/dial failure is a proxy problem,
		// not an endpoint problem, so the endpoint must not be cooled down;
		// only the proxy is. proxyTries is the maximum number of different
		// proxies to try: the endpoint's own proxy value, or its provider's when
		// the endpoint does not set one (0 from both = direct connection). Even
		// a direct connection gets one attempt.
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
				// Non-proxy error: endpoint failure.
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
				// A ProxyError is excluded first: the reset lands on the leg
				// between the gateway and the PROXY, so it looks exactly like a
				// client hangup to the string match, but the client is still
				// waiting and must be given the next model instead of a 499.
				writeAPIError(w, 499, "Client disconnected", "server_error", "client_disconnected")
				g.recordRequest(req.Model, 499, time.Since(start))
				return
			}
			tried[ep.Key()] = true
			g.recordFallback(ep)
			if !errors.As(lastErr, &pErr) {
				// Only cool down the endpoint for non-proxy errors.
				g.router.ApplyCooldownFromErrorForSession(ep, lastErr, sessionID)
			}
			g.recordCooldown()
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, lastErr.Error(), sessionID, g.testCooldown)
			}
			continue
		}
		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()

			// Command Code error statuses are remapped to the
			// OpenAI-equivalent the client and the cooldown
			// classifier expect: 402 payment-required is a rate
			// limit, 5xx is a retryable upstream error, and an
			// unlisted status becomes a 502 rather than a generic
			// failure.
			if ep.UpstreamProtocol() == protocolCommandCode {
				if mapped := ccMapError(resp.StatusCode, respBody); mapped != nil {
					resp.StatusCode = mapped.StatusCode
					respBody = mapped.Body
				}
			}

			// "This model does not support this protocol" is a statement about
			// the PROTOCOL, not about the endpoint's health: the same catalog
			// holds chat-only and responses-only models side by side. Measured
			// live 2026-10-02 against opencode.ai with one key:
			//
			//	muse-spark-1.3-contributor-free  chat 400 -> /responses 200
			//	big-pickle                       chat 200 -> /responses 400
			//
			// So the refusal means "use the other shape", and the endpoint keeps
			// serving. Cooling it down would park a working model for a day.
			// Only once BOTH shapes have refused is the endpoint genuinely
			// unusable, and that falls through to the long cooldown below.
			if isProtocolUnsupported(resp.StatusCode, string(respBody)) {
				key := ep.Key()
				if !protocolFlips[key] {
					protocolFlips[key] = true
					flip := protocolResponses
					if g.proxy.effectiveProtocol(ep) == protocolResponses {
						flip = protocolChatCompletions
					}
					log.Printf("[debug] session=%s model=%s -> %s/%s refuses this protocol; "+
						"retrying the same endpoint over %s and remembering it",
						sessionID, req.Model, ep.Provider, ep.Model, flip)
					g.router.LearnProtocol(ep, flip)
					continue
				}
			}

			g.recordAttempt(ep, time.Since(attemptStart), false)
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

		// OpenCode free-tier gate: the body gate in buildRequest forced
		// `stream:true` upstream even for this non-streaming client, so
		// an opencode endpoint answers an SSE stream where a single JSON
		// object was expected. Fold the stream back into the Chat
		// Completions shape here, so everything after this point -- TPS
		// accounting, the headers copied to the client, the body written
		// out -- sees the shape it expects.
		//
		// Only the Chat Completions protocol carries the gate: a
		// Responses-protocol endpoint keeps its own envelope, and its
		// translation below handles that shape. A truncation or
		// zero-output failure is treated as a failed attempt rather than
		// forwarded: emitting a half-folded completion would be a silent
		// corruption, while falling back gets a real answer from the next
		// endpoint.
		if isOpencodeProvider(ep.Provider) && g.proxy.effectiveProtocol(ep) == protocolChatCompletions {
			if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
				converted, cerr := opencodeAggregateStream(respBody, req.Model, time.Now().Unix())
				if cerr != nil {
					log.Printf("[debug] session=%s model=%s -> opencode SSE aggregation failed: %v", sessionID, req.Model, cerr)
					tried[ep.Key()] = true
					g.router.ApplyCooldownForSession(ep, 502, cerr.Error(), sessionID, 0)
					g.recordCooldown()
					continue
				}
				respBody = converted
			}
		}

		// An endpoint addressed with the Responses API answers with a Responses
		// envelope; the client asked for Chat Completions. Translate here so
		// everything after this point -- TPS accounting, the headers copied to
		// the client, the body written out -- sees the shape it expects.
		//
		// A translation failure is treated as a failed attempt rather than
		// forwarded: emitting an envelope the client cannot parse would be a
		// silent corruption, while falling back gets a real answer from the
		// next endpoint.
		if ep.UpstreamProtocol() == protocolResponses {
			converted, cerr := responsesBodyToChat(respBody, req.Model)
			if cerr != nil {
				log.Printf("[debug] session=%s model=%s -> responses->chat translation failed: %v", sessionID, req.Model, cerr)
				tried[ep.Key()] = true
				g.router.ApplyCooldownForSession(ep, 502, cerr.Error(), sessionID, 0)
				g.recordCooldown()
				continue
			}
			respBody = converted
		}

		// Command Code answers with an NDJSON event stream even for
		// a non-streaming client request (the envelope always sets
		// stream:true). Fold the events into a Chat Completions
		// object here. A truncated stream (no finish event) or an
		// empty one (zero output tokens) is a failed attempt: falling
		// back gets a real answer from the next endpoint.
		if ep.UpstreamProtocol() == protocolCommandCode {
			converted, cerr := ccAggregateStream(respBody, req.Model, time.Now().Unix())
			if cerr != nil {
				log.Printf("[debug] session=%s model=%s -> commandcode aggregation failed: %v", sessionID, req.Model, cerr)
				tried[ep.Key()] = true
				g.router.ApplyCooldownFromErrorForSession(ep, cerr, sessionID)
				g.recordCooldown()
				continue
			}
			respBody = converted
		}

		// Model patches rewrite provider-specific output
		// artifacts (e.g. tool calls a model traps inside its
		// thinking block) after the protocol translations
		// above, so every upstream shape is normalized before
		// the patch runs.
		if patch := modelPatchFor(ep.Model); patch != nil {
			respBody = patch.TransformCompletion(respBody)
		}

		g.router.RecordSuccess(ep)
		attemptDur := time.Since(attemptStart)
		g.recordAttempt(ep, attemptDur, true)
		if attemptDur > 0 {
			if copts := extractCompletionTokens(respBody); copts > 0 {
				g.router.RecordTPS(*ep, float64(copts)/attemptDur.Seconds())
				if g.metrics != nil {
					g.metrics.RecordDashboardTokens(ep.Key(), int64(copts))
				}
			}
		}

		for k, v := range resp.Header {
			if isHopByHopHeader(k) {
				continue
			}
			w.Header()[k] = v
		}
		// Command Code's NDJSON stream is folded into a plain JSON
		// object above; the upstream's stream content type must not
		// reach the client.
		if ep.UpstreamProtocol() == protocolCommandCode {
			w.Header().Set("Content-Type", "application/json")
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

func (g *GatewayContext) handleStream(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string, tokens int) {
	start := time.Now()
	ctx := r.Context()
	timeout := requestTimeout(tokens)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "Streaming not supported", "server_error", "streaming_unsupported")
		g.recordRequest(req.Model, 500, time.Since(start))
		return
	}

	// Wall-clock budget for this request's fallback walk, shared policy.
	walk := newBudgetState(start, timeout)
	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}

	// tried tracks endpoints that have failed in THIS request's fallback loop.
	// It is local to this goroutine so concurrent requests with the same
	// session ID (e.g. duplicate client retries) don't interfere with each
	// other's endpoint selection through a shared tried set.
	tried := map[string]bool{}
	// protocolFlips bounds protocol switching to ONE flip per endpoint per
	// request. Without it, an endpoint that refuses both shapes would bounce
	// between them forever inside the fallback budget.
	protocolFlips := map[string]bool{}

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

	// The session is inflight for the lifetime of the request;
	// see the matching comment in handleCompletion. The deferred
	// untrack covers every exit path, including the mid-stream
	// failure path that replays partial output and falls back.
	g.inflight.Track(sessionID, req.Model, "")
	defer g.inflight.Untrack(sessionID)

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
		if walk.exhaustedNow() {
			log.Printf("[debug] session=%s model=%s -> stream fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, walk.attempts, walk.elapsed().Round(time.Millisecond), walk.budget)
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
		if walk.attempts >= maxAttempts {
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
		walk.next()

		ep, wait := g.router.SelectEndpointForTokens(req.Model, sessionID, requestHasVision(body), tried, tokens)
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
			// Every endpoint in the chain is on cooldown (or otherwise
			// ineligible). Wait out the SHORTEST remaining cooldown and
			// cycle back: SelectEndpoint restarts its walk at the head of
			// the chain, so the last model in the chain leads back to the
			// first, and a model that recovers mid-request gives this
			// request another chance instead of a terminated stream. The
			// reconnecting comment keeps the SSE connection alive during
			// the wait. The wait is capped at the provider's own interruption
			// ceiling, and the loop stays bounded by the wall-clock budget and
			// the attempts counter above, so this cannot spin forever. A zero
			// wait means no cooldown is pending anywhere in the chain; there is
			// nothing to wait for, so fail fast.
			if wait > 0 {
				sleep, ok := cycleBackSleep(wait, walk, g.router.MaxInterruptionFor(req.Model))
				if !ok {
					log.Printf("[debug] session=%s model=%s -> cycle-back wait %v outlasts the remaining fallback budget %v; returning 503 now", sessionID, req.Model, wait.Round(time.Millisecond), walk.remaining().Round(time.Millisecond))
					writeSSEError(w, flusher, "All models are currently unavailable")
					g.recordRequest(req.Model, 503, time.Since(start))
					return
				}
				fmt.Fprintf(w, ": reconnecting in %v\n\n", sleep.Round(time.Second))
				flusher.Flush()
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

		// See the matching comment in handleCompletion: refresh
		// which endpoint is generating for this session before
		// the upstream call that may fall back mid-stream.
		g.inflight.UpdateEndpoint(sessionID, ep.Key())

		// StreamToClient buffers the start of the stream and only flushes to the
		// client once the first content token (or tool call) arrives, so an early
		// upstream error is swallowed and we fall back with no output sent to the
		// client. The size-based timeout guards only time-to-first-token; the
		// parent context keeps the connection alive for the rest of a long
		// generation.
		// attemptStart times this single upstream call. It must not shadow the
		// request-level `start`: every recordRequest below measures the whole
		// request, and a shadowed timer would make the request-latency metric
		// report only the final attempt.
		attemptStart := time.Now()
		// The provider's own interruption ceiling bounds how much silence this
		// endpoint may put in front of the client. It is the smaller of the
		// ceiling and what the fallback walk has left, so a provider configured
		// for a short window (a local endpoint that answers in milliseconds)
		// does not hold a client open for 30s on its cooldown.
		ceiling := g.providerMaxInterruption(ep.Provider)
		attemptTimeout := walk.attemptTimeout(minDuration(timeout, ceiling))
		if attemptTimeout <= 0 {
			log.Printf("[debug] session=%s model=%s -> stream fallback budget exhausted before attempt %d in %v (budget %v)", sessionID, req.Model, walk.attempts+1, walk.elapsed().Round(time.Millisecond), walk.budget)
			if accumulatedContent == "" {
				writeSSEError(w, flusher, "All models are currently unavailable")
			}
			g.recordRequest(req.Model, 503, time.Since(start))
			return
		}
		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, attemptTimeout, tokens)
		// For proxied endpoints, retry with different proxies before giving up
		// on the endpoint. A proxy transport/dial failure is a proxy problem,
		// not an endpoint problem, so the endpoint must not be cooled down;
		// only the proxy is. Non-proxy errors (direct connection or endpoint
		// HTTP errors) cool down the endpoint as before. A proxy failure at
		// client.Do happens before any content is sent, so it is safe to retry
		// on the same endpoint. proxyTries is the maximum number of different
		// proxies to try: the endpoint's own proxy value, or its provider's when
		// the endpoint does not set one (0 from both = direct connection). Even
		// a direct connection gets one attempt.
		var partial string
		var toolCalls []streamToolCall
		var completionTokens int
		var err error
		maxProxyTries := g.proxy.proxyTries(ep)
		if maxProxyTries <= 0 {
			maxProxyTries = 1
		}
		for proxyTry := 0; proxyTry < maxProxyTries; proxyTry++ {
			// Each proxy retry gets only what is left of the interruption
			// budget. Recomputing here matters: without it, three long proxy
			// attempts on one endpoint could hold the client silent for
			// well over the interruption ceiling.
			proxyTimeout := walk.attemptTimeout(minDuration(timeout, ceiling))
			if proxyTimeout <= 0 {
				err = context.DeadlineExceeded
				break
			}
			partial, toolCalls, completionTokens, err = g.proxy.StreamToClient(ctx, w, flusher, body, *ep, proxyTimeout, sessionID, StreamAttemptOptions{
				MaxIdleTimeout: minDuration(ceiling, walk.budget),
				OnProgress:     walk.markProgress,
			})
			if err != nil {
				var pErr *ProxyError
				if errors.As(err, &pErr) {
					// Proxy failure: mark the proxy down and try again with a
					// different proxy on the same endpoint.
					g.proxy.MarkProxyDown(pErr.Proxy.URL, proxyCooldown)
					if pErr.MidStream {
						// Content already reached the client, so re-running this
						// endpoint would duplicate it. Fall through to the
						// next-model resume path below instead of retrying.
						break
					}
					continue
				}
				// Non-proxy error: endpoint failure.
				break
			}
			break
		}
		if err == nil {
			g.router.RecordSuccess(ep)
			attemptDur := time.Since(attemptStart)
			g.recordAttempt(ep, attemptDur, true)
			if attemptDur > 0 && completionTokens > 0 {
				g.router.RecordTPS(*ep, float64(completionTokens)/attemptDur.Seconds())
				if g.metrics != nil {
					g.metrics.RecordDashboardTokens(ep.Key(), int64(completionTokens))
				}
			}
			// StreamToClient emits [DONE] on success; mark as sent so the
			// deferred sendDone() doesn't emit a duplicate.
			doneSent = true
			g.recordRequest(req.Model, 200, time.Since(start))
			return
		}

		g.recordAttempt(ep, time.Since(attemptStart), false)
		var midErr *ProxyError
		if !errors.As(err, &midErr) && isClientDisconnect(err) {
			// Client went away; nothing to resume and no point cooling a model.
			// A ProxyError is excluded first: the reset lands on the leg between
			// the gateway and the PROXY, so it looks exactly like a client
			// hangup to the string match, but the client is still waiting and
			// must be given the resumed stream instead of a 499.
			g.recordRequest(req.Model, 499, time.Since(start))
			return
		}

		// Same protocol-refusal handling as the non-streaming path: the model
		// refuses this SHAPE, not this endpoint. See the comment in
		// handleCompletion; the measurement is identical.
		var protoErr *ProviderError
		if errors.As(err, &protoErr) &&
			isProtocolUnsupported(protoErr.StatusCode, string(protoErr.Body)) &&
			!protocolFlips[ep.Key()] {
			protocolFlips[ep.Key()] = true
			flip := protocolResponses
			if g.proxy.effectiveProtocol(ep) == protocolResponses {
				flip = protocolChatCompletions
			}
			log.Printf("[debug] session=%s model=%s -> %s/%s refuses this protocol; "+
				"retrying the same endpoint over %s and remembering it",
				sessionID, req.Model, ep.Provider, ep.Model, flip)
			g.router.LearnProtocol(ep, flip)
			continue
		}

		// Mark this endpoint as tried in THIS request's local fallback set so
		// we don't retry it within the same loop. Unlike the old shared
		// triedKeys approach, this doesn't leak state to other concurrent
		// requests that share the same session ID.
		tried[ep.Key()] = true
		g.recordFallback(ep)
		var pErr *ProxyError
		if !errors.As(err, &pErr) {
			// Only cool down the endpoint for non-proxy errors. A proxy failure
			// already took its penalty on the proxy above, and charging the
			// endpoint too would drop a healthy model from rotation.
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
			} else {
				g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
			}
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
	//              DELETE /admin/cooldowns?model=a&model=b  (clears multiple)
	if r.Method == http.MethodDelete {
		modelKeys := r.URL.Query()["model"]
		if len(modelKeys) == 0 {
			removed := g.router.ClearCooldowns("")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"cleared": removed,
				"models":  []string{},
			})
			return
		}
		removed := 0
		var modelsRemoved []string
		for _, k := range modelKeys {
			n := g.router.ClearCooldowns(k)
			if n > 0 {
				removed += n
				modelsRemoved = append(modelsRemoved, k)
			} else if k != "" {
				writeAPIError(w, 404, "No cooldown for model "+k, "not_found", "cooldown_not_found")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"cleared": removed,
			"models":  modelsRemoved,
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
		ModelKey   string    `json:"model_key"`
		State      string    `json:"state"`
		OpenedAt   time.Time `json:"opened_at"`
		ProbesSent int       `json:"probes_sent"`
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
		"cooldowns": result,
		"circuits":  circuitsResult,
		"count":     len(result),
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
	// The circuit-breaker knobs are Router-level state too, applied
	// at construction in NewRouter. Push them across on every reload
	// like the jitter above, or a config change to them silently
	// no-ops until restart.
	g.router.SetCircuitBreakerThreshold(cfg.Preferences.CircuitBreakerThresholdValue())
	g.router.SetCircuitHalfOpenProbes(cfg.Preferences.CircuitHalfOpenProbesValue())
	// Same story for the metrics label cap: the collector is built once and
	// outlives every reload, so a config change to max_label_cardinality has
	// to be pushed into it or it silently no-ops until restart.
	if g.metrics != nil {
		g.metrics.SetLabelCap(cfg.Preferences.MaxLabelCardinalityValue())
	}
	// The ready-pool requirement is derived from the provider proxy budgets, so
	// it changes with the config. Without this, enabling a proxy on another
	// provider would leave the requirement at its startup value and the pool
	// would be under-provisioned for the new chain.
	if g.proxy != nil {
		g.proxy.UpdateRequiredReady()
	}
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
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'")
	w.Write([]byte(dashboardHTML))
}

// HandleAdminDashboardData serves the JSON payload the dashboard page fetches.
// It bundles chain status and the three leaderboards (speed, volume, failures)
// so a single request populates the page, and it rejects unknown windows
// rather than silently defaulting.
func (g *GatewayContext) HandleAdminDashboardData(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	if g.metrics == nil {
		writeAPIError(w, 503, "Metrics not available", "server_error", "not_initialized")
		return
	}

	raw := r.URL.Query().Get("window")
	win, ok := dashboardWindowFor(raw)
	if !ok {
		writeAPIError(w, 400, "Unknown window", "invalid_request_error", "unknown_window")
		return
	}

	cfg := g.router.Config()

	// Eligibility is computed by the router, which owns cooldowns and
	// circuits. The dashboard ring knows nothing about either, so it must
	// not be asked. ProbeAvailability rather than IsAvailable: the latter
	// mutates circuit state (Open→HalfOpen, probe counters) and the dashboard
	// scrapes every 30s, so asking it here would race the request path over
	// breakers that are mid-recovery.
	eligible := func(ep ModelEndpoint) (bool, bool, string, time.Time) {
		return g.router.ProbeAvailability(&ep)
	}

	chains := DashboardChainStatus(cfg, eligible)
	aggs := g.metrics.DashboardQuery(win)

	// Derive the three leaderboards from the same aggregate set so they
	// cannot disagree about what happened in the window.
	speed := make([]dashboardLeaderboardEntry, 0, len(aggs))
	volume := make([]dashboardLeaderboardEntry, 0, len(aggs))
	failures := make([]dashboardLeaderboardEntry, 0, len(aggs))

	// The window duration, in seconds, for the volume leaderboard's rate
	// denominator. A zero or negative window means "no data": the query
	// already returned nil.
	secs := float64(dashboardWindowSeconds(win))

	for _, a := range aggs {
		entry := dashboardLeaderboardEntry{
			Endpoint:    a.Endpoint,
			Attempts:    a.Attempts,
			Failures:    a.Failures,
			LatencyNS:   a.LatencyNS,
			Tokens:      a.Tokens,
			Percentiles: a.Percentiles,
		}
		// Speed is tokens per SECOND OF BUSY TIME, not per second of window.
		// An endpoint that served 500 tokens in one minute of a 7d window and
		// was idle the rest of it is an 8.3 tok/s endpoint, not a
		// 0.000001 tok/s one; dividing by the window would rank every quiet
		// endpoint near zero and bury the genuinely fast ones, which is the
		// opposite of what a "fastest" leaderboard should do. The ring counts
		// the ticks in which the endpoint was reached (activeTicks), so the
		// denominator is the time it was actually busy.
		//
		// An endpoint that was reached but produced no tokens (an error
		// mid-stream, or a non-chat model) gets a rate of zero rather than a
		// blank — the entry still appears, which is the honest answer.
		if a.ActiveTicks > 0 {
			entry.Rate = float64(a.Tokens) / (float64(a.ActiveTicks) * dashboardTickSeconds)
		}
		speed = append(speed, entry)

		entry.Rate = 0
		if secs > 0 {
			entry.Rate = float64(a.Attempts) / secs
		}
		volume = append(volume, entry)

		entry.Rate = 0
		if a.Attempts > 0 {
			entry.Rate = float64(a.Failures) / float64(a.Attempts)
		}
		failures = append(failures, entry)
	}

	sort.Slice(speed, func(i, j int) bool { return speed[i].Rate > speed[j].Rate })
	sort.Slice(volume, func(i, j int) bool { return volume[i].Rate > volume[j].Rate })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Rate > failures[j].Rate })

	resp := dashboardResponse{
		Chains:         chains,
		Speed:          speed,
		Volume:         volume,
		Failures:       failures,
		Inflight:       g.inflight.Snapshot(),
		Window:         string(win),
		Now:            time.Now(),
		Generated:      time.Now(),
		CircuitBreaker: g.router.CircuitBreakerEnabled(),
		Cooldowns:      g.router.GetAllCooldowns(),
		Circuits:       g.router.GetAllCircuits(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
