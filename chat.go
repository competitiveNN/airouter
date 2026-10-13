package main

// chat.go — a web chat UI served by the gateway itself, a substitute
// for open-webui.
//
// The UI is a single self-contained HTML page (chatHTML) that talks to
// two proxy endpoints on this gateway:
//
//   - POST /chat/completions  -> <endpoint.url>/chat/completions
//   - POST /chat/responses    -> <endpoint.url>/responses
//   - POST /chat/models       -> GET <endpoint.url>/models
//
// The request body carries an "endpoint" object {url, api_key} naming
// the remote OpenAI-compatible API to talk to; the handler strips it
// and forwards every other field verbatim, so the full request
// vocabulary of both APIs works without re-modelling it here. The
// default endpoint is the gateway itself, exposing smart/work/fast/large.
// The model roster of an endpoint is fetched from its /models path on
// demand, so it is never configured by hand.
//
// Per-response stats (context tokens, time-to-first-token, prefill
// t/s, decode t/s) are measured here where the upstream clock is
// authoritative:
//   - streaming: the SSE stream is scanned line-by-line and passed
//     through live; after the stream ends the proxy appends one
//     `airouter.stats` SSE event carrying the measurements.
//   - non-streaming: the same numbers travel as X-Chat-* response
//     headers (headers cannot be sent after the body starts, so the
//     streaming path cannot use them).
//
// Chats, endpoints and UI state are persisted in the browser's
// localStorage, keeping the gateway stateless; the "delete all chats"
// button clears the browser-side store.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// chatMaxRequestBytes mirrors the main API's body cap.
const chatMaxRequestBytes = 32 << 20 // 32 MiB

// Response headers carrying per-request stats on non-streaming replies.
const (
	chatHeaderContextTokens = "X-Chat-Context-Tokens"
	chatHeaderTTFTMs        = "X-Chat-TTFT-Ms"
	chatHeaderElapsedMs     = "X-Chat-Elapsed-Ms"
	chatHeaderPrefillTPS    = "X-Chat-Prefill-Tokens-Per-Sec"
	chatHeaderDecodeTPS     = "X-Chat-Decode-Tokens-Per-Sec"
)

// chatStatsEvent is the SSE event the proxy appends after the upstream
// stream ends. The namespaced name cannot collide with any OpenAI or
// Responses API event, and clients that don't know it simply ignore it.
const chatStatsEvent = "airouter.stats"

// Upstream paths appended to the endpoint's base URL.
const (
	chatUpstreamCompletions = "/chat/completions"
	chatUpstreamResponses   = "/responses"
	chatUpstreamModels      = "/models"
)

const (
	chatProtocolCompletions = "completions"
	chatProtocolResponses   = "responses"
)

// chatEndpointRef is the per-request pointer to the remote API the
// proxy should talk to. The UI keeps the roster of endpoints in
// localStorage and sends the selected one with every request, so the
// gateway stays stateless and endpoint API keys never have to be
// configured server-side.
type chatEndpointRef struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

// chatHTTPClient talks to arbitrary remote endpoints. Streams can be
// long, so there is no overall client timeout; the dial and
// response-header timeouts bound the wait for a stalled upstream.
// The server's own WriteTimeout (300s) caps total stream lifetime.
var chatHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ResponseHeaderTimeout: 300 * time.Second,
	},
}

// chatStats is the measurement payload, both as the airouter.stats
// SSE event body and (field-by-field) as response headers.
type chatStats struct {
	ContextTokens int     `json:"context_tokens"`
	TTFTMs        int64   `json:"ttft_ms"`
	ElapsedMs     int64   `json:"elapsed_ms"`
	PrefillTPS    float64 `json:"prefill_tokens_per_sec,omitempty"`
	DecodeTPS     float64 `json:"decode_tokens_per_sec,omitempty"`
}

// chatStreamEvent parses one SSE data payload from either streaming
// grammar the proxy supports:
//   - Chat Completions: {"choices":[{"delta":{"content":"..."}}],
//     "usage":{...}} (usage only on the final chunk). Reasoning
//     models add delta.reasoning_content (or .reasoning / .reasoning_text).
//   - Responses: {"type":"response.output_text.delta","delta":"..."},
//     {"type":"response.reasoning_summary_text.delta","delta":"..."},
//     {"type":"response.reasoning_summary_part.delta","part":{"delta":"..."}}
//     and {"type":"response.completed","response":{"usage":{...}}}
type chatStreamEvent struct {
	Type     string             `json:"type"`
	Delta    string             `json:"delta"`
	Part     *chatStreamPart    `json:"part"`
	Choices  []chatStreamChoice `json:"choices"`
	Usage    *chatUsage         `json:"usage"`
	Response *chatResponseRef   `json:"response"`
}

// chatStreamPart carries the delta of a Responses API
// reasoning summary part, whose text lives one level down.
type chatStreamPart struct {
	Delta string `json:"delta"`
}

type chatStreamChoice struct {
	Delta   chatDeltaContent `json:"delta"`
	Message chatDeltaContent `json:"message"`
}

type chatDeltaContent struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ReasoningText    string `json:"reasoning_text"`
}

type chatResponseRef struct {
	Usage *chatUsage `json:"usage"`
}

// chatUsage carries both grammars' token counts; the accessors prefer
// the Chat Completions spelling and fall back to the Responses one.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

func (u *chatUsage) promptTokens() int {
	if u == nil {
		return 0
	}
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	return u.InputTokens
}

func (u *chatUsage) completionTokens() int {
	if u == nil {
		return 0
	}
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	return u.OutputTokens
}

// HandleChatPage serves the chat UI. Authenticated like the admin
// surface: the header is primary, ?api_key= is the browser form (a URL
// typed into the address bar cannot carry a header).
func (g *GatewayContext) HandleChatPage(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'")
	w.Write([]byte(chatHTML))
}

// HandleWebChatCompletions proxies a Chat Completions request to the
// remote endpoint named in the request body.
func (g *GatewayContext) HandleWebChatCompletions(w http.ResponseWriter, r *http.Request) {
	g.handleChatProxy(w, r, chatUpstreamCompletions, chatProtocolCompletions)
}

// HandleWebChatResponses proxies a Responses API request to the
// remote endpoint named in the request body.
func (g *GatewayContext) HandleWebChatResponses(w http.ResponseWriter, r *http.Request) {
	g.handleChatProxy(w, r, chatUpstreamResponses, chatProtocolResponses)
}

// HandleWebChatModels fetches the model roster of the remote
// endpoint named in the request body by proxying GET to
// <endpoint.url>/models. The UI uses it to populate its model
// dropdown from what the endpoint actually serves, so the roster
// is never configured by hand. The response is passed through
// verbatim: the OpenAI shape {"object":"list","data":[{"id":...}]}
// is the caller's concern, not the proxy's.
func (g *GatewayContext) HandleWebChatModels(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, chatMaxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, 413, "Request body too large", "invalid_request_error", "request_too_large")
		return
	}
	_, ep, _, err := chatSplitEndpoint(body)
	if err != nil {
		writeAPIError(w, 400, err.Error(), "invalid_request_error", "bad_json")
		return
	}
	if ep == nil || strings.TrimSpace(ep.URL) == "" {
		writeAPIError(w, 400, "Missing chat endpoint url", "invalid_request_error", "missing_endpoint")
		return
	}

	upstreamURL := strings.TrimSuffix(strings.TrimSpace(ep.URL), "/") + chatUpstreamModels
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeAPIError(w, 400, "Invalid endpoint url: "+err.Error(), "invalid_request_error", "invalid_url")
		return
	}
	if ep.APIKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}

	resp, err := chatHTTPClient.Do(upstream)
	if err != nil {
		writeAPIError(w, 502, "Upstream request failed: "+err.Error(), "server_error", "upstream_error")
		return
	}
	defer resp.Body.Close()

	// Hop-by-hop headers belong to this connection, not the upstream's.
	for k, vs := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	upstreamBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		writeAPIError(w, 502, "Failed reading upstream response", "server_error", "upstream_error")
		return
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(upstreamBody)
}

// handleChatProxy is the shared proxy. It authenticates the caller,
// splits the endpoint reference out of the body, forwards the rest of
// the request verbatim, and measures the response.
func (g *GatewayContext) handleChatProxy(w http.ResponseWriter, r *http.Request, upstreamPath, protocol string) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, chatMaxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, 413, "Request body too large", "invalid_request_error", "request_too_large")
		return
	}
	fields, ep, stream, err := chatSplitEndpoint(body)
	if err != nil {
		writeAPIError(w, 400, err.Error(), "invalid_request_error", "bad_json")
		return
	}
	if ep == nil || strings.TrimSpace(ep.URL) == "" {
		writeAPIError(w, 400, "Missing chat endpoint url", "invalid_request_error", "missing_endpoint")
		return
	}

	// Ask the upstream to put token usage in the final stream chunk so
	// the UI can show real counts. Only when the caller streams and did
	// not set stream_options themselves.
	if protocol == chatProtocolCompletions && stream {
		if _, ok := fields["stream_options"]; !ok {
			fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	}

	forwarded, err := json.Marshal(fields)
	if err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		return
	}

	upstreamURL := strings.TrimSuffix(strings.TrimSpace(ep.URL), "/") + upstreamPath
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(forwarded))
	if err != nil {
		writeAPIError(w, 400, "Invalid endpoint url: "+err.Error(), "invalid_request_error", "invalid_url")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	if ep.APIKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}

	resp, err := chatHTTPClient.Do(upstream)
	if err != nil {
		writeAPIError(w, 502, "Upstream request failed: "+err.Error(), "server_error", "upstream_error")
		return
	}
	defer resp.Body.Close()

	// Hop-by-hop headers belong to this connection, not the upstream's.
	for k, vs := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	t0 := time.Now()
	// The Content-Type value is case-insensitive (RFC 9110);
	// a provider that spells it "Text/Event-Stream" must still
	// take the streaming path rather than be read as a whole
	// JSON body.
	isStream := stream && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	// Stats describe a successful completion; an error response
	// (including an error event inside an SSE stream) carries none.
	okStatus := resp.StatusCode >= 200 && resp.StatusCode < 300

	if isStream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeAPIError(w, 500, "Streaming not supported", "server_error", "streaming_unsupported")
			return
		}
		w.WriteHeader(resp.StatusCode)
		res := chatPumpStream(w, flusher, resp.Body)
		if !okStatus {
			if res.scanErr != nil && !isClientDisconnect(res.scanErr) {
				log.Printf("[debug] chat proxy stream scan error (%s %s): %v", protocol, upstreamURL, res.scanErr)
			}
			return
		}
		elapsed := time.Since(t0)
		ttft := res.firstContent.Sub(t0)
		if res.firstContent.IsZero() {
			ttft = elapsed
		}
		contextTokens := res.usage.promptTokens()
		if contextTokens <= 0 {
			contextTokens = estimateChatTokens(forwarded)
		}
		stats := chatComputeStats(contextTokens, res.usage.completionTokens(), ttft, elapsed)
		blob, _ := json.Marshal(stats)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", chatStatsEvent, blob)
		flusher.Flush()
		if res.scanErr != nil && !isClientDisconnect(res.scanErr) {
			log.Printf("[debug] chat proxy stream scan error (%s %s): %v", protocol, upstreamURL, res.scanErr)
		}
		return
	}

	// Non-streaming: the whole upstream body is read before anything
	// is written, so the stats can travel as headers.
	upstreamBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		writeAPIError(w, 502, "Failed reading upstream response", "server_error", "upstream_error")
		return
	}
	if !okStatus {
		w.WriteHeader(resp.StatusCode)
		w.Write(upstreamBody)
		return
	}
	elapsed := time.Since(t0)
	usage := chatUsageFromBody(upstreamBody)
	contextTokens := usage.promptTokens()
	if contextTokens <= 0 {
		contextTokens = estimateChatTokens(forwarded)
	}
	stats := chatComputeStats(contextTokens, usage.completionTokens(), elapsed, elapsed)
	w.Header().Set(chatHeaderContextTokens, strconv.Itoa(stats.ContextTokens))
	w.Header().Set(chatHeaderTTFTMs, strconv.FormatInt(stats.TTFTMs, 10))
	w.Header().Set(chatHeaderElapsedMs, strconv.FormatInt(stats.ElapsedMs, 10))
	if stats.PrefillTPS > 0 {
		w.Header().Set(chatHeaderPrefillTPS, strconv.FormatFloat(stats.PrefillTPS, 'f', 1, 64))
	}
	if stats.DecodeTPS > 0 {
		w.Header().Set(chatHeaderDecodeTPS, strconv.FormatFloat(stats.DecodeTPS, 'f', 1, 64))
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(upstreamBody)
}

// chatSplitEndpoint parses the request body, extracts and removes the
// "endpoint" object, and reports whether the request asks to stream.
func chatSplitEndpoint(body []byte) (map[string]json.RawMessage, *chatEndpointRef, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, nil, false, fmt.Errorf("invalid JSON: %v", err)
	}
	var ep *chatEndpointRef
	if raw, ok := fields["endpoint"]; ok {
		ep = &chatEndpointRef{}
		if err := json.Unmarshal(raw, ep); err != nil {
			return nil, nil, false, fmt.Errorf("invalid endpoint object: %v", err)
		}
	}
	delete(fields, "endpoint")
	stream := false
	if raw, ok := fields["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	return fields, ep, stream, nil
}

// chatScanResult collects what the stream scan learned while the bytes
// were flowing past to the client.
type chatScanResult struct {
	firstContent time.Time
	usage        chatUsage
	scanErr      error
}

// chatPumpStream forwards an SSE stream line-by-line (so the UI renders
// tokens live), timing the first content-bearing chunk and capturing
// the final usage payload. Every line is preserved, including blank
// event separators; [DONE] passes through like any other line.
func chatPumpStream(w http.ResponseWriter, flusher http.Flusher, body io.Reader) chatScanResult {
	var res chatScanResult
	scanner := bufio.NewScanner(body)
	// A single SSE line can be huge: the Responses
	// API's response.completed event carries the
	// whole output in one line, and providers
	// embed multi-MB base64 parts in a single
	// delta. The cap matches the non-streaming
	// read cap; a smaller one would truncate such
	// a stream mid-line.
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		payload := sseDataPayload(line)
		if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			if res.firstContent.IsZero() && chatEventText(payload) != "" {
				res.firstContent = time.Now()
			}
			if u := chatEventUsage(payload); u != nil {
				res.usage = *u
			}
		}
		if _, err := w.Write(line); err != nil {
			res.scanErr = err
			return res
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			res.scanErr = err
			return res
		}
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		res.scanErr = err
	}
	return res
}

// chatEventText returns the text carried by a stream event, or "" for
// events with no content (role-only deltas, usage-only chunks, errors).
// Reasoning deltas count as content: they are the first tokens a reasoning
// model emits, so they start the TTFT clock and keep the prefill figure
// honest (it measures the time up to the first output token, and reasoning
// IS output). .done events repeat the full summary text and are excluded
// by the .delta suffix, so reasoning is never counted twice.
func chatEventText(payload []byte) string {
	var ev chatStreamEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return ""
	}
	if strings.HasPrefix(ev.Type, "response.reasoning") && strings.HasSuffix(ev.Type, ".delta") {
		if ev.Delta != "" {
			return ev.Delta
		}
		if ev.Part != nil {
			return ev.Part.Delta
		}
		return ""
	}
	if ev.Type == "response.output_text.delta" {
		return ev.Delta
	}
	if len(ev.Choices) > 0 {
		d := ev.Choices[0].Delta
		if d.ReasoningContent != "" {
			return d.ReasoningContent
		}
		if d.Reasoning != "" {
			return d.Reasoning
		}
		if d.ReasoningText != "" {
			return d.ReasoningText
		}
		return d.Content
	}
	return ""
}

// chatEventUsage returns the usage payload of a stream event, from
// either grammar (top-level for Chat Completions, nested under
// "response" for the Responses API's response.completed event).
func chatEventUsage(payload []byte) *chatUsage {
	var ev chatStreamEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	if ev.Usage != nil {
		return ev.Usage
	}
	if ev.Response != nil {
		return ev.Response.Usage
	}
	return nil
}

// chatUsageFromBody reads usage from a complete (non-streaming)
// response of either API shape.
func chatUsageFromBody(body []byte) *chatUsage {
	var ev chatStreamEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if ev.Usage != nil {
		return ev.Usage
	}
	if ev.Response != nil {
		return ev.Response.Usage
	}
	return nil
}

// chatComputeStats derives the displayed numbers. Prefill speed is the
// context tokens per second up to the first content token; decode
// speed is the completion tokens per second after it. Durations are
// floored at 1ms so a fast first token cannot produce a division by
// zero or a negative decode window.
func chatComputeStats(contextTokens, completionTokens int, ttft, elapsed time.Duration) chatStats {
	stats := chatStats{
		ContextTokens: contextTokens,
		TTFTMs:        ttft.Milliseconds(),
		ElapsedMs:     elapsed.Milliseconds(),
	}
	if contextTokens > 0 {
		d := ttft
		if d < time.Millisecond {
			d = time.Millisecond
		}
		stats.PrefillTPS = float64(contextTokens) / d.Seconds()
	}
	if completionTokens > 0 && elapsed > ttft {
		d := elapsed - ttft
		if d < time.Millisecond {
			d = time.Millisecond
		}
		stats.DecodeTPS = float64(completionTokens) / d.Seconds()
	}
	return stats
}

// estimateChatTokens is the fallback context size used when the
// upstream reports no usage: roughly four characters per token over
// every string in the request (the injected stream_options is
// excluded so the proxy's own addition doesn't inflate the estimate).
func estimateChatTokens(body []byte) int {
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return len(body) / 4
	}
	chars := 0
	var walk func(x interface{})
	walk = func(x interface{}) {
		switch t := x.(type) {
		case string:
			chars += len(t)
		case map[string]interface{}:
			for k, val := range t {
				if k == "stream_options" {
					continue
				}
				walk(val)
			}
		case []interface{}:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	return chars / 4
}

// chatHTML is the whole chat UI: one page, no build step, no
// external assets (the CSP allows only 'self' plus inline
// script/style). JavaScript uses string concatenation rather than
// template literals because this Go raw string literal is delimited
// by backticks.
const chatHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>airouter chat</title>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { margin: 0; font: 14px/1.5 system-ui, -apple-system, sans-serif; background: #0f1115; color: #e6e6e6; }
#app { display: flex; height: 100vh; }
aside { width: 250px; min-width: 250px; background: #16181d; display: flex; flex-direction: column; border-right: 1px solid #262a32; }
.side-header { padding: 10px; display: flex; gap: 8px; }
.side-header button { flex: 1; }
.side-footer { padding: 10px; border-top: 1px solid #262a32; }
button { background: #23272f; color: #e6e6e6; border: 1px solid #333842; border-radius: 6px; padding: 6px 10px; cursor: pointer; font: inherit; }
button:hover { background: #2c313a; }
button.danger { border-color: #5a2b2b; }
button.danger:hover { background: #3a2222; }
#chat-list { flex: 1; overflow-y: auto; }
.chat-item { padding: 8px 10px; cursor: pointer; border-bottom: 1px solid #22252c; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.chat-item:hover { background: #1c1f26; }
.chat-item.active { background: #23272f; }
main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
#topbar { display: flex; gap: 8px; padding: 10px; border-bottom: 1px solid #262a32; align-items: center; flex-wrap: wrap; }
select, input[type=text] { background: #1b1e24; color: #e6e6e6; border: 1px solid #333842; border-radius: 6px; padding: 6px; font: inherit; }
#model-select, #model-input { flex: 1; min-width: 120px; }
#endpoint-select { max-width: 220px; }
#messages { flex: 1; overflow-y: auto; padding: 16px; }
.msg { margin-bottom: 14px; display: flex; }
.msg.user { justify-content: flex-end; }
.msg.assistant { justify-content: flex-start; }
.bubble { padding: 8px 12px; border-radius: 10px; white-space: pre-wrap; word-wrap: break-word; overflow-wrap: anywhere; }
.msg.user .bubble { background: #2d5a8a; border-bottom-right-radius: 2px; }
.msg.assistant .bubble { background: #23272f; border-bottom-left-radius: 2px; }
.bubble code { background: #1a1d23; border-radius: 4px; padding: 1px 4px; font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 0.9em; }
.code-block { background: #14171c; border: 1px solid #2a2e36; border-radius: 8px; padding: 8px 10px; margin: 6px 0; overflow-x: auto; }
.code-block code { display: block; background: none; padding: 0; color: #d6dbe4; white-space: pre; }
.math { font-family: Georgia, 'Times New Roman', serif; }
.math-inline { color: #cfe3ff; }
.math-display { display: block; background: #1a1d23; border-radius: 6px; padding: 6px 10px; margin: 6px 0; text-align: center; overflow-x: auto; }
.bubble h1, .bubble h2, .bubble h3, .bubble h4 { margin: 8px 0 4px; }
.bubble h1 { font-size: 1.25em; }
.bubble h2 { font-size: 1.15em; }
.bubble h3 { font-size: 1.05em; }
.bubble ul, .bubble ol { margin: 4px 0; padding-left: 20px; }
.bubble hr { border: none; border-top: 1px solid #2a2e36; margin: 8px 0; }
.bubble p { margin: 4px 0; }
.msg-col { max-width: 75%; display: flex; flex-direction: column; }
.msg.user .msg-col { align-items: flex-end; }
.stats { font-size: 11px; color: #8b93a1; margin-top: 4px; }
.stream-error { color: #e08080; margin-top: 4px; }
.thinking { margin: 0 0 6px; }
.thinking summary { cursor: pointer; font-size: 12px; color: #8b93a1; user-select: none; }
.thinking-body { margin-top: 4px; padding: 6px 8px; border-left: 2px solid #333842; color: #a8b2c0; font-size: 13px; }
#composer { display: flex; gap: 8px; padding: 10px; border-top: 1px solid #262a32; }
#prompt { flex: 1; resize: none; min-height: 42px; max-height: 160px; background: #1b1e24; color: #e6e6e6; border: 1px solid #333842; border-radius: 6px; padding: 8px; font: inherit; }
#modal-backdrop { position: fixed; inset: 0; background: rgba(0,0,0,.6); display: none; align-items: center; justify-content: center; z-index: 10; }
#modal { background: #16181d; border: 1px solid #333842; border-radius: 8px; padding: 16px; width: 540px; max-width: 92vw; max-height: 82vh; overflow-y: auto; }
#modal h2 { margin: 0 0 12px; font-size: 16px; }
.ep-row { border: 1px solid #2a2e36; border-radius: 6px; padding: 10px; margin-bottom: 10px; }
.ep-row label { display: block; font-size: 11px; color: #8b93a1; margin: 6px 0 2px; }
.ep-row input { width: 100%; background: #1b1e24; color: #e6e6e6; border: 1px solid #333842; border-radius: 6px; padding: 6px; font: inherit; }
.ep-actions { display: flex; gap: 8px; margin-top: 8px; }
.hint { font-size: 12px; color: #8b93a1; margin: 8px 0; }
</style>
</head>
<body>
<div id="app">
  <aside>
    <div class="side-header">
      <button id="new-chat" title="Start a new chat">New chat</button>
      <button id="delete-all" class="danger" title="Delete every saved chat">Delete all</button>
    </div>
    <div id="chat-list"></div>
    <div class="side-footer">
      <button id="settings-btn" style="width:100%">API endpoints</button>
    </div>
  </aside>
  <main>
    <div id="topbar">
      <select id="endpoint-select" title="Remote API endpoint"></select>
      <select id="protocol-select" title="API shape">
        <option value="completions">Completions</option>
        <option value="responses">Responses</option>
      </select>
      <select id="model-select" title="Model"></select>
      <input type="text" id="model-input" placeholder="model name" spellcheck="false" style="display:none">
    </div>
    <div id="messages"></div>
    <div id="composer">
      <textarea id="prompt" placeholder="Message... (Enter to send, Shift+Enter for newline)"></textarea>
      <button id="send">Send</button>
      <button id="stop" class="danger" style="display:none" title="Stop the response">Stop</button>
    </div>
  </main>
</div>
<div id="modal-backdrop">
  <div id="modal">
    <h2>Remote API endpoints</h2>
    <div class="hint">Each endpoint is any OpenAI-compatible API (base URL including /v1). The default endpoint is this gateway, exposing the smart / work / fast / large models. Keys are stored in this browser only.</div>
    <div id="endpoint-list"></div>
    <button id="ep-add">Add endpoint</button>
    <div style="margin-top:12px;text-align:right"><button id="modal-close">Close</button></div>
  </div>
</div>
<script>
'use strict';

var LS_ENDPOINTS = 'airouter.chat.endpoints';
var LS_CHATS = 'airouter.chat.chats';
var LS_UI = 'airouter.chat.ui';

var apiKey = '';
try { apiKey = new URLSearchParams(window.location.search).get('api_key') || ''; } catch (e) {}

var endpoints = [];
var chats = [];
var ui = {};
var currentChatId = null;
var generating = false;
var abortCtrl = null;

function $(id) { return document.getElementById(id); }

function load(key, fallback) {
  try {
    var raw = localStorage.getItem(key);
    if (!raw) return fallback;
    return JSON.parse(raw);
  } catch (e) { return fallback; }
}

function save(key, val) {
  try { localStorage.setItem(key, JSON.stringify(val)); } catch (e) {}
}

function newId(prefix) {
  return prefix + Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
}

function esc(s) {
  return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// renderMarkdown renders a chat message as HTML. The text is
// HTML-escaped first, so the only tags that can appear in the
// output are the ones emitted below; message content can never
// inject markup. The page's CSP allows no external scripts, so
// instead of a markdown / LaTeX library the common subset is
// implemented here: fenced and inline code, headings, lists,
// emphasis, horizontal rules, and $ / $$ LaTeX delimiters
// (kept as styled source; no math font is shipped).
function renderMarkdown(src) {
  var s = String(src == null ? '' : src);
  var stash = [];
  function keep(html) {
    stash.push(html);
    return '\u0000' + (stash.length - 1) + '\u0000';
  }
  // Fenced code blocks are extracted before escaping so their
  // content stays verbatim (and is escaped on its own).
  // Backticks are spelled \x60 because the surrounding Go raw
  // string literal is delimited with real backticks.
  s = s.replace(/\x60{3}[ \t]*(\w*)[ \t]*\r?\n([\s\S]*?)\x60{3}/g, function (m, lang, code) {
    return keep('<pre class="code-block"><code>' + esc(code.replace(/^\r?\n/, '').replace(/\r?\n[ \t]*$/, '')) + '</code></pre>');
  });
  // Inline code spans are stashed like fenced blocks, before
  // escaping: their content must survive the markdown passes
  // below (a ** or $$ inside a code span is neither
  // emphasis nor math).
  s = s.replace(/\x60([^\x60\n]+)\x60/g, function (m, code) {
    return keep('<code>' + esc(code) + '</code>');
  });
  s = esc(s);
  // LaTeX: display math $$...$$ and inline $...$.
  // The text is escaped already at this point, so
  // it is inserted as-is: escaping it a second
  // time would render the entities literally.
  s = s.replace(/\$\$([\s\S]*?)\$\$/g, function (m, tex) {
    return keep('<span class="math math-display">' + tex.replace(/\s+/g, ' ').trim() + '</span>');
  });
  s = s.replace(/\$([^$\n]+)\$/g, '<span class="math math-inline">$1</span>');
  // Headings and rules
  s = s.replace(/^#### (.*)$/gm, '<h4>$1</h4>');
  s = s.replace(/^### (.*)$/gm, '<h3>$1</h3>');
  s = s.replace(/^## (.*)$/gm, '<h2>$1</h2>');
  s = s.replace(/^# (.*)$/gm, '<h1>$1</h1>');
  s = s.replace(/^(---|\*\*\*|___)[ \t]*$/gm, '<hr>');
  // Emphasis (** before *)
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*\w])\*([^*\n]+)\*/g, '$1<em>$2</em>');
  // Paragraphs, lists and standalone blocks
  s = s.split(/\n{2,}/).map(function (p) {
    p = p.replace(/^[ \t]+/, '').replace(/[ \t]+$/, '');
    if (!p) return '';
    if (/^\u0000\d+\u0000$/.test(p)) return p;
    if (/^<(?:h[1-6]|ul|ol|pre|div|hr)\b/.test(p)) return p;
    var lines = p.split('\n');
    var items = lines.filter(function (l) { return /^[ \t]*(?:[-*+] |\d+\. )/.test(l); });
    if (items.length && items.length === lines.filter(function (l) { return l.trim(); }).length) {
      var ordered = /^\d+\./.test(items[0]);
      return (ordered ? '<ol>' : '<ul>') + items.map(function (l) {
        return '<li>' + l.replace(/^[ \t]*(?:[-*+] |\d+\. )/, '') + '</li>';
      }).join('') + (ordered ? '</ol>' : '</ul>');
    }
    return '<p>' + p.replace(/\n/g, '<br>') + '</p>';
  }).join('');
  // Restoring can expose markers nested inside a restored
  // block (math that contains a code span), so repeat until
  // no marker is left. A pass only resolves markers, it never
  // creates them, so this terminates. A marker with no
  // stashed block (message text that itself contains NUL
  // bytes) is left as-is, never replaced by "undefined".
  var prev;
  do {
  	prev = s;
  	s = s.replace(/\u0000(\d+)\u0000/g, function (m, i) {
  	  var v = stash[+i];
  	  return v === undefined ? m : v;
  	});
  } while (s !== prev);
  return s;
}

// renderThinking renders the model's reasoning as a collapsible
// block above the answer. The open argument says whether the
// block starts expanded: true only while the reasoning is
// still streaming. A complete or historical message is always
// collapsed, whatever else is happening on the page —
// switching chats mid-stream must not pop old thinking
// blocks open.
function renderThinking(text, open) {
  if (!text) return '';
  return '<details class="thinking"' + (open ? ' open' : '') + '><summary>Thinking…</summary><div class="thinking-body">' + renderMarkdown(text) + '</div></details>';
}

function defaultEndpoint() {
  return {
    id: 'self',
    name: 'airouter (this gateway)',
    url: window.location.origin + '/v1',
    api_key: apiKey,
    models: ['smart', 'work', 'fast', 'large']
  };
}

function apiUrl(path) {
  return path + (apiKey ? '?api_key=' + encodeURIComponent(apiKey) : '');
}

// ---------- endpoints ----------

function currentEndpoint() {
  var id = $('endpoint-select').value;
  for (var i = 0; i < endpoints.length; i++) {
    if (endpoints[i].id === id) return endpoints[i];
  }
  return endpoints[0] || null;
}

function renderEndpoints() {
  var sel = $('endpoint-select');
  sel.innerHTML = '';
  endpoints.forEach(function (ep) {
    var opt = document.createElement('option');
    opt.value = ep.id;
    opt.textContent = ep.name || ep.url;
    sel.appendChild(opt);
  });
  if (ui.endpointId) sel.value = ui.endpointId;
  if (!sel.value && endpoints.length) sel.value = endpoints[0].id;
  renderModels();
}

function renderModels() {
  var ep = currentEndpoint();
  var sel = $('model-select');
  sel.innerHTML = '';
  var models = (ep && ep.models) || [];
  models.forEach(function (m) {
    var opt = document.createElement('option');
    opt.value = m;
    opt.textContent = m;
    sel.appendChild(opt);
  });
  var custom = document.createElement('option');
  custom.value = '__custom__';
  custom.textContent = 'custom…';
  sel.appendChild(custom);
  var wanted = ui.model || models[0] || 'smart';
  if (models.indexOf(wanted) >= 0) {
    sel.value = wanted;
    $('model-input').style.display = 'none';
  } else {
    sel.value = '__custom__';
    $('model-input').style.display = '';
    $('model-input').value = wanted;
  }
}

// fetchModels asks the gateway to GET <endpoint.url>/models and
// returns the advertised model ids. OpenAI endpoints answer with
// {"object":"list","data":[{"id":...}]}; a bare array is also
// accepted.
function fetchModels(ep, done) {
  fetch(apiUrl('/chat/models'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ endpoint: { url: ep.url, api_key: ep.api_key } })
  }).then(function (resp) {
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    return resp.json();
  }).then(function (obj) {
    var ids = [];
    if (obj && Array.isArray(obj.data)) {
      obj.data.forEach(function (m) {
        if (m && typeof m.id === 'string' && m.id) ids.push(m.id);
      });
    } else if (Array.isArray(obj)) {
      obj.forEach(function (m) {
        if (typeof m === 'string') ids.push(m);
        else if (m && typeof m.id === 'string' && m.id) ids.push(m.id);
      });
    }
    done(null, ids);
  }).catch(function (err) { done(err); });
}

// refreshModels silently refreshes the selected endpoint's
// roster from its /models path. A failure leaves whatever
// roster is stored, so the dropdown never ends up empty.
function refreshModels() {
  var ep = currentEndpoint();
  if (!ep || !ep.url) return;
  fetchModels(ep, function (err, ids) {
    if (err || !ids.length) return;
    ep.models = ids;
    save(LS_ENDPOINTS, endpoints);
    renderModels();
  });
}

function currentModel() {
  if ($('model-select').value === '__custom__') {
    return $('model-input').value.trim() || 'smart';
  }
  return $('model-select').value || 'smart';
}

function persistUI() {
  ui.endpointId = $('endpoint-select').value;
  ui.model = currentModel();
  ui.protocol = $('protocol-select').value;
  save(LS_UI, ui);
}

function renderSettings() {
  var box = $('endpoint-list');
  box.innerHTML = '';
  endpoints.forEach(function (ep) {
    var row = document.createElement('div');
    row.className = 'ep-row';
    row.innerHTML =
      '<label>Name</label><input type="text" class="ep-name" value="' + esc(ep.name) + '">' +
      '<label>Base URL (OpenAI-compatible, incl. /v1)</label><input type="text" class="ep-url" value="' + esc(ep.url) + '">' +
      '<label>API key</label><input type="text" class="ep-key" value="' + esc(ep.api_key) + '">' +
      '<div class="ep-actions"><button class="ep-fetch">Fetch models</button><button class="ep-save">Save</button><button class="ep-del danger">Delete</button></div>';
    row.querySelector('.ep-save').onclick = function () {
      var name = row.querySelector('.ep-name').value.trim();
      var url = row.querySelector('.ep-url').value.trim();
      var key = row.querySelector('.ep-key').value;
      if (!url) { alert('Base URL is required.'); return; }
      ep.name = name || url;
      ep.url = url;
      ep.api_key = key;
      save(LS_ENDPOINTS, endpoints);
      renderSettings();
      renderEndpoints();
      persistUI();
    };
    row.querySelector('.ep-fetch').onclick = function () {
      var url = row.querySelector('.ep-url').value.trim();
      var key = row.querySelector('.ep-key').value;
      if (!url) { alert('Base URL is required.'); return; }
      fetchModels({ url: url, api_key: key }, function (err, ids) {
        if (err) { alert('Fetching models failed: ' + err.message); return; }
        if (!ids.length) { alert('The endpoint returned no models.'); return; }
        ep.models = ids;
        ep.url = url;
        ep.api_key = key;
        save(LS_ENDPOINTS, endpoints);
        renderSettings();
        renderEndpoints();
        renderModels();
        persistUI();
      });
    };
    row.querySelector('.ep-del').onclick = function () {
      if (ep.id === 'self') {
        alert('The gateway endpoint cannot be deleted.');
        return;
      }
      if (!confirm('Delete endpoint ' + (ep.name || ep.url) + '?')) return;
      endpoints = endpoints.filter(function (x) { return x.id !== ep.id; });
      save(LS_ENDPOINTS, endpoints);
      renderSettings();
      renderEndpoints();
      persistUI();
    };
    box.appendChild(row);
  });
}

// ---------- chats ----------

function chatsSorted() {
  return chats.slice().sort(function (a, b) { return (b.updated || 0) - (a.updated || 0); });
}

function renderChats() {
  var list = $('chat-list');
  list.innerHTML = '';
  chatsSorted().forEach(function (c) {
    var div = document.createElement('div');
    div.className = 'chat-item' + (c.id === currentChatId ? ' active' : '');
    div.textContent = c.title || 'New chat';
    div.title = c.title || '';
    div.onclick = function () {
      currentChatId = c.id;
      renderMessages();
      renderChats();
    };
    list.appendChild(div);
  });
}

function ensureChat() {
  if (currentChatId) {
    for (var i = 0; i < chats.length; i++) {
      if (chats[i].id === currentChatId) return chats[i];
    }
  }
  var chat = { id: newId('c'), title: '', messages: [], created: Date.now(), updated: Date.now() };
  chats.push(chat);
  currentChatId = chat.id;
  return chat;
}

function appendMessage(role, content, stats, thinking) {
  var box = $('messages');
  var wrap = document.createElement('div');
  wrap.className = 'msg ' + role;
  var col = document.createElement('div');
  col.className = 'msg-col';
  var bubble = document.createElement('div');
  bubble.className = 'bubble';
  bubble.innerHTML = renderThinking(thinking, false) + renderMarkdown(content);
  col.appendChild(bubble);
  if (stats) {
    var st = document.createElement('div');
    st.className = 'stats';
    st.textContent = statsText(stats);
    col.appendChild(st);
  }
  wrap.appendChild(col);
  box.appendChild(wrap);
  box.scrollTop = box.scrollHeight;
  return col;
}

function renderMessages() {
  var box = $('messages');
  box.innerHTML = '';
  var chat = null;
  for (var i = 0; i < chats.length; i++) {
    if (chats[i].id === currentChatId) { chat = chats[i]; break; }
  }
  if (!chat) return;
  chat.messages.forEach(function (m) {
    appendMessage(m.role, m.content, m.stats, m.thinking);
  });
  box.scrollTop = box.scrollHeight;
}

function statsText(s) {
  if (!s) return '';
  var parts = ['ctx ' + fmtInt(s.context_tokens) + ' tok'];
  if (s.ttft_ms) parts.push('ttft ' + s.ttft_ms + ' ms');
  if (s.prefill_tokens_per_sec) parts.push('prefill ' + Math.round(s.prefill_tokens_per_sec) + ' t/s');
  if (s.decode_tokens_per_sec) parts.push('decode ' + Math.round(s.decode_tokens_per_sec) + ' t/s');
  return parts.join(' · ');
}

function fmtInt(n) {
  return (n || 0).toLocaleString();
}

// ---------- sending ----------

// toResponsesItems builds the Responses API "input" from the
// stored history. A reasoning model's reasoning items are passed
// back ahead of the answer they produced: the API expects them
// there (the model keeps its reasoning chain), and sending the
// same items every turn keeps the request prefix identical to
// the one the provider cached server-side — dropping them would
// silently turn every later turn's cache into a miss.
function toResponsesItems(messages) {
  var items = [];
  messages.forEach(function (m) {
    if (m.role === 'assistant') {
      var reasoning = m.reasoning || [];
      for (var i = 0; i < reasoning.length; i++) {
        items.push(reasoning[i]);
      }
      if (!reasoning.length && m.thinking) {
        items.push({ type: 'reasoning', summary: [{ type: 'summary_text', text: m.thinking }] });
      }
    }
    items.push({
      type: 'message',
      role: m.role,
      content: [{ type: m.role === 'user' ? 'input_text' : 'output_text', text: m.content }]
    });
  });
  return items;
}

// updateComposer toggles the Send and Stop buttons so an
// in-flight stream can always be cancelled.
function updateComposer() {
  $('send').style.display = generating ? 'none' : '';
  $('stop').style.display = generating ? '' : 'none';
}

function send() {
  if (generating) return;
  var text = $('prompt').value.trim();
  if (!text) return;
  var ep = currentEndpoint();
  if (!ep || !ep.url) { alert('Add an API endpoint first (button at the bottom left).'); return; }
  var model = currentModel();
  var protocol = $('protocol-select').value;

  var chat = ensureChat();
  chat.messages.push({ role: 'user', content: text });
  if (!chat.title) chat.title = text.length > 40 ? text.slice(0, 40) + '…' : text;
  chat.updated = Date.now();
  save(LS_CHATS, chats);
  renderMessages();
  renderChats();
  $('prompt').value = '';
  $('prompt').focus();

  var col = appendMessage('assistant', '', null, null);
  var bubble = col.querySelector('.bubble');
  var statsEl = document.createElement('div');
  statsEl.className = 'stats';
  col.appendChild(statsEl);

  var payload = {
    endpoint: { url: ep.url, api_key: ep.api_key },
    model: model,
    stream: true
  };
  if (protocol === 'responses') {
    payload.input = toResponsesItems(chat.messages);
  } else {
    payload.messages = chat.messages.map(function (m) { return { role: m.role, content: m.content }; });
  }

  generating = true;
  abortCtrl = new AbortController();
  updateComposer();
  var full = '';
  var thinking = '';
  var reasoningItems = null;
  var stats = null;

  doSend(payload, protocol,
    function (chunk, isThinking) {
      if (isThinking) thinking += chunk; else full += chunk;
      bubble.innerHTML = renderThinking(thinking, true) + renderMarkdown(full);
      $('messages').scrollTop = $('messages').scrollHeight;
    },
    function (s) { stats = s; statsEl.textContent = statsText(s); },
    function (items) { reasoningItems = items; }
  ).then(function () {
    generating = false;
    abortCtrl = null;
    updateComposer();
    if (full || thinking) {
      chat.messages.push({ role: 'assistant', content: full, thinking: thinking, reasoning: reasoningItems, stats: stats });
      chat.updated = Date.now();
      save(LS_CHATS, chats);
      renderChats();
      // Re-render now that the stream is over so the
      // thinking block collapses.
      bubble.innerHTML = renderThinking(thinking, false) + renderMarkdown(full);
    } else {
      bubble.textContent = '(empty response)';
    }
  }).catch(function (err) {
    var aborted = err && err.name === 'AbortError';
    generating = false;
    abortCtrl = null;
    updateComposer();
    if (aborted) {
      // Stopped mid-stream: keep the partial reasoning and
      // answer exactly as the model produced them so far.
      if (full || thinking) {
        bubble.innerHTML = renderThinking(thinking, false) + renderMarkdown(full);
        chat.messages.push({ role: 'assistant', content: full, thinking: thinking, reasoning: reasoningItems, stats: stats });
        chat.updated = Date.now();
        save(LS_CHATS, chats);
        renderChats();
      } else {
        bubble.textContent = '(stopped)';
      }
      return;
    }
    // The stream failed (an error event mid-stream, a
    // network error, ...). Whatever the model produced
    // before the failure is kept and saved, with the
    // error appended — its work so far is not thrown away.
    if (full || thinking) {
      bubble.innerHTML = renderThinking(thinking, false) + renderMarkdown(full) + '<p class="stream-error">Error: ' + esc(err) + '</p>';
      chat.messages.push({ role: 'assistant', content: full, thinking: thinking, reasoning: reasoningItems, stats: stats });
      chat.updated = Date.now();
      save(LS_CHATS, chats);
      renderChats();
    } else {
      bubble.textContent = 'Error: ' + err;
    }
  });
}

function doSend(payload, protocol, onText, onStats, onItems) {
  return fetch(apiUrl('/chat/' + protocol), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
    signal: abortCtrl ? abortCtrl.signal : null
  }).then(function (resp) {
    if (!resp.ok) {
      return resp.text().then(function (t) {
        // An error status can carry an SSE stream whose
        // data lines hold the real error JSON; show that
        // instead of the raw stream text.
        var msg = t || '';
        var m = msg.match(/data:[ \t]*(\{[\s\S]*\})/);
        if (m) {
          try {
            var obj = JSON.parse(m[1]);
            if (obj && obj.error) {
              msg = obj.error.message || JSON.stringify(obj.error);
            }
          } catch (e) {}
        }
        throw new Error('HTTP ' + resp.status + ' ' + String(msg).slice(0, 300));
      });
    }
    var ct = resp.headers.get('content-type') || '';
    if (ct.indexOf('text/event-stream') >= 0) {
      return readStream(resp, onText, onStats, onItems);
    }
    return resp.json().then(function (obj) {
      var parts = extractParts(obj, protocol);
      if (parts.thinking) onText(parts.thinking, true);
      if (parts.text) onText(parts.text, false);
      if (parts.reasoning && parts.reasoning.length) onItems(parts.reasoning);
      onStats(statsFromHeaders(resp));
    });
  });
}

function readStream(resp, onText, onStats, onItems) {
  var reader = resp.body.getReader();
  var decoder = new TextDecoder();
  var buf = '';
  function pump() {
    return reader.read().then(function (result) {
      if (result.done) {
        // Flush the decoder so a multi-byte character split
        // across the final chunks is not dropped.
        buf += decoder.decode();
        if (buf.trim()) handleSSE(buf, onText, onStats, onItems);
        return;
      }
      buf += decoder.decode(result.value, { stream: true });
      var idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        var ev = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        handleSSE(ev, onText, onStats, onItems);
      }
      return pump();
    });
  }
  // A failed stream (an error event throws out of
  // handleSSE, the request was aborted) releases
  // the response body instead of leaving it open
  // until GC.
  return pump().catch(function (err) {
    reader.cancel();
    throw err;
  });
}

function handleSSE(rawEvent, onText, onStats, onItems) {
  var lines = rawEvent.split('\n');
  var eventName = '';
  var dataLines = [];
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i];
    // A field name is followed by an optional single space
    // (SSE spec), and an event may carry several data: lines,
    // which the spec joins with newlines.
    if (line.indexOf('event:') === 0) {
      eventName = line.slice(6).replace(/^ /, '');
    } else if (line.indexOf('data:') === 0) {
      dataLines.push(line.slice(5).replace(/^ /, ''));
    }
  }
  var data = dataLines.join('\n');
  if (!data) return;
  if (eventName === 'airouter.stats') {
    try { onStats(JSON.parse(data)); } catch (e) {}
    return;
  }
  if (data === '[DONE]') return;
  var obj;
  try { obj = JSON.parse(data); } catch (e) { return; }
  if (obj.error) {
    throw new Error((obj.error && obj.error.message) || JSON.stringify(obj.error));
  }
  // response.completed carries the whole output, reasoning
  // items included (with the ids the provider gave them).
  // Keeping them lets the next turn send them back, which is
  // what the Responses API expects of a reasoning model and
  // what keeps the provider's server-side prefix cache hit.
  if (obj.type === 'response.completed' && obj.response && obj.response.output && onItems) {
    var reasoning = [];
    for (var i = 0; i < obj.response.output.length; i++) {
      if (obj.response.output[i] && obj.response.output[i].type === 'reasoning') {
        reasoning.push(obj.response.output[i]);
      }
    }
    if (reasoning.length) onItems(reasoning);
  }
  var text = '';
  var isThinking = false;
  if (obj.type === 'response.output_text.delta') {
    text = obj.delta || '';
  } else if (obj.type && obj.type.indexOf('response.reasoning') === 0 && obj.type.slice(-6) === '.delta') {
    // The Responses API streams reasoning summaries either as a
    // top-level delta or inside the event's part object.
    text = obj.delta || (obj.part && obj.part.delta) || '';
    isThinking = true;
  } else if (obj.choices && obj.choices[0] && obj.choices[0].delta) {
    var delta = obj.choices[0].delta;
    if (delta.reasoning_content || delta.reasoning || delta.reasoning_text) {
      text = delta.reasoning_content || delta.reasoning || delta.reasoning_text || '';
      isThinking = true;
    } else {
      text = delta.content || '';
    }
  }
  if (text) onText(text, isThinking);
}

// extractParts pulls the answer text, the reasoning text
// and the raw reasoning items out of a complete
// (non-streaming) response of either API shape.
function extractParts(obj, protocol) {
  var parts = { text: '', thinking: '', reasoning: [] };
  if (!obj) return parts;
  if (protocol === 'responses') {
    if (obj.output) {
      for (var i = 0; i < obj.output.length; i++) {
        var item = obj.output[i];
        if (item.type === 'message' && item.content) {
          for (var j = 0; j < item.content.length; j++) {
            if (item.content[j].type === 'output_text' || item.content[j].type === 'text') {
              parts.text += item.content[j].text || '';
            }
          }
        } else if (item.type === 'reasoning') {
          parts.reasoning.push(item);
          var added = false;
          var summary = item.summary || [];
          for (var k = 0; k < summary.length; k++) {
            if (summary[k] && summary[k].text) {
              parts.thinking += summary[k].text;
              added = true;
            }
          }
          if (!added && item.text) parts.thinking += item.text;
        }
      }
    }
    return parts;
  }
  if (obj.choices && obj.choices[0] && obj.choices[0].message) {
    var msg = obj.choices[0].message;
    parts.text = msg.content || '';
    parts.thinking = msg.reasoning_content || msg.reasoning || msg.reasoning_text || '';
  }
  return parts;
}

function statsFromHeaders(resp) {
  var ct = resp.headers.get('X-Chat-Context-Tokens');
  if (!ct) return null;
  return {
    context_tokens: parseInt(ct, 10) || 0,
    ttft_ms: parseInt(resp.headers.get('X-Chat-TTFT-Ms'), 10) || 0,
    elapsed_ms: parseInt(resp.headers.get('X-Chat-Elapsed-Ms'), 10) || 0,
    prefill_tokens_per_sec: parseFloat(resp.headers.get('X-Chat-Prefill-Tokens-Per-Sec')) || 0,
    decode_tokens_per_sec: parseFloat(resp.headers.get('X-Chat-Decode-Tokens-Per-Sec')) || 0
  };
}

// ---------- wiring ----------

function init() {
  endpoints = load(LS_ENDPOINTS, []);
  chats = load(LS_CHATS, []);
  ui = load(LS_UI, {});
  if (!Array.isArray(endpoints)) endpoints = [];
  if (!Array.isArray(chats)) chats = [];
  if (typeof ui !== 'object' || ui === null) ui = {};

  // The gateway itself is always the default endpoint; its key is
  // refreshed from the page URL on every load.
  var self = null;
  for (var i = 0; i < endpoints.length; i++) {
    if (endpoints[i].id === 'self') { self = endpoints[i]; break; }
  }
  if (!self) {
    self = defaultEndpoint();
    endpoints.unshift(self);
  } else {
    self.api_key = apiKey;
  }
  save(LS_ENDPOINTS, endpoints);

  // A chat whose id was lost (a hand-edited
  // store) is repaired in place, and the repair
  // is persisted so the next load does not mint
  // a fresh id for it.
  var chatIdsRepaired = false;
  chats.forEach(function (c) {
    if (!c.id) { c.id = newId('c'); chatIdsRepaired = true; }
  });
  if (chatIdsRepaired) save(LS_CHATS, chats);

  renderEndpoints();
  if (ui.protocol) $('protocol-select').value = ui.protocol;
  renderModels();
  refreshModels();

  var sorted = chatsSorted();
  if (sorted.length) {
    currentChatId = sorted[0].id;
  } else {
    var chat = { id: newId('c'), title: '', messages: [], created: Date.now(), updated: Date.now() };
    chats.push(chat);
    currentChatId = chat.id;
  }
  renderChats();
  renderMessages();

  $('endpoint-select').onchange = function () {
    persistUI();
    refreshModels();
  };
  $('protocol-select').onchange = persistUI;
  $('model-select').onchange = function () {
    var custom = $('model-select').value === '__custom__';
    $('model-input').style.display = custom ? '' : 'none';
    if (custom) $('model-input').focus();
    persistUI();
  };
  $('model-input').onchange = persistUI;
  $('send').onclick = send;
  $('stop').onclick = function () {
    if (abortCtrl) abortCtrl.abort();
  };
  $('prompt').addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
  $('new-chat').onclick = function () {
    if (generating) return;
    var chat = { id: newId('c'), title: '', messages: [], created: Date.now(), updated: Date.now() };
    chats.push(chat);
    currentChatId = chat.id;
    save(LS_CHATS, chats);
    renderChats();
    renderMessages();
    $('prompt').focus();
  };
  $('delete-all').onclick = function () {
    if (generating) return;
    if (!chats.length) return;
    if (!confirm('Delete all ' + chats.length + ' past chats? This cannot be undone.')) return;
    chats = [];
    currentChatId = null;
    save(LS_CHATS, chats);
    var chat = { id: newId('c'), title: '', messages: [], created: Date.now(), updated: Date.now() };
    chats.push(chat);
    currentChatId = chat.id;
    save(LS_CHATS, chats);
    renderChats();
    renderMessages();
  };
  $('settings-btn').onclick = function () {
    renderSettings();
    $('modal-backdrop').style.display = 'flex';
  };
  $('modal-close').onclick = function () {
    $('modal-backdrop').style.display = 'none';
  };
  $('modal-backdrop').onclick = function (e) {
    if (e.target === $('modal-backdrop')) $('modal-backdrop').style.display = 'none';
  };
  $('ep-add').onclick = function () {
    endpoints.push({ id: newId('ep'), name: 'New endpoint', url: 'http://localhost:8080/v1', api_key: '', models: [] });
    save(LS_ENDPOINTS, endpoints);
    renderSettings();
  };
  updateComposer();
  $('prompt').focus();
}

init();
</script>
</body>
</html>`
