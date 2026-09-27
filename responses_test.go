package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testGatewayKey is the gateway key the Responses tests authenticate with, so
// the tests exercise the same auth path as production rather than opting out.
const testGatewayKey = "gw-test"

// responsesGateway wires a gateway in front of a test backend, exactly as the
// existing chat tests do, so the Responses surface is exercised end to end
// through real routing and proxying rather than in isolation.
func responsesGateway(t *testing.T, handler http.HandlerFunc, chain ...ModelEndpoint) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"openai": {URL: backend.URL, APIKeyEnv: "TEST_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: chain},
		},
	}
	t.Setenv("TEST_KEY", "sk-test")

	proxy := NewProxy(cfg)
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	g := NewGatewayContext(router, proxy, cfg, "", testGatewayKey, false)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", g.HandleResponses)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postResponses(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// TestResponsesNonStreamingStringInput covers the simplest client shape: input
// as a bare string, non-streaming. Verifies the whole path -- translation to
// chat messages upstream, and shaping back into a Responses envelope.
func TestResponsesNonStreamingStringInput(t *testing.T) {
	var gotBody map[string]any
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "chatcmpl-1", Object: "chat.completion", Created: 42, Model: "gpt-4",
			Choices: []ChatCompletionChoice{
				{Index: 0, Message: ChatCompletionMessage{Role: "assistant", Content: "hello there"}},
			},
			Usage: &ChatCompletionUsage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"smart","input":"hi there"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The upstream must have received chat completions, with a user message
	// carrying the input string.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("upstream messages = %v, want 1 message", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hi there" {
		t.Errorf("upstream message = %v, want user/hi there", first)
	}

	var env responseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Object != "response" || env.Status != "completed" {
		t.Errorf("envelope object/status = %s/%s, want response/completed", env.Object, env.Status)
	}
	if !strings.HasPrefix(env.ID, "resp_") {
		t.Errorf("envelope id = %q, want resp_ prefix", env.ID)
	}
	if len(env.Output) != 1 || env.Output[0].Type != "message" {
		t.Fatalf("output = %+v, want one message item", env.Output)
	}
	if got := env.Output[0].Content[0].Text; got != "hello there" {
		t.Errorf("output text = %q, want %q", got, "hello there")
	}
	if env.Usage == nil || env.Usage.InputTokens != 5 || env.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want 5/3", env.Usage)
	}
}

// TestResponsesInstructionsBecomeSystemMessage verifies instructions are mapped
// to a leading system message rather than dropped.
func TestResponsesInstructionsBecomeSystemMessage(t *testing.T) {
	var gotBody map[string]any
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c1", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{Message: ChatCompletionMessage{Role: "assistant", Content: "ok"}}},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"smart","instructions":"be terse","input":"hi"}`)
	defer resp.Body.Close()

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 (system + user)", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be terse" {
		t.Errorf("first message = %v, want system/be terse", first)
	}
}

// TestResponsesItemInputFlattens verifies typed input items flatten into
// alternating user/assistant turns, and that a reasoning item is dropped
// (it is not representable upstream).
func TestResponsesItemInputFlattens(t *testing.T) {
	var gotBody map[string]any
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c1", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{Message: ChatCompletionMessage{Role: "assistant", Content: "ok"}}},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"smart","input":[
		{"type":"reasoning","summary":"thinking"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q1"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}
	]}`)
	defer resp.Body.Close()

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (reasoning dropped)", len(msgs))
	}
	want := []struct{ role, content string }{
		{"user", "q1"}, {"assistant", "a1"}, {"user", "q2"},
	}
	for i, w := range want {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != w.role || m["content"] != w.content {
			t.Errorf("msg[%d] = %v, want %s/%s", i, m, w.role, w.content)
		}
	}
}

// TestResponsesToolCallsBecomeFunctionCallItems verifies a tool-calling turn
// is shaped into Responses function_call items, which are separate output
// items rather than nested in the message.
func TestResponsesToolCallsBecomeFunctionCallItems(t *testing.T) {
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		tc := ChatCompletionToolCall{ID: "call_1", Type: "function"}
		tc.Function.Name = "get_weather"
		tc.Function.Arguments = `{"city":"Oslo"}`
		finish := "tool_calls"
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c1", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{
				Message:      ChatCompletionMessage{Role: "assistant", ToolCalls: []ChatCompletionToolCall{tc}},
				FinishReason: &finish,
			}},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"smart","input":"weather?"}`)
	defer resp.Body.Close()

	var env responseEnvelope
	json.NewDecoder(resp.Body).Decode(&env)
	if len(env.Output) != 1 || env.Output[0].Type != "function_call" {
		t.Fatalf("output = %+v, want one function_call", env.Output)
	}
	fc := env.Output[0]
	if fc.Name != "get_weather" || fc.CallID != "call_1" {
		t.Errorf("tool call = %+v, want get_weather/call_1", fc)
	}
	if fc.Arguments != `{"city":"Oslo"}` {
		t.Errorf("arguments = %q", fc.Arguments)
	}
}

// TestResponsesFinishLengthIsIncomplete verifies a truncated upstream answer is
// reported as status "incomplete" rather than a clean "completed", which would
// silently hide a truncated answer from the client.
func TestResponsesFinishLengthIsIncomplete(t *testing.T) {
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		finish := "length"
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c1", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{
				Message:      ChatCompletionMessage{Role: "assistant", Content: "trunc"},
				FinishReason: &finish,
			}},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"smart","input":"hi"}`)
	defer resp.Body.Close()

	var env responseEnvelope
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Status != "incomplete" {
		t.Errorf("status = %q, want incomplete", env.Status)
	}
	// The partial text must still be delivered.
	if len(env.Output) != 1 || env.Output[0].Content[0].Text != "trunc" {
		t.Errorf("output = %+v, want the partial text preserved", env.Output)
	}
}

// TestResponsesUnknownModelRejected verifies routing validation is enforced on
// this surface too.
func TestResponsesUnknownModelRejected(t *testing.T) {
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend must not be called for an unknown model")
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"nope","input":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestResponsesFallsBackOn500 verifies the fallback chain works on this
// surface: a failing head model must not surface an error to the client while
// a healthy model sits behind it.
func TestResponsesFallsBackOn500(t *testing.T) {
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c2", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{Message: ChatCompletionMessage{Role: "assistant", Content: "recovered"}}},
		})
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"a": {URL: backend.URL, APIKeyEnv: "K"},
			"b": {URL: backend.URL, APIKeyEnv: "K"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "a", Model: "m1"},
				{Provider: "b", Model: "m2"},
			}},
		},
	}
	t.Setenv("K", "sk-test")
	router := NewRouter(cfg, "")
	defer router.Close()
	g := NewGatewayContext(router, NewProxy(cfg), cfg, "", testGatewayKey, false)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", g.HandleResponses)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"smart","input":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (fallback should have recovered)", resp.StatusCode)
	}
	var env responseEnvelope
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Output[0].Content[0].Text != "recovered" {
		t.Errorf("text = %q, want recovered", env.Output[0].Content[0].Text)
	}
	if calls < 2 {
		t.Errorf("backend calls = %d, want >= 2 (first must fail over)", calls)
	}
}

// TestResponsesStreamLifecycle verifies the streaming surface emits the
// Responses event sequence in the right order, with a delta carrying the text
// and a terminal response.completed.
func TestResponsesStreamLifecycle(t *testing.T) {
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, tok := range []string{"Hel", "lo"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(`{"model":"smart","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body := readAllString(t, resp)
	events := sseEventNames(body)
	// The first token is buffered until the attempt is known to be good, so
	// the "Hel"+"lo" prefix is coalesced into a single output_text.delta
	// rather than two. That is deliberate: it is what lets a pre-token
	// failure fail over with nothing written to the client.
	want := []string{
		evResponseCreated, evResponseInProgress, evOutputItemAdded,
		evContentPartAdded, evOutputTextDelta,
		evOutputTextDone, evContentPartDone, evOutputItemDone, evResponseCompleted,
	}
	assertEventSequence(t, events, want)

	// The deltas must reassemble into the full text.
	if !strings.Contains(body, "Hello") {
		t.Errorf("stream body missing reassembled text; got:\n%s", body)
	}
}

// TestResponsesStreamToolCallEmitsFunctionCallItem verifies a streamed tool
// call is delivered as a completed function_call item, not as partial
// argument deltas the client would have to reassemble.
func TestResponsesStreamToolCallEmitsFunctionCallItem(t *testing.T) {
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Split the arguments across two deltas, as providers do.
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(`{"model":"smart","input":"q","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	body := readAllString(t, resp)
	if !strings.Contains(body, evFunctionCallArgsDone) {
		t.Errorf("missing function_call_arguments.done; got:\n%s", body)
	}
	// The two argument deltas must be concatenated into one complete call.
	if !strings.Contains(body, `{\"q\":\"go\"}`) {
		t.Errorf("tool call arguments were not accumulated; got:\n%s", body)
	}
	if !strings.Contains(body, evResponseCompleted) {
		t.Errorf("missing terminal response.completed; got:\n%s", body)
	}
}

// TestResponsesNoContentOnEarlyStreamError verifies that an upstream failure
// before the first token produces no client output, so the turn can fall back
// to the next model without half-writing a response.
func TestResponsesNoContentOnEarlyStreamError(t *testing.T) {
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// 200 + SSE content type, but an immediate error event and no text.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"boom\"}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"a": {URL: backend.URL, APIKeyEnv: "K"},
			"b": {URL: backend.URL, APIKeyEnv: "K"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "a", Model: "m1"},
				{Provider: "b", Model: "m2"},
			}},
		},
	}
	t.Setenv("K", "sk-test")
	router := NewRouter(cfg, "")
	defer router.Close()
	g := NewGatewayContext(router, NewProxy(cfg), cfg, "", testGatewayKey, false)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", g.HandleResponses)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(`{"model":"smart","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	body := readAllString(t, resp)
	if !strings.Contains(body, "second") {
		t.Errorf("expected failover content; got:\n%s", body)
	}
	// Only the successful attempt's events should be present: exactly one
	// response.created, from the retry.
	if n := strings.Count(body, "event: "+evResponseCreated); n != 1 {
		t.Errorf("response.created count = %d, want 1 (failed attempt must emit nothing); got:\n%s", n, body)
	}
}

// TestResponsesVisionInputTranslated verifies a Responses input_image part is
// translated into a chat image_url part, so capability-aware routing sees the
// request as vision and does not pick a text-only model.
func TestResponsesVisionInputTranslated(t *testing.T) {
	var gotBody map[string]any
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c1", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{Message: ChatCompletionMessage{Role: "assistant", Content: "an image"}}},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4o"})

	resp := postResponses(t, srv, `{"model":"smart","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"what is this"},
			{"type":"input_image","image_url":"https://example.com/a.png"}
		]}
	]}`)
	defer resp.Body.Close()

	if !requestHasVision(mustJSON(t, gotBody)) {
		t.Errorf("translated body is not detected as vision; got %v", gotBody)
	}
}

// TestResponsesAcceptsIgnoredFields verifies a spec-complete client request
// carrying Responses-only fields is accepted rather than rejected, since those
// fields are not representable upstream.
func TestResponsesAcceptsIgnoredFields(t *testing.T) {
	srv := responsesGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "c1", Object: "chat.completion", Created: 1,
			Choices: []ChatCompletionChoice{{Message: ChatCompletionMessage{Role: "assistant", Content: "ok"}}},
		})
	}, ModelEndpoint{Provider: "openai", Model: "gpt-4"})

	resp := postResponses(t, srv, `{"model":"smart","input":"hi","store":true,"previous_response_id":"resp_prev",
		"metadata":{"k":"v"},"reasoning":{"effort":"high"},"text":{"format":{"type":"json_object"}}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (Responses-only fields should be ignored, not rejected)", resp.StatusCode)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// readAllString drains and closes a response body.
func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// sseEventNames extracts the ordered list of `event:` names from an SSE blob.
func sseEventNames(sse string) []string {
	var names []string
	for _, line := range strings.Split(sse, "\n") {
		if strings.HasPrefix(line, "event:") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	return names
}

// assertEventSequence checks the emitted event names contain the expected
// sequence in order, allowing extra events in between (sequence_number-bearing
// bookkeeping) but not missing or reordered ones.
func assertEventSequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("event sequence = %v, want it to contain %v in order (matched %d/%d)", got, want, i, len(want))
	}
}
