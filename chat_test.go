package main

// chat_test.go — tests for the web chat proxy: endpoint
// forwarding, both streaming grammars, the stats measurement,
// and auth. The upstream is always an httptest server so the
// proxy's parsing and timing logic is exercised end to end
// without touching the network.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const chatTestKey = "chat-test-key"

func newChatTestGateway(t *testing.T) *GatewayContext {
	t.Helper()
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	proxy := NewProxy(cfg)
	return NewGatewayContext(router, proxy, cfg, "", chatTestKey)
}

func chatTestRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+chatTestKey)
	return req
}

// TestChatPageServedBehindAuth pins the two facts that make the
// UI reachable from a browser: the page answers on GET with the
// HTML, and the ?api_key= query form (the only one a typed URL
// can carry) authenticates.
func TestChatPageServedBehindAuth(t *testing.T) {
	g := newChatTestGateway(t)

	rec := httptest.NewRecorder()
	g.HandleChatPage(rec, chatTestRequest(t, http.MethodGet, "/chat?api_key="+chatTestKey, ""))
	if rec.Code != 200 {
		t.Fatalf("expected 200 with api_key query param, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html, got %s", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control no-cache like every other HTML surface, got %q", cc)
	}
	if body := rec.Body.String(); !strings.Contains(body, "airouter chat") {
		t.Errorf("expected the chat UI in the response body")
	}

	rec = httptest.NewRecorder()
	g.HandleChatPage(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))
	if rec.Code != 401 {
		t.Errorf("expected 401 without a key, got %d", rec.Code)
	}
}

// TestChatProxyRequiresAuth covers the proxy endpoints
// themselves: no key, wrong key, and wrong method are all
// rejected before anything is forwarded.
func TestChatProxyRequiresAuth(t *testing.T) {
	g := newChatTestGateway(t)
	body := `{"endpoint":{"url":"http://localhost:1/v1"},"model":"smart","messages":[]}`

	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body)))
	if rec.Code != 401 {
		t.Errorf("expected 401 without a key, got %d", rec.Code)
	}

	req := chatTestRequest(t, http.MethodPost, "/chat/completions", body)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec = httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 with a wrong key, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodGet, "/chat/completions", ""))
	if rec.Code != 405 {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

// TestChatProxyMissingEndpointIsRejected covers the 400 path
// for a body without an endpoint object.
func TestChatProxyMissingEndpointIsRejected(t *testing.T) {
	g := newChatTestGateway(t)
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", `{"model":"smart"}`))
	if rec.Code != 400 {
		t.Errorf("expected 400 for a missing endpoint, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", `not json`))
	if rec.Code != 400 {
		t.Errorf("expected 400 for a malformed body, got %d", rec.Code)
	}
}

// TestChatProxyForwardsToEndpointAndKey proves the proxy
// targets the endpoint named in the body (not the gateway's own
// providers), appends the right upstream path, and presents
// the endpoint's API key.
func TestChatProxyForwardsToEndpointAndKey(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1","api_key":"upstream-key"},"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("expected upstream path /v1/chat/completions, got %s", gotPath)
	}
	if gotAuth != "Bearer upstream-key" {
		t.Errorf("expected the endpoint's Bearer key upstream, got %q", gotAuth)
	}
	if gotBody["model"] != "smart" {
		t.Errorf("expected the request fields forwarded verbatim, model=%v", gotBody["model"])
	}
	if _, leaked := gotBody["endpoint"]; leaked {
		t.Errorf("endpoint object must be stripped before forwarding")
	}
}

// TestChatProxyCompletionsStreamingMetrics is the core
// correctness property of the streaming measurement: the
// client sees the upstream stream unmodified, followed by one
// airouter.stats event whose context tokens come from the
// upstream usage chunk and whose speeds are derived from the
// measured first-token time.
func TestChatProxyCompletionsStreamingMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		flusher.Flush()
		// A pause before the first content token makes TTFT
		// measurable and deterministic.
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hel") || !strings.Contains(body, "lo") {
		t.Fatalf("expected the stream content to pass through, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected the [DONE] sentinel to pass through")
	}
	idx := strings.Index(body, "event: "+chatStatsEvent)
	if idx < 0 {
		t.Fatalf("expected an %s event appended to the stream, got: %s", chatStatsEvent, body)
	}
	dataIdx := strings.Index(body[idx:], "data: ")
	if dataIdx < 0 {
		t.Fatalf("stats event has no data line: %s", body[idx:])
	}
	dataLine := body[idx+dataIdx+len("data: "):]
	if nl := strings.Index(dataLine, "\n"); nl >= 0 {
		dataLine = dataLine[:nl]
	}
	var stats chatStats
	if err := json.Unmarshal([]byte(dataLine), &stats); err != nil {
		t.Fatalf("stats event is not valid JSON (%v): %s", err, dataLine)
	}
	if stats.ContextTokens != 100 {
		t.Errorf("expected context tokens 100 from the usage chunk, got %d", stats.ContextTokens)
	}
	if stats.TTFTMs < 10 {
		t.Errorf("expected TTFT >= 10ms after a 20ms pause, got %dms", stats.TTFTMs)
	}
	if stats.PrefillTPS <= 0 {
		t.Errorf("expected a positive prefill t/s, got %f", stats.PrefillTPS)
	}
	if stats.DecodeTPS <= 0 {
		t.Errorf("expected a positive decode t/s, got %f", stats.DecodeTPS)
	}
	if stats.ElapsedMs < stats.TTFTMs {
		t.Errorf("elapsed (%dms) must not precede TTFT (%dms)", stats.ElapsedMs, stats.TTFTMs)
	}
}

// TestChatProxyTimesFromRequestStart pins when the
// stats clock starts: before the upstream request is
// sent, not when its response headers arrive. An
// upstream that walks a fallback chain (the gateway
// itself does, between its providers) sends nothing
// — headers included — until it has a first chunk,
// so a clock that starts at the headers would report
// a near-zero TTFT for a request the user waited
// seconds for. Here the upstream delays its headers
// by 40ms and its first content token by another
// 30ms; TTFT must cover both waits.
func TestChatProxyTimesFromRequestStart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		time.Sleep(30 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	idx := strings.Index(rec.Body.String(), "event: "+chatStatsEvent)
	if idx < 0 {
		t.Fatalf("expected an %s event, got: %s", chatStatsEvent, rec.Body.String())
	}
	dataIdx := strings.Index(rec.Body.String()[idx:], "data: ")
	dataLine := rec.Body.String()[idx+dataIdx+len("data: "):]
	if nl := strings.Index(dataLine, "\n"); nl >= 0 {
		dataLine = dataLine[:nl]
	}
	var stats chatStats
	if err := json.Unmarshal([]byte(dataLine), &stats); err != nil {
		t.Fatalf("stats event is not valid JSON (%v): %s", err, dataLine)
	}
	// Both sleeps (40ms before headers, 30ms before
	// the first token) plus scheduling slack. A clock
	// started at the response headers would report
	// only the 30ms.
	if stats.TTFTMs < 60 {
		t.Errorf("expected TTFT >= 60ms (40ms header wait + 30ms token wait), got %dms", stats.TTFTMs)
	}
	if stats.ElapsedMs < stats.TTFTMs {
		t.Errorf("elapsed (%dms) must not precede TTFT (%dms)", stats.ElapsedMs, stats.TTFTMs)
	}
	if stats.PrefillTPS <= 0 {
		t.Errorf("expected a positive prefill t/s, got %f", stats.PrefillTPS)
	}
}

// TestChatProxyResponsesStreamingMetrics covers the Responses
// API grammar: text deltas in response.output_text.delta events
// and usage nested inside response.completed.
func TestChatProxyResponsesStreamingMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" there\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":50,\"output_tokens\":3}}}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatResponses(rec, chatTestRequest(t, http.MethodPost, "/chat/responses", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hi") || !strings.Contains(body, " there") {
		t.Fatalf("expected the responses stream content to pass through, got: %s", body)
	}
	idx := strings.Index(body, "event: "+chatStatsEvent)
	if idx < 0 {
		t.Fatalf("expected an %s event appended to the stream, got: %s", chatStatsEvent, body)
	}
	start := strings.Index(body[idx:], "data: ") + idx + len("data: ")
	dataLine := body[start:]
	if nl := strings.Index(dataLine, "\n"); nl >= 0 {
		dataLine = dataLine[:nl]
	}
	var stats chatStats
	if err := json.Unmarshal([]byte(dataLine), &stats); err != nil {
		t.Fatalf("stats event is not valid JSON (%v): %s", err, dataLine)
	}
	if stats.ContextTokens != 50 {
		t.Errorf("expected context tokens 50 from response.completed, got %d", stats.ContextTokens)
	}
	if stats.PrefillTPS <= 0 || stats.DecodeTPS <= 0 {
		t.Errorf("expected positive speeds, got prefill=%f decode=%f", stats.PrefillTPS, stats.DecodeTPS)
	}
}

// TestChatProxyReasoningStream covers a reasoning model's
// stream: reasoning_content chunks pass through verbatim like
// any other delta, and they start the TTFT clock (the prefill
// window ends at the model's first output token, reasoning
// included — not at the first answer token).
func TestChatProxyReasoningStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		flusher.Flush()
		// A pause before the first reasoning token makes
		// TTFT measurable and deterministic.
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Let \"}}]}\n\n")
		flusher.Flush()
		// A second pause before the answer: if TTFT only
		// watched answer content it would measure ~80ms.
		time.Sleep(60 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"me think\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":3}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Let ", "me think", "Hi"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the reasoning and answer to pass through verbatim (missing %q): %s", want, body)
		}
	}
	idx := strings.Index(body, "event: "+chatStatsEvent)
	if idx < 0 {
		t.Fatalf("expected an %s event appended to the stream, got: %s", chatStatsEvent, body)
	}
	start := strings.Index(body[idx:], "data: ") + idx + len("data: ")
	dataLine := body[start:]
	if nl := strings.Index(dataLine, "\n"); nl >= 0 {
		dataLine = dataLine[:nl]
	}
	var stats chatStats
	if err := json.Unmarshal([]byte(dataLine), &stats); err != nil {
		t.Fatalf("stats event is not valid JSON (%v): %s", err, dataLine)
	}
	if stats.ContextTokens != 100 {
		t.Errorf("expected context tokens 100 from the usage chunk, got %d", stats.ContextTokens)
	}
	// The first reasoning token arrived after ~20ms; the first
	// answer token after ~80ms. TTFT must follow the reasoning.
	if stats.TTFTMs < 10 || stats.TTFTMs >= 60 {
		t.Errorf("expected TTFT to track the first reasoning chunk (~20ms), got %dms", stats.TTFTMs)
	}
	if stats.PrefillTPS <= 0 || stats.DecodeTPS <= 0 {
		t.Errorf("expected positive speeds, got prefill=%f decode=%f", stats.PrefillTPS, stats.DecodeTPS)
	}
}

// TestChatProxyStreamContentTypeCaseInsensitive pins that
// the streaming path is chosen by the Content-Type value,
// not its letter case: header values are case-insensitive
// (RFC 9110), so a provider that spells it "Text/Event-
// Stream" must still stream live and get the appended
// stats event — not be read as a complete JSON body.
func TestChatProxyStreamContentTypeCaseInsensitive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Text/Event-Stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: "+chatStatsEvent) {
		t.Errorf("expected the streaming path (appended %s event), got: %s", chatStatsEvent, body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("expected the upstream delta passed through, got: %s", body)
	}
}

// TestChatProxyNonStreamingHeaders covers the non-streaming
// path: usage is read from the JSON body and the stats travel
// as headers (there is no body yet when headers are sent).
// Decode t/s is not measurable without a stream, so it must be
// absent rather than invented.
func TestChatProxyNonStreamingHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":42,"completion_tokens":7}}`)
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Hello") {
		t.Errorf("expected the upstream body to pass through, got: %s", rec.Body.String())
	}
	if got := rec.Header().Get(chatHeaderContextTokens); got != "42" {
		t.Errorf("expected %s=42, got %q", chatHeaderContextTokens, got)
	}
	if got := rec.Header().Get(chatHeaderTTFTMs); got == "" {
		t.Errorf("expected %s to be set", chatHeaderTTFTMs)
	}
	if got := rec.Header().Get(chatHeaderPrefillTPS); got == "" {
		t.Errorf("expected %s to be set", chatHeaderPrefillTPS)
	}
	if got := rec.Header().Get(chatHeaderDecodeTPS); got != "" {
		t.Errorf("expected no decode t/s without a stream, got %q", got)
	}
}

// TestChatProxyInjectsStreamOptions proves the proxy asks the
// upstream for usage in the final stream chunk — but never
// overrides a stream_options the caller set themselves.
func TestChatProxyInjectsStreamOptions(t *testing.T) {
	var upstreamBody map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &upstreamBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"messages":[]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	opts, ok := upstreamBody["stream_options"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected stream_options to be injected, got: %v", upstreamBody["stream_options"])
	}
	if opts["include_usage"] != true {
		t.Errorf("expected include_usage=true, got %v", opts["include_usage"])
	}

	// Caller-provided stream_options must survive untouched.
	upstreamBody = nil
	payload = `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","stream":true,"stream_options":{"include_usage":false},"messages":[]}`
	rec = httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	opts, ok = upstreamBody["stream_options"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected stream_options to be forwarded, got: %v", upstreamBody["stream_options"])
	}
	if opts["include_usage"] != false {
		t.Errorf("expected the caller's include_usage=false preserved, got %v", opts["include_usage"])
	}
}

// TestChatProxyPreservesMessagesBytes pins the prefix-cache
// property of the proxy: the conversation history is forwarded
// byte-for-byte. chatSplitEndpoint keeps every field as a
// json.RawMessage, so the messages array (and the Responses
// input array) reach the upstream with their original key
// order and spacing intact — never re-serialized. A
// re-serializing proxy would normalize the history, changing
// the request prefix and turning the provider's server-side
// prefix cache into a miss on every turn.
func TestChatProxyPreservesMessagesBytes(t *testing.T) {
	var gotBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	// The message uses a non-standard key order (content
	// before role) on purpose: a proxy that re-marshals the
	// history would sort the keys and change the prefix.
	messages := `{"content":"hi","role":"user"}`
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","messages":[` + messages + `]}`

	// The same request twice must produce the same upstream
	// bytes: a stable prefix is what the provider caches.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))
		if rec.Code != 200 {
			t.Fatalf("request %d: expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if len(gotBodies) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(gotBodies))
	}
	if gotBodies[0] != gotBodies[1] {
		t.Errorf("identical requests must forward identical bytes:\n  %s\n  %s", gotBodies[0], gotBodies[1])
	}
	if !strings.Contains(gotBodies[0], `"content":"hi","role":"user"`) {
		t.Errorf("expected the messages bytes preserved verbatim, got: %s", gotBodies[0])
	}

	// The Responses API carries the history in "input"; its
	// bytes are preserved the same way.
	input := `[{"type":"message","content":[{"type":"input_text","text":"hi"}],"role":"user"}]`
	respPayload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","input":` + input + `}`
	rec := httptest.NewRecorder()
	g.HandleWebChatResponses(rec, chatTestRequest(t, http.MethodPost, "/chat/responses", respPayload))
	if rec.Code != 200 {
		t.Fatalf("responses: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	last := gotBodies[len(gotBodies)-1]
	if !strings.Contains(last, input) {
		t.Errorf("expected the input bytes preserved verbatim, got: %s", last)
	}
}

// TestChatProxyUpstreamErrorPassthrough covers a failing
// upstream: the status and error body reach the client and no
// stats are fabricated.
func TestChatProxyUpstreamErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	payload := `{"endpoint":{"url":"` + upstream.URL + `/v1"},"model":"smart","messages":[]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))

	if rec.Code != 500 {
		t.Fatalf("expected the upstream 500 to pass through, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("expected the upstream error body, got: %s", rec.Body.String())
	}
	if rec.Header().Get(chatHeaderContextTokens) != "" {
		t.Errorf("expected no stats headers on an error response")
	}
}

// TestChatProxyUnreachableEndpoint covers a connection failure
// (not an HTTP error): the client gets a 502, not a hang.
func TestChatProxyUnreachableEndpoint(t *testing.T) {
	g := newChatTestGateway(t)
	// Port 1 on localhost refuses connections immediately.
	payload := `{"endpoint":{"url":"http://127.0.0.1:1/v1"},"model":"smart","messages":[]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", payload))
	if rec.Code != 502 {
		t.Errorf("expected 502 for an unreachable endpoint, got %d", rec.Code)
	}
}

// TestChatComputeStats pins the arithmetic: prefill speed is
// context tokens over the time to first token, decode speed is
// completion tokens over the remaining time, and a non-streaming
// response (TTFT == elapsed) reports no decode speed.
func TestChatComputeStats(t *testing.T) {
	stats := chatComputeStats(100, 50, 200*time.Millisecond, 1200*time.Millisecond)
	if stats.ContextTokens != 100 || stats.TTFTMs != 200 || stats.ElapsedMs != 1200 {
		t.Errorf("unexpected base fields: %+v", stats)
	}
	if stats.PrefillTPS < 400 || stats.PrefillTPS > 600 {
		t.Errorf("expected prefill ~500 t/s (100 tok / 200ms), got %f", stats.PrefillTPS)
	}
	if stats.DecodeTPS < 40 || stats.DecodeTPS > 60 {
		t.Errorf("expected decode ~50 t/s (50 tok / 1000ms), got %f", stats.DecodeTPS)
	}

	// Non-streaming: no window to decode in.
	stats = chatComputeStats(100, 50, time.Second, time.Second)
	if stats.DecodeTPS != 0 {
		t.Errorf("expected no decode t/s when TTFT equals elapsed, got %f", stats.DecodeTPS)
	}
	if stats.PrefillTPS != 100 {
		t.Errorf("expected prefill 100 t/s, got %f", stats.PrefillTPS)
	}

	// A first token faster than the clock floor must not divide
	// by zero.
	stats = chatComputeStats(100, 50, 0, 2*time.Millisecond)
	if stats.PrefillTPS <= 0 || stats.DecodeTPS <= 0 {
		t.Errorf("expected floored durations to yield positive speeds, got %+v", stats)
	}
}

// TestEstimateChatTokens covers the fallback context estimate:
// four characters per token over string values, and a
// whole-body fallback for non-JSON.
func TestEstimateChatTokens(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello world"}],"stream":true}`)
	// "user" (4) + "hello world" (11) = 15 chars -> 3 tokens.
	if got := estimateChatTokens(body); got != 3 {
		t.Errorf("expected 3 tokens, got %d", got)
	}
	if got := estimateChatTokens([]byte("not json at all")); got != len("not json at all")/4 {
		t.Errorf("expected the raw-length fallback, got %d", got)
	}
}

// TestChatEventParsing pins the two-grammar event parsing the
// stream scan depends on: content only from real text deltas
// (never role-only chunks), and usage from either shape.
func TestChatEventParsing(t *testing.T) {
	if got := chatEventText([]byte(`{"choices":[{"delta":{"role":"assistant"}}]}`)); got != "" {
		t.Errorf("role-only chunk must not count as content, got %q", got)
	}
	if got := chatEventText([]byte(`{"choices":[{"delta":{"content":"Hi"}}]}`)); got != "Hi" {
		t.Errorf("expected completions delta, got %q", got)
	}
	if got := chatEventText([]byte(`{"type":"response.output_text.delta","delta":"Yo"}`)); got != "Yo" {
		t.Errorf("expected responses delta, got %q", got)
	}

	u := chatEventUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`))
	if u == nil || u.promptTokens() != 10 || u.completionTokens() != 1 {
		t.Errorf("expected completions usage, got %+v", u)
	}
	u = chatEventUsage([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":2}}}`))
	if u == nil || u.promptTokens() != 20 || u.completionTokens() != 2 {
		t.Errorf("expected responses usage, got %+v", u)
	}
	if u := chatEventUsage([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)); u != nil {
		t.Errorf("content chunk must not carry usage, got %+v", u)
	}

	// Non-streaming bodies of both shapes.
	u = chatUsageFromBody([]byte(`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	if u == nil || u.promptTokens() != 5 {
		t.Errorf("expected completions body usage, got %+v", u)
	}
	u = chatUsageFromBody([]byte(`{"output":[{"type":"message"}],"usage":{"input_tokens":6,"output_tokens":1}}`))
	if u == nil || u.promptTokens() != 6 {
		t.Errorf("expected responses body usage, got %+v", u)
	}
}

// TestChatEventReasoningParsing pins the reasoning parsing the
// stream scan depends on: reasoning deltas count as content so a
// reasoning model's first token starts the TTFT clock, from both
// grammars and every field spelling providers use.
func TestChatEventReasoningParsing(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"completions reasoning_content", `{"choices":[{"delta":{"reasoning_content":"Think"}}]}`, "Think"},
		{"completions reasoning", `{"choices":[{"delta":{"reasoning":"hmm"}}]}`, "hmm"},
		{"completions reasoning_text", `{"choices":[{"delta":{"reasoning_text":"um"}}]}`, "um"},
		{"completions content without reasoning", `{"choices":[{"delta":{"content":"Hi"}}]}`, "Hi"},
		{"responses summary text delta", `{"type":"response.reasoning_summary_text.delta","delta":"step"}`, "step"},
		{"responses summary part delta", `{"type":"response.reasoning_summary_part.delta","part":{"delta":"part"}}`, "part"},
		{"responses generic reasoning delta", `{"type":"response.reasoning.delta","delta":"raw"}`, "raw"},
	}
	for _, tc := range cases {
		if got := chatEventText([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: expected %q, got %q", tc.name, tc.want, got)
		}
	}
	// .done events repeat the full summary text; they must not
	// count as content or the reasoning would be measured twice.
	if got := chatEventText([]byte(`{"type":"response.reasoning_summary_text.done","delta":"step"}`)); got != "" {
		t.Errorf("done event must not count as content, got %q", got)
	}
	// A part-added event carries no delta of its own.
	if got := chatEventText([]byte(`{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text"}}`)); got != "" {
		t.Errorf("part-added event must not count as content, got %q", got)
	}
}

// TestChatSplitEndpoint covers the body split: the endpoint
// object is extracted and removed, the stream flag is read, and
// everything else stays in the forwarded fields.
func TestChatSplitEndpoint(t *testing.T) {
	fields, ep, stream, err := chatSplitEndpoint([]byte(`{"endpoint":{"url":"http://x/v1","api_key":"k"},"model":"smart","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep == nil || ep.URL != "http://x/v1" || ep.APIKey != "k" {
		t.Errorf("unexpected endpoint ref: %+v", ep)
	}
	if !stream {
		t.Errorf("expected stream=true")
	}
	if _, ok := fields["endpoint"]; ok {
		t.Errorf("endpoint must be removed from the forwarded fields")
	}
	if string(fields["model"]) != `"smart"` {
		t.Errorf("expected model preserved, got %s", fields["model"])
	}
}

// TestWebChatModelsProxy pins the roster fetch: the endpoint
// ref's URL gets /models appended (trailing slashes tolerated),
// the API key travels as a Bearer header, and the upstream's
// status and body are passed through verbatim.
func TestWebChatModelsProxy(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"smart"},{"id":"work"}]}`))
	}))
	t.Cleanup(upstream.Close)

	g := newChatTestGateway(t)
	body := fmt.Sprintf(`{"endpoint":{"url":%q,"api_key":"upstream-key"}}`, upstream.URL+"/v1/")
	rec := httptest.NewRecorder()
	g.HandleWebChatModels(rec, chatTestRequest(t, http.MethodPost, "/chat/models", body))
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Errorf("expected upstream path /v1/models, got %s", gotPath)
	}
	if gotAuth != "Bearer upstream-key" {
		t.Errorf("expected bearer auth upstream, got %q", gotAuth)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"smart"`) {
		t.Errorf("expected the upstream roster verbatim, got %s", body)
	}
}

// TestWebChatModelsErrors covers the failure modes: auth,
// method, malformed body, missing url, unreachable upstream
// and an upstream error status (passed through, not masked).
func TestWebChatModelsErrors(t *testing.T) {
	g := newChatTestGateway(t)

	rec := httptest.NewRecorder()
	g.HandleWebChatModels(rec, httptest.NewRequest(http.MethodPost, "/chat/models", strings.NewReader(`{}`)))
	if rec.Code != 401 {
		t.Errorf("expected 401 without a key, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	g.HandleWebChatModels(rec, chatTestRequest(t, http.MethodGet, "/chat/models", ""))
	if rec.Code != 405 {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	g.HandleWebChatModels(rec, chatTestRequest(t, http.MethodPost, "/chat/models", `not json`))
	if rec.Code != 400 {
		t.Errorf("expected 400 for bad json, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	g.HandleWebChatModels(rec, chatTestRequest(t, http.MethodPost, "/chat/models", `{"endpoint":{}}`))
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing url, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	g.HandleWebChatModels(rec, chatTestRequest(t, http.MethodPost, "/chat/models", `{"endpoint":{"url":"http://localhost:1/v1"}}`))
	if rec.Code != 502 {
		t.Errorf("expected 502 for unreachable upstream, got %d", rec.Code)
	}

	errUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	t.Cleanup(errUpstream.Close)
	rec = httptest.NewRecorder()
	g.HandleWebChatModels(rec, chatTestRequest(t, http.MethodPost, "/chat/models", fmt.Sprintf(`{"endpoint":{"url":%q}}`, errUpstream.URL)))
	if rec.Code != 403 {
		t.Errorf("expected upstream 403 passed through, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden") {
		t.Errorf("expected the upstream error body, got %s", rec.Body.String())
	}
}

// TestChatPageComposerControls pins the page features the
// chat UI must carry: a Stop button that cancels an
// in-flight stream through an AbortController, the
// built-in markdown renderer (the page's CSP allows no
// external scripts, so formatting cannot come from a CDN),
// and the /models roster fetch (the model list is read
// from each endpoint, never configured by hand).
func TestChatPageComposerControls(t *testing.T) {
	for _, want := range []string{
		`id="stop"`,
		"abortCtrl = new AbortController()",
		`signal: signal || (abortCtrl ? abortCtrl.signal : null)`,
		"err.name === 'AbortError'",
		"function renderMarkdown",
		"bubble.innerHTML = renderThinking(thinking, true) + renderMarkdown(full)",
		"function fetchModels",
		"function refreshModels",
		"apiUrl('/chat/models')",
		"class=\"ep-fetch\"",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
	if strings.Contains(chatHTML, "ep-models") {
		t.Errorf("the hand-edited models input must be gone; models come from /models")
	}
}

// TestChatPageThinkingUI pins the reasoning display: thinking
// is parsed from both grammars' deltas (streaming and not),
// rendered as its own collapsible block above the answer, and
// persisted with the message so a reload still shows it.
func TestChatPageThinkingUI(t *testing.T) {
	for _, want := range []string{
		"function renderThinking",
		"class=\"thinking\"",
		"delta.reasoning_content || delta.reasoning || delta.reasoning_text",
		"obj.type.indexOf('response.reasoning') === 0",
		"obj.part && obj.part.delta",
		"function extractParts",
		"item.type === 'reasoning'",
		"msg.reasoning_content || msg.reasoning || msg.reasoning_text",
		"bubble.innerHTML = renderThinking(thinking, true) + renderMarkdown(full)",
		"content: full, thinking: thinking, reasoning: reasoningItems, stats: stats",
		"appendMessage(m.role, m.content, m.stats, m.thinking)",
		"if (text) onText(text, isThinking);",
		"onText(parts.thinking, true);",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
	if strings.Contains(chatHTML, "function extractContent") {
		t.Errorf("extractContent must be replaced by extractParts (it drops reasoning)")
	}
}

// TestChatPageStreamRobustness pins the SSE client fixes:
// endpoint saves validate before mutating the endpoint, the
// delete-all button is blocked mid-stream like new-chat, an
// SSE event's data lines are joined per the spec (and the
// field-name space is optional), the TextDecoder is flushed
// when the stream ends, and a NUL marker with no stashed
// block is left as-is instead of rendering "undefined".
func TestChatPageStreamRobustness(t *testing.T) {
	for _, want := range []string{
		"dataLines.push(line.slice(5)",
		"var data = dataLines.join('\\n');",
		"buf += decoder.decode();",
		"ep.name = name || url;",
		"return v === undefined ? m : v;",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
	// send(), new-chat and delete-all are all blocked
	// while a stream is in flight (or a benchmark is
	// running).
	if got := strings.Count(chatHTML, "if (generating || benchRunning) return;"); got != 3 {
		t.Errorf("expected send/new-chat/delete-all all blocked mid-stream, found %d guards", got)
	}
}

// TestChatPageReasoningRoundTrip pins the prefix-cache
// behavior of the Responses API client: a reasoning model's
// reasoning items are captured from the completed response,
// stored with the message, and passed back ahead of the
// answer on the next turn. The Responses API expects them
// there (the model keeps its reasoning chain) and resending
// the same items every turn keeps the request prefix
// identical to the one the provider cached server-side —
// dropping them would silently turn every later turn's cache
// into a miss.
func TestChatPageReasoningRoundTrip(t *testing.T) {
	for _, want := range []string{
		// Capture the reasoning items from a completed response
		// (they carry the ids the provider assigned).
		"obj.response.output[i].type === 'reasoning'",
		// A non-streaming response carries them in output[].
		"parts.reasoning.push(item)",
		// The items are stored with the message and read back
		// out of the history.
		"reasoning: reasoningItems",
		"m.reasoning",
		// And rebuilt into the request input ahead of the
		// assistant message.
		"items.push(reasoning[i])",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
}

// TestChatPageAudit3Fixes pins the client-side robustness
// fixes: a mid-stream error keeps the answer produced so far
// (with the error appended) instead of wiping it, the
// thinking block is collapsed for any finished or historical
// message (not keyed off the global streaming flag), the
// gateway endpoint cannot be deleted, inline code spans are
// stashed before escaping (so ** and $$ inside code are not
// reformatted), marker restore loops to a fixpoint (so a
// marker nested inside a restored block recovers), and an
// error status carrying an SSE stream shows the stream's
// error message rather than the raw event text.
func TestChatPageAudit3Fixes(t *testing.T) {
	for _, want := range []string{
		// Mid-stream error: partials kept, error appended,
		// message saved.
		`<p class="stream-error">Error: ' + esc(err) + '</p>`,
		// The thinking block is explicitly collapsed for
		// finished and historical messages.
		"renderThinking(thinking, false)",
		// The gateway endpoint is not deletable.
		"ep.id === 'self'",
		// Inline code spans are stashed before escaping.
		"keep('<code>' + esc(code)",
		// Marker restore repeats until no marker is left.
		"} while (s !== prev);",
		// An error status can carry an SSE stream; the real
		// error message is extracted from its data line.
		"var m = msg.match(/data:[ \\t]*(\\{[\\s\\S]*\\})/);",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
}

// TestChatProxyStreamLongLines pins the stream
// scanner's line cap: a single SSE line larger
// than a megabyte (the Responses API's
// response.completed event carries a whole
// output in one line, and providers embed
// multi-MB base64 parts in a single delta) is
// forwarded whole, and the final usage chunk
// after it is still captured for the stats
// event. A smaller cap would truncate the
// stream mid-line and lose the usage.
func TestChatProxyStreamLongLines(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", big)
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	g := newChatTestGateway(t)
	body := `{"endpoint":{"url":"` + upstream.URL + `"},"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	g.HandleWebChatCompletions(rec, chatTestRequest(t, http.MethodPost, "/chat/completions", body))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, big) {
		t.Errorf("the 2 MiB SSE line was not forwarded whole")
	}
	if !strings.Contains(out, "event: airouter.stats") {
		t.Fatalf("no stats event appended; stream tail: %.200s", out[len(out)-200:])
	}
	if !strings.Contains(out, "\"context_tokens\":7") {
		t.Errorf("usage was not captured from the chunk after the long line")
	}
}

// TestChatPageAudit4Fixes pins the audit #4
// client fixes: display math is not escaped a
// second time (the text is escaped before the
// math pass, so escaping again would render
// the entities literally), the response body
// reader is released when a stream fails (an
// error event throws out of handleSSE), and a
// chat id repaired at load is persisted.
func TestChatPageAudit4Fixes(t *testing.T) {
	for _, want := range []string{
		// Display math: the already-escaped text
		// is inserted as-is.
		"' + tex.replace(/\\s+/g, ' ').trim() + '</span>'",
		// The reader is cancelled when the stream
		// fails.
		"reader.cancel();",
		// A repaired chat id is persisted.
		"if (chatIdsRepaired) save(LS_CHATS, chats);",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
	// The display-math pass must not escape a
	// second time.
	if strings.Contains(chatHTML, "esc(tex.replace") {
		t.Errorf("display math is escaped twice: entities would render literally")
	}
}

// TestChatPageBenchmarkUI pins the in-chat
// benchmark: it measures the selected model at
// the four context depths through the same
// chat proxy (so the gateway's own
// measurements are the numbers shown), caps
// the completion so decode speed is
// comparable across depths, runs the depths
// sequentially under their own abort
// controller, and a failing depth shows its
// error and lets the run continue.
func TestChatPageBenchmarkUI(t *testing.T) {
	for _, want := range []string{
		`id="benchmark"`,
		"var BENCH_DEPTHS = [512, 4096, 16384, 65536];",
		"var BENCH_MAX_TOKENS = 256;",
		"function benchPrompt",
		"function runBenchmark",
		"function benchWindowLabel",
		"$('bench-window-val').textContent = benchWindowLabel();",
		// Concurrency is per depth: one request per depth is
		// launched in a loop, and the depth's result is written
		// only once every request in the batch has settled.
		"concurrent requests per depth",
		"function renderBatch()",
		"if (settled < benchConcurrency) return;",
		"renderBatch();",
		"payload.max_tokens = BENCH_MAX_TOKENS;",
		"payload.max_output_tokens = BENCH_MAX_TOKENS;",
		"benchAbortCtrl = new AbortController();",
		"signal: signal || (abortCtrl ? abortCtrl.signal : null)",
		"className = 'bench-table';",
		"$('benchmark').onclick = runBenchmark;",
		"if (generating || benchRunning) return;",
		"while (row.cells.length > 1) row.deleteCell(1);",
		"bad.colSpan = 4;",
		// Concurrency slider (1..32).
		`id="bench-concurrency-input"`,
		`min="1"`,
		`max="32"`,
		`id="bench-concurrency-val"`,
		// Context window slider (4 positions).
		`id="bench-window-input"`,
		`min="1"`,
		`max="4"`,
		`id="bench-window-val"`,
		"var BENCH_WINDOWS = [",
		"var benchWindow = 0;",
		// A dedicated stop button that can abort a run at any time.
		`id="bench-stop"`,
		"function stopBenchmark",
		"$('bench-stop').onclick = stopBenchmark;",
	} {
		if !strings.Contains(chatHTML, want) {
			t.Errorf("chatHTML missing %q", want)
		}
	}
	// The benchmark must measure both directions:
	// a depth row records the real context size
	// plus ttft, prefill and decode.
	if strings.Contains(chatHTML, "function runBenchmark") && !strings.Contains(chatHTML, "s.prefill_tokens_per_sec") {
		t.Errorf("benchmark rows do not record prefill speed")
	}
	if strings.Contains(chatHTML, "function runBenchmark") && !strings.Contains(chatHTML, "s.decode_tokens_per_sec") {
		t.Errorf("benchmark rows do not record decode speed")
	}
}
