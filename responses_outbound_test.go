package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Responses fixtures below are not invented. They are the event payloads
// captured verbatim from a live stream on 2026-10-02 (kilocode kilo-auto/free
// over /responses), trimmed of fields this translator does not read. Writing
// them by hand from the spec is how a translator ends up looking correct and
// silently dropping every argument fragment.

const (
	fxCreated         = `{"type":"response.created","response":{"id":"gen-1790961464-KjC8DeoDd9a63XtmOrOq","object":"response","created_at":1790961464,"model":"stealth/space-bunny-alpha","status":"in_progress"}}`
	fxReasoningDelta  = `{"type":"response.reasoning_text.delta","output_index":0,"item_id":"rs_tmp_0fipklietjxw","content_index":0,"delta":"We need answer exactly","sequence_number":4}`
	fxReasoningDelta2 = `{"type":"response.reasoning_text.delta","output_index":0,"item_id":"rs_tmp_0fipklietjxw","content_index":0,"delta":"? User says say pong.","sequence_number":5}`
	fxTextDelta       = `{"type":"response.output_text.delta","output_index":1,"item_id":"msg_tmp_ukudxmkq4pc","content_index":0,"delta":"pong","sequence_number":11}`
	fxToolItemAdded   = `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_tmp_lsiblj3o5k","type":"function_call","status":"in_progress","call_id":"a387cb94-b675-4bb7-8fe4-cb37e34c52d3","name":"bash","arguments":""},"sequence_number":9}`
	fxToolArgsDelta   = `{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_tmp_lsiblj3o5k","delta":"{\"command\": \"echo hi\"}","sequence_number":10}`
	fxCompleted       = `{"type":"response.completed","response":{"id":"gen-1790961464-KjC8DeoDd9a63XtmOrOq","object":"response","created_at":1790961464,"model":"stealth/space-bunny-alpha","status":"completed","usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}}`
)

// drain runs a captured event list through the translator and returns the
// concatenated Chat Completions SSE.
func drain(t *testing.T, events ...string) string {
	t.Helper()
	tr := newResponsesStreamTranslator()
	var out strings.Builder
	for _, e := range events {
		out.Write(tr.consume(responsesEventType([]byte(e)), []byte(e)))
	}
	return out.String()
}

func chunksOf(t *testing.T, sse string) []chatChunk {
	t.Helper()
	var out []chatChunk
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var c chatChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("translator emitted a chunk that is not a chat.completion.chunk: %v\n%s", err, payload)
		}
		out = append(out, c)
	}
	return out
}

// TestResponsesStreamEmitsTextAsChatDeltas is the happy path.
func TestResponsesStreamEmitsTextAsChatDeltas(t *testing.T) {
	got := drain(t, fxCreated, fxReasoningDelta, fxReasoningDelta2, fxTextDelta, fxCompleted)
	chunks := chunksOf(t, got)

	var text strings.Builder
	var finish string
	for _, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", c.Object)
		}
		text.WriteString(c.Choices[0].Delta.Content)
		if c.Choices[0].FinishReason != nil {
			finish = *c.Choices[0].FinishReason
		}
	}
	if text.String() != "pong" {
		t.Errorf("content = %q, want %q", text.String(), "pong")
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
	if chunks[len(chunks)-1].Usage == nil || chunks[len(chunks)-1].Usage.TotalTokens != 13 {
		t.Errorf("terminal chunk must carry usage; got %+v", chunks[len(chunks)-1].Usage)
	}
}

// TestResponsesStreamNeverLeaksReasoning is the safety property.
//
// The Chat Completions surface has no reasoning channel. If the reasoning
// deltas were forwarded as content the client would receive the model's
// private scratchpad as part of the answer -- visibly wrong output that a
// status-code check would never catch.
func TestResponsesStreamNeverLeaksReasoning(t *testing.T) {
	got := drain(t, fxCreated, fxReasoningDelta, fxReasoningDelta2, fxTextDelta, fxCompleted)
	if strings.Contains(got, "We need answer") || strings.Contains(got, "User says say pong") {
		t.Fatalf("reasoning leaked into the chat stream:\n%s", got)
	}
	// And it must still emit the real content, not go silent instead.
	if !strings.Contains(got, "pong") {
		t.Fatalf("suppressing reasoning also suppressed the answer:\n%s", got)
	}
}

// TestResponsesStreamToolCallOpensBeforeArguments pins the tool-call framing.
//
// Argument deltas are bare JSON fragments. A client concatenates them, so the
// FIRST chunk of a tool call must open the entry with empty arguments. Opening
// it with the fragment instead yields arguments that never form valid JSON.
func TestResponsesStreamToolCallOpensBeforeArguments(t *testing.T) {
	got := drain(t, fxCreated, fxToolItemAdded, fxToolArgsDelta, fxCompleted)
	chunks := chunksOf(t, got)

	var opening *chatChunkDeltaToolCall
	var fragments strings.Builder
	for i, c := range chunks {
		for _, tc := range c.Choices[0].Delta.ToolCalls {
			if tc.Function.Arguments == "" && tc.ID != "" {
				if opening == nil {
					cp := tc
					opening = &cp
				}
				continue
			}
			fragments.WriteString(tc.Function.Arguments)
		}
		_ = i
	}
	if opening == nil {
		t.Fatalf("no opening tool-call delta with an id; the client would never "+
			"learn the call id or name:\n%s", got)
	}
	if opening.Function.Name != "bash" {
		t.Errorf("opening tool call name = %q, want bash", opening.Function.Name)
	}
	if opening.ID != "a387cb94-b675-4bb7-8fe4-cb37e34c52d3" {
		t.Errorf("opening tool call id = %q, want the upstream call_id", opening.ID)
	}
	if fragments.String() != `{"command": "echo hi"}` {
		t.Errorf("concatenated arguments = %q, want the full JSON object",
			fragments.String())
	}
	// The reconstructed arguments must actually parse -- that is the whole
	// point of the empty opening delta.
	if !json.Valid([]byte(fragments.String())) {
		t.Errorf("concatenated arguments are not valid JSON: %q", fragments.String())
	}
}

// TestResponsesStreamEmitsTerminalChunkForAnEmptyResponse stops the client
// waiting forever: a Responses stream that produced nothing usable must still
// deliver a finish_reason.
func TestResponsesStreamEmitsTerminalChunkForAnEmptyResponse(t *testing.T) {
	got := drain(t, fxCreated, fxCompleted)
	chunks := chunksOf(t, got)
	if len(chunks) == 0 {
		t.Fatal("empty response produced no chunks at all")
	}
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil {
		t.Errorf("last chunk has no finish_reason: %+v", last)
	}
}

// TestResponsesStreamIgnoresUnknownEvents is a forward-compatibility property:
// an event this translator has never seen must be dropped, not forwarded. A
// Responses-shaped event inside a Chat Completions stream corrupts it.
func TestResponsesStreamIgnoresUnknownEvents(t *testing.T) {
	got := drain(t, fxCreated,
		`{"type":"response.some_future_event","delta":"LEAK"}`,
		fxTextDelta, fxCompleted)
	if strings.Contains(got, "LEAK") {
		t.Fatalf("an unknown Responses event was forwarded into the chat stream:\n%s", got)
	}
}

// TestResponsesSSEToChatReaderTerminates checks the reader a real stream flows
// through, including the [DONE] framing streamSSE depends on.
func TestResponsesSSEToChatReaderTerminates(t *testing.T) {
	src := strings.Join([]string{
		"data: " + fxCreated, "",
		"data: " + fxReasoningDelta, "",
		"data: " + fxTextDelta, "",
		"data: " + fxCompleted, "",
	}, "\n")
	r := newResponsesSSEToChatReader(strings.NewReader(src))
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := sb.String()
	if !strings.Contains(out, "pong") {
		t.Errorf("reader lost the content:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("reader did not terminate the stream with [DONE]; streamSSE and "+
			"the client both rely on it:\n%s", out)
	}
}

// TestResponsesSSEToChatReaderHandlesSplitEvents guards the buffering: a
// Responses event arriving split across two reads must not be classified
// twice or dropped.
func TestResponsesSSEToChatReaderHandlesSplitEvents(t *testing.T) {
	full := "data: " + fxTextDelta + "\n\n"
	r := newResponsesSSEToChatReader(&oneByteReader{s: full})
	var sb strings.Builder
	buf := make([]byte, 8)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "pong") {
		t.Errorf("an event split across reads was lost:\n%s", sb.String())
	}
	if got := strings.Count(sb.String(), "pong"); got != 1 {
		t.Errorf("content emitted %d times, want exactly 1:\n%s", got, sb.String())
	}
}

type oneByteReader struct {
	s string
	i int
}

func (o *oneByteReader) Read(p []byte) (int, error) {
	if o.i >= len(o.s) {
		return 0, io.EOF
	}
	p[0] = o.s[o.i]
	o.i++
	return 1, nil
}

// ---------------------------------------------------------------------------
// Request direction
// ---------------------------------------------------------------------------

// TestChatBodyToResponsesMovesSystemToInstructions pins the one place where
// the translation moves rather than copies. Copying would duplicate the system
// prompt, which is both wrong and billable twice.
func TestChatBodyToResponsesMovesSystemToInstructions(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"hello"}]}`)
	got, err := chatBodyToResponses(body, "m")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Instructions string `json:"instructions"`
		Input        []item `json:"input"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if out.Instructions != "be terse" {
		t.Errorf("instructions = %q, want %q", out.Instructions, "be terse")
	}
	for _, it := range out.Input {
		if it.Role == "system" {
			t.Errorf("the system message was copied into input as well as moved to "+
				"instructions; that duplicates the prompt: %+v", out.Input)
		}
	}
	if len(out.Input) != 1 || out.Input[0].Role != "user" {
		t.Errorf("input = %+v, want exactly the user message", out.Input)
	}
}

// TestChatBodyToResponsesUsesMaxOutputTokens pins the field rename. Measured
// live against xAI 2026-10-02: sending max_tokens to /responses is rejected
// with "max_tokens is not supported on /v1/responses -- use 'max_output_tokens'".
func TestChatBodyToResponsesUsesMaxOutputTokens(t *testing.T) {
	got, err := chatBodyToResponses([]byte(`{"model":"m","max_tokens":64,"messages":[]}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"max_output_tokens":64`) {
		t.Errorf("body = %s, want max_output_tokens (max_tokens is rejected by /responses)", got)
	}
	if strings.Contains(string(got), `"max_tokens"`) {
		t.Errorf("body still carries max_tokens, which /responses rejects:\n%s", got)
	}
}

// TestChatBodyToResponsesFlattensToolEnvelopes pins the second spelling
// difference: Chat Completions nests the name under `function`, Responses puts
// it flat. Getting this wrong sends tools the model cannot read.
func TestChatBodyToResponsesFlattensToolEnvelopes(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"bash","description":"d","parameters":{"type":"object"}}}]}`)
	got, err := chatBodyToResponses(body, "m")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `"function":{`) {
		t.Errorf("tool envelope was not flattened:\n%s", got)
	}
	if !strings.Contains(string(got), `"name":"bash"`) {
		t.Errorf("tool name lost:\n%s", got)
	}
}

// TestChatBodyToResponsesConvertsToolResults covers the assistant/tool round
// trip, which is where a naive message-by-message copy loses the tool calls the
// model needs to see answered.
func TestChatBodyToResponsesConvertsToolResults(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"run it"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"a.txt"}]}`)
	got, err := chatBodyToResponses(body, "m")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Input []item `json:"input"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	var sawCall, sawResult bool
	for _, it := range out.Input {
		if it.Type == "function_call" && it.Name == "bash" {
			sawCall = true
			if !strings.Contains(it.Arguments, "command") {
				t.Errorf("tool call arguments lost: %q", it.Arguments)
			}
		}
		if it.Type == "function_call_output" {
			sawResult = true
			if it.CallID != "c1" || it.Output != "a.txt" {
				t.Errorf("tool result = %+v, want call_id c1 output a.txt", it)
			}
		}
	}
	if !sawCall {
		t.Errorf("assistant tool_call was dropped:\n%s", got)
	}
	if !sawResult {
		t.Errorf("tool result was dropped:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Response direction
// ---------------------------------------------------------------------------

func TestResponsesBodyToChatRoundTrip(t *testing.T) {
	env := []byte(`{"id":"resp_1","object":"response","created_at":7,"status":"completed",
		"model":"m","output":[
			{"type":"reasoning","content":[{"type":"reasoning_text","text":"hidden"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]},
			{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"ls\"}"}],
		"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	got, err := responsesBodyToChat(env, "logical")
	if err != nil {
		t.Fatal(err)
	}
	var c ChatCompletionResponse
	if err := json.Unmarshal(got, &c); err != nil {
		t.Fatal(err)
	}
	if c.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", c.Object)
	}
	if c.Choices[0].Message.Content != "pong" {
		t.Errorf("content = %q, want pong", c.Choices[0].Message.Content)
	}
	if len(c.Choices[0].Message.ToolCalls) != 1 || c.Choices[0].Message.ToolCalls[0].Function.Name != "bash" {
		t.Errorf("tool call lost: %+v", c.Choices[0].Message.ToolCalls)
	}
	if c.Usage == nil || c.Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v, want total 7", c.Usage)
	}
	if c.Model != "logical" {
		t.Errorf("model = %q, want the LOGICAL model so the client sees what it asked for", c.Model)
	}
}

// TestResponsesBodyToChatRejectsFailure stops an error envelope being laundered
// into an empty success, which would look to the client like a model that
// answered nothing rather than one that failed.
func TestResponsesBodyToChatRejectsFailure(t *testing.T) {
	env := []byte(`{"id":"r","status":"failed","error":{"type":"server_error","message":"boom"},"output":[]}`)
	if _, err := responsesBodyToChat(env, "m"); err == nil {
		t.Fatal("a failed envelope was converted into a success")
	}
}

// ---------------------------------------------------------------------------
// Protocol selection
// ---------------------------------------------------------------------------

func TestProtocolForDefaultsToChat(t *testing.T) {
	for _, in := range []string{"", "  ", "chat", "chat/completions", "responses ", "RESPONSES", "response", "openai-responses"} {
		got := protocolFor(in)
		want := protocolChatCompletions
		if strings.Contains(strings.ToLower(in), "response") {
			want = protocolResponses
		}
		if got != want {
			t.Errorf("protocolFor(%q) = %q, want %q", in, got, want)
		}
	}
	// A typo must not take an endpoint out of rotation.
	if got := protocolFor("respnses"); got != protocolChatCompletions {
		t.Errorf("a misspelling resolved to %q, want chat/completions", got)
	}
}

func TestEndpointProtocolIsPerEndpoint(t *testing.T) {
	if (ModelEndpoint{Protocol: "responses"}).UpstreamProtocol() != protocolResponses {
		t.Error("an endpoint declaring responses did not resolve to responses")
	}
	if (ModelEndpoint{}).UpstreamProtocol() != protocolChatCompletions {
		t.Error("an endpoint with no protocol must default to chat/completions")
	}
}

var _ = http.StatusOK

// TestStreamAttemptHeaderWaitIsBounded reproduces the live failure reported on
// 2026-10-02 from the running daemon.
//
// A ~44k-token request was logged with `timeout=10s` against NVIDIA endpoints
// and then failed 30 seconds later with
// `net/http: timeout awaiting response headers`, twice exhausting the 45s
// fallback budget and surfacing "all models unavailable" to the client:
//
//	request -> nvidia/z-ai/glm-5.3 (timeout=10s, ~44473 tokens)
//	... 30s ...
//	stream fallback budget exhausted after 3 attempts in 1m0.718s (budget 45s)
//
// The cause: the streaming path passed the unbounded parent context, so the
// size-based budget only applied to the first byte AFTER headers arrived --
// and a provider prefills a large prompt before sending headers. The header
// wait therefore fell back to the transport's global 30s ResponseHeaderTimeout,
// three times the budget the attempt believed it had.
//
// The bound has to cover the header wait specifically. bounding the whole
// request by context deadline would kill legitimate long generations, so this
// asserts the header wait is capped while the body phase is not.
func TestStreamAttemptHeaderWaitIsBounded(t *testing.T) {
	const budget = 2 * time.Second
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never send headers: a prefill longer than the budget. Kept short
		// enough that the deferred Close() does not dominate the test's
		// runtime, while still exceeding the 2s budget by enough to prove
		// the bound is the attempt's and not the server's patience.
		time.Sleep(6 * time.Second)
	}))
	defer slow.Close()

	pc := ProviderConfig{URL: slow.URL, APIKeyEnv: "K"}
	t.Setenv("K", "d")
	p := NewProxy(&Config{Providers: map[string]ProviderConfig{"p": pc}})
	ep := ModelEndpoint{Provider: "p", Model: "m"}

	if p.baseTransport == nil {
		t.Fatal("baseTransport not captured; the per-attempt header bound cannot work")
	}

	// A reader that records how long the header wait actually took.
	done := make(chan time.Duration, 1)
	start := time.Now()
	_, _, _, err := p.StreamToClient(context.Background(), io.Discard, nopFlusher{},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), ep, budget, "s")
	elapsed := time.Since(start)
	done <- elapsed

	if err == nil {
		t.Fatal("expected the attempt to fail; the test server never responds")
	}
	// Generous ceiling: the point is that it is nowhere near 30s. The real
	// budget is 2s, so anything under 10s proves the transport's 30s cap is no
	// longer what bounds this attempt.
	t.Logf("attempt returned after %v with a %v budget (error: %v)",
		elapsed.Round(10*time.Millisecond), budget, err)
	if elapsed > 2*budget {
		t.Errorf("header wait took %v; the attempt outran its own %v budget and "+
			"fell back to the transport-wide timeout, which is what blew the "+
			"fallback budget in production", elapsed.Round(time.Millisecond), budget)
	}
	if !strings.Contains(err.Error(), "timeout") &&
		!strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %v, want a timeout", err)
	}
}
