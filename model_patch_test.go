package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The real payload shape observed live from dots-studio/dots-3-note-preview:
// a tool call wrapped in thinking as Anthropic-style XML.
const dotsSampleReasoning = "thinking> <dots_function_call>\n<invoke name=\"bg_status\">\n<parameter name=\"read\">\ntrue\n</parameter>\n<parameter name=\"session\">\nCfvc87M6BoC3ZUU4U2QGH\n</parameter>\n</invoke>\n</dots_function_call>"

// dotsCompletion builds a chat completion whose message carries
// the given thinking text.
func dotsCompletion(thinking string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"dots-studio/dots-3-note-preview:free","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":` + jsonString(thinking) + `}}]}`
}

// jsonString encodes s as a JSON string literal.
func jsonString(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestModelPatchForDotsModel(t *testing.T) {
	for _, id := range []string{
		"dots-studio/dots-3-note-preview:free",
		"dots-studio/dots-3-note-preview",
		"DOTS-STUDIO/DOTS-3-NOTE-PREVIEW:FREE",
	} {
		p := modelPatchFor(id)
		if p == nil {
			t.Errorf("modelPatchFor(%q) = nil, want the dots patch", id)
			continue
		}
		if !p.Matches(id) {
			t.Errorf("patch does not match %q", id)
		}
	}
}

func TestModelPatchForUnknownModel(t *testing.T) {
	for _, id := range []string{
		"",
		"opencode/big-pickle-free",
		"dots-studio/dots-3-note-preview-v2:free",
		"commandcode2/poolside-laguna",
	} {
		if p := modelPatchFor(id); p != nil {
			t.Errorf("modelPatchFor(%q) = %T, want nil", id, p)
		}
	}
}

func TestDotsPatchTransformCompletion_InvokeForm(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	out := p.TransformCompletion([]byte(dotsCompletion(dotsSampleReasoning)))

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.Reasoning != "thinking> " {
		t.Errorf("reasoning_content = %q, want %q (tag stripped)", msg.Reasoning, "thinking> ")
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.Function.Name != "bg_status" {
		t.Errorf("tool name = %q, want bg_status", tc.Function.Name)
	}
	if tc.Type != "function" {
		t.Errorf("tool type = %q, want function", tc.Type)
	}
	if tc.ID == "" {
		t.Errorf("tool id is empty")
	}
	// Arguments are the parameters as a JSON object of strings.
	if tc.Function.Arguments != `{"read":"true","session":"Cfvc87M6BoC3ZUU4U2QGH"}` {
		t.Errorf("tool arguments = %q, want the parameter map as JSON", tc.Function.Arguments)
	}
}

func TestDotsPatchTransformCompletion_JSONForm(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	thinking := `<dots_function_call>{"name":"get_weather","arguments":{"city":"Paris"}}</dots_function_call>`
	out := p.TransformCompletion([]byte(dotsCompletion(thinking)))
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Errorf("JSON-form call not extracted: %s", out)
	}
	if !strings.Contains(string(out), `"arguments":"{\"city\":\"Paris\"}"`) {
		t.Errorf("JSON-form arguments not carried over: %s", out)
	}
	if strings.Contains(string(out), "dots_function_call") {
		t.Errorf("tag leaked into output: %s", out)
	}
}

func TestDotsPatchTransformCompletion_AttributeForm(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	thinking := `let me <dots_function_call name="search" arguments='{"q":"airouter"}'>check</dots_function_call> now`
	out := p.TransformCompletion([]byte(dotsCompletion(thinking)))
	if !strings.Contains(string(out), `"name":"search"`) {
		t.Errorf("attribute-form call not extracted: %s", out)
	}
	if strings.Contains(string(out), "dots_function_call") {
		t.Errorf("tag leaked into output: %s", out)
	}
}

func TestDotsPatchTransformCompletion_MultipleInvokes(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	thinking := `<dots_function_call>` +
		`<invoke name="first"><parameter name="a">1</parameter></invoke>` +
		`<invoke name="second"><parameter name="b">2</parameter></invoke>` +
		`</dots_function_call>`
	out := p.TransformCompletion([]byte(dotsCompletion(thinking)))
	if strings.Count(string(out), `"type":"function"`) != 2 {
		t.Errorf("want 2 tool calls in output: %s", out)
	}
	if !strings.Contains(string(out), `"name":"first"`) || !strings.Contains(string(out), `"name":"second"`) {
		t.Errorf("both invokes must be extracted: %s", out)
	}
}

func TestDotsPatchTransformCompletion_Passthrough(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	// No tag: byte-identical output, including unknown fields.
	body := `{"id":"x","model":"dots-studio/dots-3-note-preview:free","choices":[{"message":{"role":"assistant","content":"hello <b>world</b>","reasoning_content":"thinking about <b>tags</b>"}}],"unknown_field":{"a":1}}`
	out := p.TransformCompletion([]byte(body))
	if string(out) != body {
		t.Errorf("tag-free body was rewritten:\n got: %s\nwant: %s", out, body)
	}
	// A '<' that is not a dots tag stays untouched.
	body2 := dotsCompletion("a < b and <dots_function_callX> stays")
	out2 := p.TransformCompletion([]byte(body2))
	if string(out2) != body2 {
		t.Errorf("non-tag angle brackets were rewritten:\n got: %s\nwant: %s", out2, body2)
	}
	// Unterminated block is preserved verbatim.
	body3 := dotsCompletion("start <dots_function_call><invoke name=\"x\">")
	out3 := p.TransformCompletion([]byte(body3))
	if string(out3) != body3 {
		t.Errorf("unterminated block was rewritten:\n got: %s\nwant: %s", out3, body3)
	}
}

func TestDotsPatchTransformStreamChunk_SplitTag(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	st := p.BeginStream()

	chunks := []string{
		`{"choices":[{"delta":{"role":"assistant","content":"","reasoning":"Let me check "}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning":"<dots"}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning":"_function_call>\n<invoke name=\"bg_status\">"}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning":"<parameter name=\"read\">true</parameter>"}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning":"</invoke>\n</dots_function_call>"}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning":" done."}}]}`,
	}
	var cleaned strings.Builder
	var calls []PatchToolCall
	for _, c := range chunks {
		out, extracted := p.TransformStreamChunk(st, []byte(c))
		cleaned.Write(out)
		calls = append(calls, extracted...)
	}
	// The tag streams in pieces; only the chunk carrying the
	// closer completes the call.
	if len(calls) != 1 {
		t.Fatalf("extracted %d calls, want 1", len(calls))
	}
	if calls[0].Name != "bg_status" {
		t.Errorf("call name = %q, want bg_status", calls[0].Name)
	}
	if calls[0].Arguments != `{"read":"true"}` {
		t.Errorf("call arguments = %q, want {\"read\":\"true\"}", calls[0].Arguments)
	}
	if calls[0].ID == "" {
		t.Errorf("call id is empty")
	}
	// Reassembled reasoning is the original minus the tag.
	if got := dotsConcatenatedReasoning(cleaned.String()); got != "Let me check  done." {
		t.Errorf("concatenated reasoning = %q, want %q", got, "Let me check  done.")
	}
}

// dotsConcatenatedReasoning extracts the reasoning fields from a
// sequence of chunk payloads.
func dotsConcatenatedReasoning(payloads string) string {
	var out strings.Builder
	for _, payload := range strings.Split(payloads, "}{") {
		payload = strings.TrimPrefix(payload, "{")
		payload = strings.TrimSuffix(payload, "}")
		if i := strings.Index(payload, `"reasoning":"`); i >= 0 {
			rest := payload[i+len(`"reasoning":"`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				out.WriteString(rest[:j])
			}
		}
	}
	return out.String()
}

func TestDotsPatchTransformStreamChunk_OrdinaryAngleBrackets(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	st := p.BeginStream()
	chunks := []string{
		`{"choices":[{"delta":{"reasoning":"a < b"}}]}`,
		`{"choices":[{"delta":{"reasoning":"c <dots are cool"}}]}`,
	}
	var reasoning strings.Builder
	for _, c := range chunks {
		out, calls := p.TransformStreamChunk(st, []byte(c))
		if len(calls) != 0 {
			t.Fatalf("unexpected calls: %+v", calls)
		}
		reasoning.WriteString(dotsReasoningOf(t, out))
	}
	if reasoning.String() != "a < bc <dots are cool" {
		t.Errorf("concatenated reasoning = %q", reasoning.String())
	}
}

// dotsReasoningOf extracts the reasoning field of one chunk payload.
func dotsReasoningOf(t *testing.T, payload []byte) string {
	t.Helper()
	var chunk struct {
		Choices []struct {
			Delta struct {
				Reasoning string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		t.Fatalf("bad payload %s: %v", payload, err)
	}
	if len(chunk.Choices) == 0 {
		return ""
	}
	return chunk.Choices[0].Delta.Reasoning
}

func TestDotsPatchFlushStream_UnterminatedTag(t *testing.T) {
	p := modelPatchFor("dots-studio/dots-3-note-preview:free")
	st := p.BeginStream()
	if _, calls := p.TransformStreamChunk(st, []byte(`{"choices":[{"delta":{"reasoning":"keep me <dots_function_call><invoke name=\"x\">"}}]}`)); len(calls) != 0 {
		t.Fatalf("unterminated tag must not yield a call: %+v", calls)
	}
	final, calls := p.FlushStream(st)
	if len(calls) != 0 {
		t.Fatalf("flush must not invent calls: %+v", calls)
	}
	if final == nil {
		t.Fatalf("flush dropped held text")
	}
	if got := dotsReasoningOf(t, final); got != "<invoke name=\"x\">" {
		t.Errorf("flush reasoning = %q, want the held text verbatim", got)
	}
	// A second flush has nothing left to emit.
	if final2, _ := p.FlushStream(st); final2 != nil {
		t.Errorf("second flush emitted %s, want nil", final2)
	}
}

func TestStreamSSE_DotsThinkingOnlyStream(t *testing.T) {
	// The reported failure shape: a reply that is only a thinking
	// block (with the tool call trapped inside) and an empty
	// content message.
	input := "data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"model\":\"dots-studio/dots-3-note-preview:free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning\":\"thinking> <dots\"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"model\":\"dots-studio/dots-3-note-preview:free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\",\"reasoning\":\"_function_call>\\n<invoke name=\\\"bg_status\\\">\\n<parameter name=\\\"read\\\">true</parameter>\\n</invoke>\\n</dots_function_call>\"}}]}\n\n" +
		"data: [DONE]\n\n"

	for _, toolCalls := range []bool{true, false} {
		t.Run(map[bool]string{true: "coalesced", false: "verbatim"}[toolCalls], func(t *testing.T) {
			buf := &bytes.Buffer{}
			flusher := &testFlusher{buf}
			p := &Proxy{}
			p.SetToolCalls(toolCalls)

			acc, tcs, _, err := p.streamSSE(flusher, flusher, strings.NewReader(input), "dots-studio/dots-3-note-preview:free")
			if err != nil {
				t.Fatalf("streamSSE returned error: %v", err)
			}
			_ = acc

			clientPayload := buf.String()
			// The tag must never reach the client's thinking.
			if strings.Contains(clientPayload, "dots_function_call") {
				t.Errorf("client stream leaked the raw tag:\n%s", clientPayload)
			}
			// The tool call must be synthesized...
			if !strings.Contains(clientPayload, `"name":"bg_status"`) {
				t.Errorf("client stream missing the rescued tool call:\n%s", clientPayload)
			}
			if !strings.Contains(clientPayload, `"arguments":"{\"read\":\"true\"}"`) {
				t.Errorf("client stream missing the rescued tool arguments:\n%s", clientPayload)
			}
			// ...before [DONE]: clients finalize parsing there.
			tcIdx := strings.Index(clientPayload, `"name":"bg_status"`)
			doneIdx := strings.Index(clientPayload, "[DONE]")
			if tcIdx == -1 || doneIdx == -1 || tcIdx > doneIdx {
				t.Errorf("tool call does not precede [DONE]:\n%s", clientPayload)
			}
			// And the fallback accumulator carries it, so a
			// mid-stream failure replays it.
			if len(tcs) != 1 || tcs[0].Name != "bg_status" {
				t.Errorf("fallback accumulator = %+v, want one bg_status call", tcs)
			}
		})
	}
}

func TestStreamSSE_DotsStreamWithContent(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning\":\"let me <dots_function_call><invoke name=\\\"f\\\"/></dots_function_call>\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"here is the answer\"}}]}\n\n" +
		"data: [DONE]\n\n"

	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, strings.NewReader(input), "dots-studio/dots-3-note-preview:free")
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}
	clientPayload := buf.String()
	if !strings.Contains(clientPayload, "here is the answer") {
		t.Errorf("content missing from client stream:\n%s", clientPayload)
	}
	if strings.Contains(clientPayload, "dots_function_call") {
		t.Errorf("tag leaked into client stream:\n%s", clientPayload)
	}
	if !strings.Contains(clientPayload, `"name":"f"`) {
		t.Errorf("rescued tool call missing:\n%s", clientPayload)
	}
	if len(tcs) != 1 || tcs[0].Name != "f" {
		t.Errorf("fallback accumulator = %+v, want one f call", tcs)
	}
}

func TestStreamSSE_NonDotsModelUntouched(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning\":\"raw <dots_function_call> text stays\"}}]}\n\n" +
		"data: [DONE]\n\n"

	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, strings.NewReader(input), "opencode/big-pickle-free")
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "dots_function_call") {
		t.Errorf("non-dots model stream was rewritten:\n%s", buf.String())
	}
	if len(tcs) != 0 {
		t.Errorf("non-dots model yielded tool calls: %+v", tcs)
	}
}

func TestTranslateChatSSE_DotsThinkingCall(t *testing.T) {
	// The Responses surface drops thinking, but the rescued tool
	// call must survive the translation.
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning\":\"<dots_function_call><invoke name=\\\"bg_status\\\"><parameter name=\\\"read\\\">true</parameter></invoke></dots_function_call>\"}}]}\n\n" +
		"data: [DONE]\n\n"

	buf := &bytes.Buffer{}
	sw := newResponsesStreamWriter(buf, nopFlusher{}, "smart")
	_, tcs, err := translateChatSSE(strings.NewReader(input), sw, "dots-studio/dots-3-note-preview:free")
	if err != nil {
		t.Fatalf("translateChatSSE returned error: %v", err)
	}
	_ = tcs
	if len(tcs) != 1 {
		t.Fatalf("translated tool calls = %+v, want one rescued call", tcs)
	}
	if tcs[0].Name != "bg_status" {
		t.Errorf("tool name = %q, want bg_status", tcs[0].Name)
	}
	if tcs[0].Arguments != `{"read":"true"}` {
		t.Errorf("tool arguments = %q", tcs[0].Arguments)
	}
}
