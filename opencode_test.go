package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpencodeGateBody verifies the free-tier body gate rewrite:
// stream is forced on and the `bash` and `read` tools are injected
// when absent, while a body that already satisfies the gate is left
// alone and an unparseable body passes through unchanged.
func TestOpencodeGateBody(t *testing.T) {
	// A bare chat completion -- the shape a non-agent client sends.
	bare := `{"model":"nemotron-3-ultra-free","messages":[{"role":"user","content":"hi"}]}`
	out := opencodeGateBody([]byte(bare))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("gate output is not JSON: %v", err)
	}
	if string(got["stream"]) != "true" {
		t.Errorf("stream = %s, want true", got["stream"])
	}
	names := opencodeGateToolNames(t, got["tools"])
	if !names["bash"] || !names["read"] {
		t.Errorf("gate tools = %v, want bash and read present", names)
	}

	// A client that already streams with both tools keeps its own
	// tools (the gate must not duplicate them).
	satisfied := `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"bash"}},{"type":"function","function":{"name":"read"}},{"type":"function","function":{"name":"edit"}}]}`
	out = opencodeGateBody([]byte(satisfied))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("gate output is not JSON: %v", err)
	}
	names = opencodeGateToolNames(t, got["tools"])
	if len(names) != 3 || !names["bash"] || !names["read"] || !names["edit"] {
		t.Errorf("existing tools were disturbed: %v", names)
	}

	// A client that set stream=false explicitly is overridden: the
	// upstream answers 403 FreeTierError otherwise.
	noStream := `{"model":"m","stream":false,"tools":[{"type":"function","function":{"name":"bash"}},{"type":"function","function":{"name":"read"}}]}`
	out = opencodeGateBody([]byte(noStream))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("gate output is not JSON: %v", err)
	}
	if string(got["stream"]) != "true" {
		t.Errorf("stream = %s, want true (client's explicit false is overridden)", got["stream"])
	}

	// Responses-style flat tool names are recognized, so a flat
	// `bash` is not duplicated by a nested one.
	flat := `{"model":"m","tools":[{"type":"function","name":"bash"}]}`
	out = opencodeGateBody([]byte(flat))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("gate output is not JSON: %v", err)
	}
	names = opencodeGateToolNames(t, got["tools"])
	if count := strings.Count(string(got["tools"]), `"bash"`); count != 1 {
		t.Errorf("flat bash tool was duplicated: %s", got["tools"])
	}
	if !names["read"] {
		t.Errorf("read tool missing: %v", names)
	}

	// Fail-open: an unparseable body is forwarded verbatim.
	if out := opencodeGateBody([]byte("not json")); string(out) != "not json" {
		t.Errorf("unparseable body was rewritten: %s", out)
	}
}

func opencodeGateToolNames(t *testing.T, raw json.RawMessage) map[string]bool {
	t.Helper()
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("tools is not an array: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[opencodeGateToolName(tool)] = true
	}
	return names
}

// TestOpencodeAggregateStream verifies the SSE folding the
// non-streaming path applies to an opencode endpoint's answer:
// content deltas concatenate, reasoning deltas carry over, tool-call
// deltas merge per index, the finish reason and usage carry over,
// and a truncated stream (no [DONE] sentinel) is reported as an
// error rather than as a silently wrong completion.
func TestOpencodeAggregateStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"nemotron-3-ultra-free","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		``,
		`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"nemotron-3-ultra-free","choices":[{"index":0,"delta":{"reasoning_content":"pondering"},"finish_reason":null}]}`,
		``,
		`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"nemotron-3-ultra-free","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		``,
		`: keep-alive`,
		``,
		`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"nemotron-3-ultra-free","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"nemotron-3-ultra-free","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"nemotron-3-ultra-free","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := opencodeAggregateStream([]byte(stream), "nemotron-3-ultra-free", 12345)
	if err != nil {
		t.Fatalf("opencodeAggregateStream: %v", err)
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("aggregated response is not JSON: %v", err)
	}
	if resp.Object != "chat.completion" || resp.Model != "nemotron-3-ultra-free" || resp.Created != 12345 {
		t.Errorf("envelope mismatch: %+v", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.Content; got != "Hello" {
		t.Errorf("content = %q, want %q", got, "Hello")
	}
	if got := resp.Choices[0].Message.ReasoningContent; got != "pondering" {
		t.Errorf("reasoning_content = %q, want %q", got, "pondering")
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", resp.Choices[0].FinishReason)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(tcs))
	}
	if tcs[0].ID != "call_1" || tcs[0].Type != "function" || tcs[0].Function.Name != "bash" {
		t.Errorf("tool call identity mismatch: %+v", tcs[0])
	}
	if tcs[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("tool call arguments = %q, want the concatenated deltas", tcs[0].Function.Arguments)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want the streamed usage", resp.Usage)
	}

	// A stream with no [DONE] sentinel is truncated.
	truncated := `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`
	if _, err := opencodeAggregateStream([]byte(truncated), "m", 1); err == nil {
		t.Error("truncated stream (no [DONE]) must be an error")
	}

	// A stream that ends in [DONE] but produced no content is the
	// empty-completion artifact seen live on 2026-09-30: it folds
	// into a 200 completion with empty content, reasoning_content
	// and finish_reason carried over, which the client's "200 AND
	// usable" retry predicate detects and retries. Only a stream
	// with no [DONE] sentinel is a failure.
	empty := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"thinking"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	emptyOut, err := opencodeAggregateStream([]byte(empty), "m", 1)
	if err != nil {
		t.Fatalf("completed empty stream must fold, not fail: %v", err)
	}
	var emptyResp ChatCompletionResponse
	if err := json.Unmarshal(emptyOut, &emptyResp); err != nil {
		t.Fatalf("empty completion is not JSON: %v", err)
	}
	if got := emptyResp.Choices[0].Message.Content; got != "" {
		t.Errorf("empty completion content = %q, want empty", got)
	}
	if got := emptyResp.Choices[0].Message.ReasoningContent; got != "thinking" {
		t.Errorf("empty completion reasoning_content = %q, want thinking", got)
	}
	if emptyResp.Choices[0].FinishReason == nil || *emptyResp.Choices[0].FinishReason != "length" {
		t.Errorf("empty completion finish_reason = %v, want length", emptyResp.Choices[0].FinishReason)
	}
}

// TestOpencodeGateOnWire proves that a bare chat completion -- a
// request with no `stream` field and no `tools` array, the shape a
// non-agent client sends -- leaves airouter toward an opencode
// gateway in the free-tier gate's shape: stream true and a tools
// array holding both `bash` and `read`. Without the rewrite the
// gateway answers 403 FreeTierError (measured live 2026-10-11).
func TestOpencodeGateOnWire(t *testing.T) {
	var gotBody map[string]json.RawMessage
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := r.ContentLength
		buf := make([]byte, n)
		if n > 0 {
			if _, err := ioReadFull(r, buf); err != nil {
				t.Errorf("read body: %v", err)
			}
		}
		if err := json.Unmarshal(buf, &gotBody); err != nil {
			t.Errorf("upstream body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID: "test", Object: "chat.completion", Created: 1, Model: "m",
			Choices: []ChatCompletionChoice{
				{Index: 0, Message: ChatCompletionMessage{Role: "assistant", Content: "OK"}},
			},
		})
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"opencode": {URL: backend.URL, APIKeyEnv: "OPENCODE_API_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "opencode", Model: "nemotron-3-ultra-free"}}},
		},
	}
	t.Setenv("OPENCODE_API_KEY", "sk-test")

	proxy := NewProxy(cfg)
	router := NewRouter(cfg, "")
	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	ep, _ := router.SelectEndpoint("smart", "test-session", false, nil)
	resp, err := proxy.Forward(context.Background(), []byte(body), *ep, "test-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if string(gotBody["stream"]) != "true" {
		t.Errorf("wire stream = %s, want true (the free-tier gate requires it)", gotBody["stream"])
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(gotBody["tools"], &tools); err != nil {
		t.Fatalf("wire tools is not an array: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[opencodeGateToolName(tool)] = true
	}
	if !names["bash"] || !names["read"] {
		t.Errorf("wire tools = %v, want bash and read (the free-tier gate requires both)", names)
	}
}

func ioReadFull(r *http.Request, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Body.Read(buf[total:])
		total += n
		if err != nil {
			if err.Error() == "EOF" && total == len(buf) {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}

// TestOpencodeHeaderShape verifies the synthesized attribution
// headers match the client the gateway recognizes: the official
// `opencode/<version>` user agent (the exact string the shipped
// CLI sends), and the session id carried under every spelling the
// routing backend reads.
func TestOpencodeHeaderShape(t *testing.T) {
	hdrs := opencodeRequestHeaders()
	ua := hdrs["User-Agent"]
	if ua != "opencode/1.19.0" {
		t.Errorf("User-Agent = %q, want the official opencode/1.19.0", ua)
	}
	// The session id is the canonical `ses_<12 hex><14 base62>`
	// shape the gateway measures a 403 for any other form of.
	sess := hdrs["x-opencode-session"]
	if !strings.HasPrefix(sess, "ses_") || len(sess) != 4+12+14 {
		t.Errorf("x-opencode-session = %q, want the canonical ses_ shape", sess)
	}
	for _, name := range []string{"x-opencode-session-id", "x-session-affinity", "X-Session-ID"} {
		if hdrs[name] != sess {
			t.Errorf("%s = %q, want the same session id as x-opencode-session (%q)", name, hdrs[name], sess)
		}
	}
	if hdrs["x-opencode-client"] != "cli" {
		t.Errorf("x-opencode-client = %q, want cli", hdrs["x-opencode-client"])
	}
	if !strings.HasPrefix(hdrs["x-opencode-request"], "msg_") {
		t.Errorf("x-opencode-request = %q, want the msg_ shape", hdrs["x-opencode-request"])
	}
	if !bytes.HasPrefix([]byte(sess), []byte("ses_")) {
		t.Error("unreachable")
	}
}
