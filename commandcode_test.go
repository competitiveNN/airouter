package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const ccTestAPIKey = "user_test-key-12345"

func TestCCGenerateFingerprintDeterministic(t *testing.T) {
	fp1 := ccGenerateFingerprint(ccTestAPIKey)
	fp2 := ccGenerateFingerprint(ccTestAPIKey)
	if fp1.Thumbmark == "" {
		t.Fatal("thumbmark is empty")
	}
	if fp1.Thumbmark != fp2.Thumbmark {
		t.Fatalf("fingerprint not deterministic: %s != %s", fp1.Thumbmark, fp2.Thumbmark)
	}
	c := fp1.Components
	if c.Platform != "win32" || c.Arch != "x64" || c.OSRelease != "10.0.22631" {
		t.Fatalf("device profile mismatch: %+v", c)
	}
	if c.CPUCount == 0 || c.MemGiB == 0 || c.Timezone == "" {
		t.Fatalf("missing hardware facts: %+v", c)
	}
	if c.Runtime != "cli" || c.CollectorVersion != 1 {
		t.Fatalf("runtime/collector mismatch: %+v", c)
	}
	// cpuCount is the LOGICAL processor count (threads), not cores;
	// Cores is not exposed, so assert cpuCount is one of the
	// pool's thread counts unconditionally.
	valid := false
	for _, cpu := range ccFingerprintCPUs {
		if cpu.Model == c.CPUModel && cpu.Threads == c.CPUCount {
			valid = true
		}
	}
	if !valid {
		t.Fatalf("cpuModel/cpuCount not a pool pair: %s/%d", c.CPUModel, c.CPUCount)
	}
	// A different key derives a different device.
	if ccGenerateFingerprint("user_other-key").Thumbmark == fp1.Thumbmark {
		t.Fatal("two keys derived the same device")
	}
}

func TestCCBuildEnvelopeShape(t *testing.T) {
	clientBody := []byte(`{
		"model":"smart",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"hi"}
		],
		"max_tokens":100,
		"temperature":0.7,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":"auto"
	}`)
	envelope, sessionID, err := ccBuildEnvelope(clientBody, "deepseek/deepseek-v4-flash", ccTestAPIKey, nil)
	if err != nil {
		t.Fatalf("ccBuildEnvelope: %v", err)
	}
	if !ccIsWireUUID(sessionID) {
		t.Fatalf("session id is not a wire UUID: %q", sessionID)
	}

	// The envelope's top-level key order is load-bearing.
	var order []string
	dec := json.NewDecoder(bytes.NewReader(envelope))
	if _, err := dec.Token(); err != nil { // opening '{'
		t.Fatalf("decode token: %v", err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key: %v", err)
		}
		order = append(order, tok.(string))
		// skip the value
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode value: %v", err)
		}
	}
	want := []string{"config", "memory", "taste", "skills", "permissionMode", "threadId", "mode", "params"}
	if len(order) != len(want) {
		t.Fatalf("envelope key order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("envelope key order = %v, want %v", order, want)
		}
	}

	var env struct {
		Config struct {
			WorkingDir  string `json:"workingDir"`
			Environment string `json:"environment"`
			IsGitRepo   bool   `json:"isGitRepo"`
		} `json:"config"`
		Memory         json.RawMessage `json:"memory"`
		Taste          json.RawMessage `json:"taste"`
		Skills         json.RawMessage `json:"skills"`
		PermissionMode string          `json:"permissionMode"`
		ThreadID       string          `json:"threadId"`
		Mode           string          `json:"mode"`
		Params         struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Stream    bool   `json:"stream"`
			System    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"system"`
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
			ToolChoice map[string]any `json:"tool_choice"`
		} `json:"params"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Config.Environment != "win32" || env.Config.IsGitRepo {
		t.Fatalf("config mismatch: %+v", env.Config)
	}
	if string(env.Memory) != "null" || string(env.Taste) != "null" || string(env.Skills) != "null" {
		t.Fatalf("memory/taste/skills must be null: %s %s %s", env.Memory, env.Taste, env.Skills)
	}
	if env.PermissionMode != "standard" || env.Mode != "agent" {
		t.Fatalf("permissionMode/mode mismatch: %q/%q", env.PermissionMode, env.Mode)
	}
	if env.ThreadID != sessionID {
		t.Fatalf("threadId %q != session id %q", env.ThreadID, sessionID)
	}
	if env.Params.Model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("params.model = %q", env.Params.Model)
	}
	if env.Params.MaxTokens != 100 {
		t.Fatalf("params.max_tokens = %d, want 100", env.Params.MaxTokens)
	}
	if !env.Params.Stream {
		t.Fatal("params.stream must always be true")
	}
	// System prompt: one block, no trailing newline on the last block.
	if len(env.Params.System) != 1 || env.Params.System[0].Text != "You are helpful." {
		t.Fatalf("system blocks = %+v", env.Params.System)
	}
	// Tools: name/description/input_schema, no type field.
	if len(env.Params.Tools) != 1 {
		t.Fatalf("tools = %+v", env.Params.Tools)
	}
	if env.Params.Tools[0].Name != "get_weather" || env.Params.Tools[0].Description != "Get weather" {
		t.Fatalf("tool mismatch: %+v", env.Params.Tools[0])
	}
	if string(env.Params.Tools[0].InputSchema) == "" {
		t.Fatal("tool input_schema is empty")
	}
	if env.Params.ToolChoice["type"] != "auto" {
		t.Fatalf("tool_choice = %+v", env.Params.ToolChoice)
	}
}

func TestCCBuildEnvelopeEmptySystemPlaceholder(t *testing.T) {
	clientBody := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	envelope, _, err := ccBuildEnvelope(clientBody, "m", ccTestAPIKey, nil)
	if err != nil {
		t.Fatalf("ccBuildEnvelope: %v", err)
	}
	var env struct {
		Params struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		} `json:"params"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Params.System) != 1 || env.Params.System[0].Text != " " {
		t.Fatalf("empty-system placeholder = %+v, want single space", env.Params.System)
	}
}

func TestCCBuildEnvelopeToolNoneClearsTools(t *testing.T) {
	clientBody := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`)
	envelope, _, err := ccBuildEnvelope(clientBody, "m", ccTestAPIKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Params struct {
			Tools      []any           `json:"tools"`
			ToolChoice json.RawMessage `json:"tool_choice"`
		} `json:"params"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Params.Tools) != 0 {
		t.Fatalf("tool_choice=none must clear tools, got %+v", env.Params.Tools)
	}
	if len(env.Params.ToolChoice) != 0 {
		t.Fatalf("tool_choice=none must omit tool_choice, got %s", env.Params.ToolChoice)
	}
}

func TestCCBuildEnvelopeAssistantToolCallAndToolResult(t *testing.T) {
	clientBody := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"sunny"}
		]
	}`)
	envelope, _, err := ccBuildEnvelope(clientBody, "m", ccTestAPIKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Params struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type       string `json:"type"`
					ToolCallID string `json:"toolCallId"`
					ToolName   string `json:"toolName"`
					Input      any    `json:"input"`
					Output     struct {
						Type  string `json:"type"`
						Value any    `json:"value"`
					} `json:"output"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"params"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatal(err)
	}
	// user, assistant(tool-call), tool(tool-result)
	if len(env.Params.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(env.Params.Messages))
	}
	asm := env.Params.Messages[1]
	if asm.Role != "assistant" || len(asm.Content) != 1 || asm.Content[0].Type != "tool-call" {
		t.Fatalf("assistant message = %+v", asm)
	}
	if asm.Content[0].ToolName != "get_weather" {
		t.Fatalf("tool-call toolName = %q", asm.Content[0].ToolName)
	}
	input, _ := json.Marshal(asm.Content[0].Input)
	if !strings.Contains(string(input), "Paris") {
		t.Fatalf("tool-call input = %s", input)
	}
	tool := env.Params.Messages[2]
	if tool.Role != "tool" || len(tool.Content) != 1 || tool.Content[0].Type != "tool-result" {
		t.Fatalf("tool message = %+v", tool)
	}
	if tool.Content[0].ToolCallID != "call_1" || tool.Content[0].ToolName != "get_weather" {
		t.Fatalf("tool-result ids = %+v", tool.Content[0])
	}
	if tool.Content[0].Output.Value != "sunny" {
		t.Fatalf("tool-result value = %+v", tool.Content[0].Output.Value)
	}
}

func TestCCStreamToChatReader(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"text-delta","text":"Hel"}`,
		`{"type":"text-delta","text":"lo"}`,
		`{"type":"tool-call","toolCallId":"call_9","toolName":"f","input":{"a":1}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":2}}`,
		``,
	}, "\n")
	r := newCCStreamToChatReader(strings.NewReader(ndjson), "m", 12345)
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	s := out.String()
	// Role-bearing first chunk, then content, then tool call, then
	// finish chunk with usage, then [DONE].
	if !strings.Contains(s, `"role":"assistant"`) {
		t.Fatalf("no role chunk:\n%s", s)
	}
	if !strings.Contains(s, `"content":"Hel"`) || !strings.Contains(s, `"content":"Hello"[:0]+"`) {
		// content deltas are emitted per-event; just check both texts appear.
	}
	if !strings.Contains(s, `"content":"Hel"`) || !strings.Contains(s, `"content":"lo"`) {
		t.Fatalf("text deltas missing:\n%s", s)
	}
	if !strings.Contains(s, `"tool_calls"`) || !strings.Contains(s, `"name":"f"`) {
		t.Fatalf("tool call missing:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason missing:\n%s", s)
	}
	if !strings.Contains(s, `"completion_tokens":5`) || !strings.Contains(s, `"cached_tokens":2`) {
		t.Fatalf("usage missing:\n%s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", s)
	}
}

func TestCCStreamToChatReaderTruncated(t *testing.T) {
	// A stream with no finish event is a truncation: the translator
	// must surface an error event so the fallback walk can react.
	ndjson := `{"type":"text-delta","text":"par"}` + "\n"
	r := newCCStreamToChatReader(strings.NewReader(ndjson), "m", 1)
	var out bytes.Buffer
	io.Copy(&out, r)
	if !strings.Contains(out.String(), `"error"`) {
		t.Fatalf("truncated stream produced no error event:\n%s", out.String())
	}
}

func TestCCAggregateStream(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"reasoning-delta","text":"thinking"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":7,"outputTokens":3,"cachedInputTokens":1}}`,
		``,
	}, "\n")
	body, err := ccAggregateStream([]byte(ndjson), "m", 12345)
	if err != nil {
		t.Fatalf("ccAggregateStream: %v", err)
	}
	var completion struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		t.Fatalf("unmarshal completion: %v", err)
	}
	if completion.Object != "chat.completion" || completion.Model != "m" {
		t.Fatalf("completion header = %s/%s", completion.Object, completion.Model)
	}
	if completion.Choices[0].Message.Content != "Hello" {
		t.Fatalf("content = %q", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].Message.ReasoningContent != "thinking" {
		t.Fatalf("reasoning_content = %q", completion.Choices[0].Message.ReasoningContent)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	if completion.Usage.PromptTokens != 7 || completion.Usage.CompletionTokens != 3 || completion.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v", completion.Usage)
	}
}

func TestCCAggregateStreamGuards(t *testing.T) {
	// No finish event -> truncated -> 502.
	if _, err := ccAggregateStream([]byte(`{"type":"text-delta","text":"x"}`), "m", 1); err == nil {
		t.Fatal("no-finish stream must fail")
	} else if pe, ok := err.(*ProviderError); !ok || pe.StatusCode != 502 {
		t.Fatalf("no-finish error = %#v", err)
	}
	// Zero output tokens -> 429 (rate-limit family).
	zeroOut := `{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":0}}`
	if _, err := ccAggregateStream([]byte(zeroOut), "m", 1); err == nil {
		t.Fatal("zero-output stream must fail")
	} else if pe, ok := err.(*ProviderError); !ok || pe.StatusCode != 429 {
		t.Fatalf("zero-output error = %#v", err)
	}
}

func TestCCMapError(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{400, 400}, {401, 401}, {402, 429}, {403, 401}, {404, 404},
		{422, 400}, {429, 429}, {500, 502}, {502, 502}, {503, 503},
		{418, 502}, // unlisted -> retryable upstream_error
	}
	for _, c := range cases {
		pe := ccMapError(c.status, []byte(`{"error":{"message":"boom","code":"BAD_REQUEST"}}`))
		if pe.StatusCode != c.want {
			t.Fatalf("ccMapError(%d).StatusCode = %d, want %d", c.status, pe.StatusCode, c.want)
		}
	}
	// 429 carries a retry-after floor.
	pe := ccMapError(429, nil)
	if pe.RetryAfter != 30*time.Second {
		t.Fatalf("429 retry-after = %v", pe.RetryAfter)
	}
}

// TestCCMapErrorQuotaRefusal pins the one refinement on the raw
// CC_STATUS_MAP: a 400 whose body is a credit/quota exhaustion
// refusal is the same event as a 402 payment-required (already
// mapped to a 429 rate limit), not a malformed request. Measured
// live 2026-10-12: commandcode's free-tier accounts answer every
// request with HTTP 400 "You have insufficient credits ...", and
// classifying it as invalid_request_error left the endpoint in
// rotation with a zero-second cooldown -- 708 failed round trips in
// 90 minutes, 100% failure, never once appearing in cooldowns.json.
func TestCCMapErrorQuotaRefusal(t *testing.T) {
	const refusal = `{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","code":"BAD_REQUEST"}}`
	pe := ccMapError(400, []byte(refusal))
	if pe.StatusCode != 429 {
		t.Fatalf("a credits refusal mapped to %d, want 429", pe.StatusCode)
	}
	// The provider's own message survives into the client-visible error.
	if !strings.Contains(string(pe.Body), "insufficient credits") {
		t.Errorf("the refusal message was dropped from the mapped body: %s", pe.Body)
	}
	// An ordinary 400 keeps the raw mapping.
	pe = ccMapError(400, []byte(`{"error":{"message":"max_tokens is not supported","code":"BAD_REQUEST"}}`))
	if pe.StatusCode != 400 {
		t.Fatalf("an ordinary 400 mapped to %d, want 400", pe.StatusCode)
	}
}

func TestCCSlugifyProjectPath(t *testing.T) {
	if got := ccSlugifyProjectPath(`C:\Users\dev\projects\app`); got != "c-users-dev-projects-app" {
		t.Fatalf("slugify = %q", got)
	}
	if got := ccSlugifyProjectPath(""); got != "root" {
		t.Fatalf("slugify empty = %q", got)
	}
}

// TestCCStreamToolCallIndexCountsCalls pins the tool-call delta
// index: it counts tool calls, not chunks. A call issued after
// text deltas must still start at 0 -- OpenAI clients group
// tool-call deltas by this index, so a mis-indexed first call
// (the old behavior reused the chunk counter and emitted index 2
// for the stream below) is mis-grouped or dropped client-side.
func TestCCStreamToolCallIndexCountsCalls(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"text-delta","text":"Let me check"}`,
		`{"type":"text-delta","text":" that."}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"f","input":{"a":1}}`,
		`{"type":"tool-call","toolCallId":"call_2","toolName":"g","input":{"b":2}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":10,"outputTokens":5}}`,
		``,
	}, "\n")
	r := newCCStreamToChatReader(strings.NewReader(ndjson), "m", 1)
	var out bytes.Buffer
	io.Copy(&out, r)
	s := out.String()
	if !strings.Contains(s, `"id":"call_1","index":0`) {
		t.Fatalf("first tool call not indexed 0:\n%s", s)
	}
	if !strings.Contains(s, `"id":"call_2","index":1`) {
		t.Fatalf("second tool call not indexed 1:\n%s", s)
	}
}

// TestCCStreamZeroOutputGuard pins the streaming zero-output
// guard: a finish that reports zero output tokens with no
// content is an empty response, which the upstream treats as a
// rate-limit-family failure (the same rule ccAggregateStream
// enforces on the non-streaming path and the reference proxy
// enforces on its stream). The translator must surface an error
// event -- never a clean [DONE] -- so the fallback walk can
// retry the next endpoint.
func TestCCStreamZeroOutputGuard(t *testing.T) {
	ndjson := `{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":0}}` + "\n"
	r := newCCStreamToChatReader(strings.NewReader(ndjson), "m", 1)
	var out bytes.Buffer
	io.Copy(&out, r)
	s := out.String()
	if !strings.Contains(s, `"error"`) {
		t.Fatalf("zero-output stream produced no error event:\n%s", s)
	}
	if !strings.Contains(s, "zero output tokens") {
		t.Fatalf("wrong error message:\n%s", s)
	}
	if strings.Contains(s, "data: [DONE]") {
		t.Fatalf("zero-output stream must not terminate cleanly:\n%s", s)
	}

	// The guard must NOT fire once content has been emitted: an
	// error event after delivered text would truncate a good
	// response. A stream with text and a zero-token finish
	// (a broken-usage upstream) still ends in [DONE].
	withText := strings.Join([]string{
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":0}}`,
		``,
	}, "\n")
	r = newCCStreamToChatReader(strings.NewReader(withText), "m", 1)
	out.Reset()
	io.Copy(&out, r)
	if !strings.Contains(out.String(), "data: [DONE]") {
		t.Fatalf("stream with delivered text must still terminate:\n%s", out.String())
	}
}

// TestCCUpstreamProxyRouting pins CC_UPSTREAM_PROXY: when set,
// every Command Code upstream request dials the proxy with
// CONNECT instead of going direct, and the proxy-routed client is
// the one the request path uses (Forward, StreamToClient and the
// fingerprint/lifecycle pre-requests all share it).
func TestCCUpstreamProxyRouting(t *testing.T) {
	// Without the env var the commandcode accessors fall back to
	// the direct client/transport.
	direct := NewProxy(&Config{})
	if direct.ccClient != nil || direct.ccTransport != nil {
		t.Fatal("no proxy configured, yet a proxied client was built")
	}
	if direct.commandCodeClient() != direct.client {
		t.Fatal("commandCodeClient() must fall back to the shared client")
	}
	if direct.commandCodeTransport() != direct.baseTransport {
		t.Fatal("commandCodeTransport() must fall back to the shared transport")
	}

	// With the env var set, the generation request must arrive at
	// the proxy as a CONNECT for the Command Code API host. The
	// fake proxy cannot complete the TLS handshake that follows
	// its 200, so Forward returns an error -- the CONNECT itself
	// is the assertion.
	var mu sync.Mutex
	var connects []string
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connects = append(connects, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer fakeProxy.Close()

	t.Setenv("CC_UPSTREAM_PROXY", fakeProxy.URL)
	cfg := &Config{Providers: map[string]ProviderConfig{
		"commandcode": {URL: "https://api.commandcode.ai"},
	}}
	p := NewProxy(cfg)
	if p.ccClient == nil || p.ccTransport == nil {
		t.Fatal("CC_UPSTREAM_PROXY did not build the proxied client/transport")
	}
	if p.commandCodeClient() == p.client {
		t.Fatal("commandCodeClient() must return the proxied client")
	}

	_, err := p.Forward(context.Background(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		ModelEndpoint{Provider: "commandcode", Model: "m", Protocol: "commandcode"}, "s")
	if err == nil {
		t.Log("note: Forward unexpectedly succeeded through the fake proxy")
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, c := range connects {
		if c == "CONNECT api.commandcode.ai:443" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no CONNECT to api.commandcode.ai:443 reached the proxy; saw %v", connects)
	}
}

// TestRedactProxyURL pins the log redaction: a proxy URL may
// carry user:pass and must never reach a log line whole.
func TestRedactProxyURL(t *testing.T) {
	if got := redactProxyURL("http://user:secret@proxy.example:8080"); got != "http://proxy.example:8080" {
		t.Fatalf("redactProxyURL = %q", got)
	}
	if got := redactProxyURL(""); got != "(invalid proxy url)" {
		t.Fatalf("redactProxyURL(\"\") = %q", got)
	}
}

// TestCCAssistantReasoningNotDuplicated pins the reference's
// `!msg.reasoning_content` guard: a client echoing its reasoning in BOTH
// reasoning_content and a content[] reasoning part must not send the same
// thinking text twice on the wire. When reasoning_content is absent the
// reasoning part must survive.
func TestCCAssistantReasoningNotDuplicated(t *testing.T) {
	reasoningPartsOf := func(t *testing.T, body []byte) []string {
		t.Helper()
		envelope, _, err := ccBuildEnvelope(body, "m", ccTestAPIKey, nil)
		if err != nil {
			t.Fatalf("ccBuildEnvelope: %v", err)
		}
		var env struct {
			Params struct {
				Messages []struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"messages"`
			} `json:"params"`
		}
		if err := json.Unmarshal(envelope, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		for _, m := range env.Params.Messages {
			if m.Role != "assistant" {
				continue
			}
			var out []string
			for _, p := range m.Content {
				if p.Type == "reasoning" {
					out = append(out, p.Text)
				}
			}
			return out
		}
		t.Fatal("no assistant message in the envelope")
		return nil
	}

	both := []byte(`{"model":"m","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","reasoning_content":"think","content":[{"type":"reasoning","text":"think"},{"type":"text","text":"answer"}]}
	]}`)
	if got := reasoningPartsOf(t, both); len(got) != 1 || got[0] != "think" {
		t.Fatalf("reasoning parts = %v, want exactly one 'think': reasoning_content plus a reasoning part must not duplicate the thinking text", got)
	}

	onlyPart := []byte(`{"model":"m","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":[{"type":"reasoning","text":"think"},{"type":"text","text":"answer"}]}
	]}`)
	if got := reasoningPartsOf(t, onlyPart); len(got) != 1 || got[0] != "think" {
		t.Fatalf("reasoning parts = %v, want exactly one 'think' kept when reasoning_content is absent", got)
	}
}

// TestCCMaxTokensExplicitZeroUsesDefault pins the reference's
// `Math.min(max_tokens || 64000, 200000)`: an absent OR explicit-zero
// max_tokens means the default budget. A literal 0 promises the upstream
// an empty completion, which its zero-output guard rejects as a
// rate-limit-family failure.
func TestCCMaxTokensExplicitZeroUsesDefault(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"absent", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 64000},
		{"explicit zero", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`, 64000},
		{"explicit budget", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`, 100},
		{"above the cap", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":500000}`, 200000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			envelope, _, err := ccBuildEnvelope([]byte(c.body), "m", ccTestAPIKey, nil)
			if err != nil {
				t.Fatalf("ccBuildEnvelope: %v", err)
			}
			var env struct {
				Params struct {
					MaxTokens int `json:"max_tokens"`
				} `json:"params"`
			}
			if err := json.Unmarshal(envelope, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if env.Params.MaxTokens != c.want {
				t.Fatalf("params.max_tokens = %d, want %d", env.Params.MaxTokens, c.want)
			}
		})
	}
}

// TestCCStreamHTTPStatusIsMapped pins streaming-path parity with the
// non-streaming path (api.go): a Command Code HTTP error must reach the
// cooldown classifier as its CC_STATUS_MAP equivalent. A raw 402 would
// bypass the transient 429 escalation curve and walk into a soft ban
// after three consecutive failures.
func TestCCStreamHTTPStatusIsMapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits","code":"PAYMENT_REQUIRED"}}`))
	}))
	defer srv.Close()

	p := NewProxy(&Config{Providers: map[string]ProviderConfig{
		"commandcode": {URL: srv.URL},
	}})
	endpoint := ModelEndpoint{Provider: "commandcode", Model: "m", Protocol: "commandcode"}
	_, _, _, err := p.StreamToClient(context.Background(), io.Discard, nopFlusher{},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), endpoint, 5*time.Second, "s")
	if err == nil {
		t.Fatal("expected the 402 upstream to fail the attempt")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("got %T (%v), want *ProviderError", err, err)
	}
	if pe.StatusCode != 429 {
		t.Fatalf("StatusCode = %d, want 429: CC_STATUS_MAP maps 402 to the rate-limit family", pe.StatusCode)
	}
}
