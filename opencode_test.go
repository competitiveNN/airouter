package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
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
	// tools (the gate must not duplicate them). Both tools carry a
	// parameters schema with at least one property: measured live
	// against opencode.ai/inference/openai/v1 on 2026-10-15, a `read`
	// tool with `{"type":"object","properties":{}}` -- or no parameters
	// key at all -- is refused with 403 FreeTierError, so a body the
	// gate accepts must declare a property. The assertion is about
	// non-duplication, not about whether the client's schema is empty.
	satisfied := `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
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

	// The injected gate tools must carry a parameters schema with at least
	// one property. Measured live against opencode.ai/inference/openai/v1
	// on 2026-10-15: a `read` tool whose parameters is
	// `{"type":"object","properties":{}}` is refused with 403 FreeTierError,
	// while one carrying any property passes. An empty schema used to be
	// emitted here and is exactly what made every free-tier request 403.
	for _, tool := range []map[string]json.RawMessage{opencodeGateTool("bash"), opencodeGateTool("read")} {
		var fn struct {
			Parameters json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(tool["function"], &fn); err != nil {
			t.Fatalf("injected gate tool is malformed: %v", err)
		}
		var params struct {
			Properties json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(fn.Parameters, &params); err != nil {
			t.Fatalf("injected gate tool parameters is not an object: %s", fn.Parameters)
		}
		if len(params.Properties) == 0 || string(params.Properties) == "{}" {
			t.Errorf("injected gate tool parameters has no properties: %s", fn.Parameters)
		}
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

// opencodeGateBodyMapOracle is the implementation opencodeGateBody used before
// it was rewritten onto byte spans: decode into a map, rewrite, re-encode. It
// survives here as the oracle the span version is checked against.
func opencodeGateBodyMapOracle(body []byte) []byte {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}
	data["stream"] = json.RawMessage("true")
	var tools []map[string]json.RawMessage
	if raw, ok := data["tools"]; ok {
		_ = json.Unmarshal(raw, &tools)
	}
	hasTool := func(name string) bool {
		for _, tool := range tools {
			if opencodeGateToolName(tool) == name {
				return true
			}
		}
		return false
	}
	if !hasTool("bash") {
		tools = append(tools, opencodeGateTool("bash"))
	}
	if !hasTool("read") {
		tools = append(tools, opencodeGateTool("read"))
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return body
	}
	data["tools"] = encoded
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

// opencodeConversationIDDecoderOracle is the decoder-based implementation of
// opencodeConversationID, kept as the oracle for the span-based one.
func opencodeConversationIDDecoderOracle(body []byte) string {
	var partial struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &partial); err != nil || partial.Model == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(partial.Model))
	anchored := false
	for _, role := range []string{"system", "user"} {
		for _, m := range partial.Messages {
			if m.Role != role {
				continue
			}
			h.Write([]byte{0})
			h.Write(m.Content)
			anchored = true
			break
		}
	}
	if !anchored {
		return ""
	}
	return "conv:" + hex.EncodeToString(h.Sum(nil)[:16])
}

var opencodeGateCorpus = []string{
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`,
	`{"model":"m","stream":false,"tools":[{"type":"function","function":{"name":"bash"}},{"type":"function","function":{"name":"read"}}]}`,
	`{"model":"m","tools":[{"type":"function","name":"bash"}]}`,
	`{"model":"m","stream":true,"tools":[]}`,
	`{"model":"m","stream":true,"tools":null}`,
	`{"model":"m","stream":true,"tools":{}}`,
	`{"model":"m","stream":false,"stream":true,"tools":[{"name":"re\u0061d"},{"type":"function","function":{"name":"bash"}}]}`,
	`{"model":"m","tools":[{"type":"function","function":{"name":"bash","description":"the client's own bash"}},{"type":"function","function":{"name":"edit"}}],"temperature":0.2}`,
	" {\n \"model\" : \"m\" ,\n \"stream\" : false ,\n \"unknown\" : [1,2,3] ,\n \"messages\" : [ { \"role\" : \"user\" , \"content\" : \"hi\" } ]\n } ",
}

// TestOpencodeGateBodyMatchesTheMapRewrite pins the span-based gate rewrite to
// the behaviour of the map-based one it replaced, on the shapes the gate has
// to handle: nothing sent, everything already sent, the tool under either
// spelling, and every non-array spelling of `tools`.
func TestOpencodeGateBodyMatchesTheMapRewrite(t *testing.T) {
	for _, body := range opencodeGateCorpus {
		want := opencodeGateBodyMapOracle([]byte(body))
		got := opencodeGateBody([]byte(body))

		var wantV, gotV any
		if err := json.Unmarshal(want, &wantV); err != nil {
			t.Fatalf("oracle produced invalid JSON for %s: %v", body, err)
		}
		if err := json.Unmarshal(got, &gotV); err != nil {
			t.Fatalf("gate produced invalid JSON for %s: %v (%s)", body, err, got)
		}
		if !reflect.DeepEqual(wantV, gotV) {
			t.Errorf("gate rewrite differs from the map rewrite\nbody: %s\ngot:  %s\nwant: %s", body, got, want)
		}
	}

	// A body that already satisfies the gate is forwarded byte for byte: the
	// span rewrite has nothing to splice, so it must not re-encode anything.
	satisfied := opencodeGateCorpus[1]
	if got := opencodeGateBody([]byte(satisfied)); string(got) != satisfied {
		t.Errorf("satisfied body was rewritten:\ngot:  %s\nwant: %s", got, satisfied)
	}
}

// TestOpencodeGateBodyKeepsClientBytes verifies the reason for the rewrite:
// every byte the client sent survives, including fields and key order the
// gateway does not understand. The map-based version dropped nothing either,
// but it re-encoded everything, which is what made the rewrite cost a copy of
// the whole conversation.
func TestOpencodeGateBodyKeepsClientBytes(t *testing.T) {
	body := `{"z_client":"first","model":"m","tools":[{"type":"function","function":{"name":"bash","description":"keep my bytes"}}],"a_last":{"keep":true}}`
	out := string(opencodeGateBody([]byte(body)))
	if !strings.HasPrefix(out, `{"z_client":"first","model":"m","tools":[{"type":"function","function":{"name":"bash","description":"keep my bytes"}}`) {
		t.Errorf("client prefix was disturbed: %s", out)
	}
	if !strings.Contains(out, `"z_client":"first"`) || !strings.Contains(out, `"a_last":{"keep":true}`) {
		t.Errorf("client fields were lost: %s", out)
	}
	if !strings.Contains(out, `,"stream":true}`) {
		t.Errorf("stream was not appended to the object: %s", out)
	}
	if strings.Count(out, `"stream"`) != 1 {
		t.Errorf("stream was added more than once: %s", out)
	}
	if !strings.Contains(out, `"read"`) {
		t.Errorf("read tool was not injected: %s", out)
	}
}

var opencodeConversationCorpus = []string{
	`{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`,
	`{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"system","content":"sys"}]}`,
	`{"model":"m","messages":[{"role":"user","content":"hello"}]}`,
	`{"model":"m","messages":[{"role":"system","content":"sys"}]}`,
	`{"model":"m","messages":[{"role":"assistant","content":"a"}]}`,
	`{"messages":[{"role":"user","content":"hello"}]}`,
	`{"model":"","messages":[{"role":"user","content":"hello"}]}`,
	`{"model":"m","messages":[{"role":"system"},{"role":"user","content":"hello"}]}`,
	`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"s"}]},{"role":"user","content":[{"type":"text","text":"h"}]}]}`,
	`{"model":"m","messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}]}`,
	`{"model":"m","messages":[]}`,
	`{"model":"m","messages":null}`,
	`{"model":"m","messages":{"role":"user"}}`,
	`{"model":"m","messages":[1]}`,
	`{"model":"m","messages":[{"role":7,"content":"x"}]}`,
	`{"model":7,"messages":[{"role":"user","content":"x"}]}`,
	`{"model":"m","messages":[{"role":"user","content":"x"}]} trailing`,
	`{"model":"a","model":"b","messages":[{"role":"user","content":"x"}]}`,
	`{"model":"m","messages":[{"role":"system","content":"a \"quoted\" } ] brace"},{"role":"user","content":"hi"}]}`,
	" {\n \"model\":\"m\",\n \"messages\":[ {\"role\":\"user\",\"content\":\"x\"} ]\n }\n",
}

// TestOpencodeConversationIDMatchesTheDecoder pins the span-based conversation
// anchor to the decoder-based one: the id is the OpenCode session identity, so
// a byte of drift splits one conversation across two upstream sessions.
func TestOpencodeConversationIDMatchesTheDecoder(t *testing.T) {
	for _, body := range opencodeConversationCorpus {
		want := opencodeConversationIDDecoderOracle([]byte(body))
		got := opencodeConversationID([]byte(body))
		if got != want {
			t.Errorf("conversation id differs from the decoder\nbody: %s\ngot:  %q\nwant: %q", body, got, want)
		}
	}
}

// TestOpencodeRewriteDoesNotCopyTheBody is the reason both functions were
// rewritten: their cost used to be a full decode (and, for the gate, a full
// re-encode) of the conversation on every attempt on the OpenCode path.
func TestOpencodeRewriteDoesNotCopyTheBody(t *testing.T) {
	big := strings.Repeat("x", 200<<10)
	anchored := []byte(`{"model":"m","messages":[{"role":"system","content":"` + big + `"},{"role":"user","content":"` + big + `"}]}`)
	satisfied := []byte(`{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"bash"}},{"type":"function","function":{"name":"read"}}],"messages":[{"role":"user","content":"` + big + `"}]}`)

	measure := func(t *testing.T, name string, body []byte, fn func([]byte) int) {
		t.Helper()
		for i := 0; i < 5; i++ {
			fn(body)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		const runs = 50
		var sink int
		for i := 0; i < runs; i++ {
			sink += fn(body)
		}
		runtime.ReadMemStats(&after)
		if sink == 0 {
			t.Fatalf("%s reported no work on a %d-byte body", name, len(body))
		}
		perCall := (after.TotalAlloc - before.TotalAlloc) / runs
		if limit := uint64(len(body)) / 8; perCall > limit {
			t.Errorf("%s allocated %d bytes per call on a %d-byte body (limit %d): it is copying the conversation again", name, perCall, len(body), limit)
		}
	}

	measure(t, "opencodeConversationID", anchored, func(body []byte) int {
		return len(opencodeConversationID(body))
	})
	measure(t, "opencodeGateBody", satisfied, func(body []byte) int {
		return len(opencodeGateBody(body)) - len(body) + 1
	})
}
