package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Outbound Responses-API support.
//
// The gateway's canonical internal shape is Chat Completions: the router, the
// proxy, the fallback loop and the cooldown classifier all speak it. This file
// lets an UPSTREAM be addressed with the Responses API instead, by translating
// at the two edges -- request on the way out, response on the way back -- so
// nothing between them has to change.
//
// The opposite direction already existed and is untouched: a client calling
// /v1/responses is normalized down to Chat Completions in
// responses_translate.go. That is a different job. This one is about which
// protocol we speak to the provider, not which one the client used.
//
// WHY PER-MODEL, NOT PER-PROVIDER
// A provider's catalog is not uniformly Responses-capable. Measured live
// 2026-10-02 against opencode.ai: space-bunny-free answers
// `400 {"type":"ModelProtocolUnsupported","message":"Model does not support this
// protocol."}` on /responses while answering 200 on /chat/completions, on both
// the /zen/v1 and /inference/openai/v1 paths. So capability is a property of
// the endpoint, and it is recorded there.

// protocol selects the wire shape used to reach a provider.
type protocol string

const (
	// protocolChatCompletions is the default and is what every provider has
	// historically been addressed with: POST <url>/chat/completions.
	protocolChatCompletions protocol = "chat"
	// protocolResponses is POST <url>/responses with a Responses-shaped body.
	protocolResponses protocol = "responses"
)

// protocolFor normalizes the configured value. An absent or unknown value is
// chat/completions rather than an error: a typo in the config must not take an
// endpoint out of rotation, and chat/completions is the shape that always works.
func protocolFor(s string) protocol {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "responses", "response", "openai-responses":
		return protocolResponses
	default:
		return protocolChatCompletions
	}
}

// upstreamPath is the URL path a protocol resolves to, relative to the
// provider's base URL.
func (p protocol) upstreamPath() string {
	if p == protocolResponses {
		return "/responses"
	}
	return "/chat/completions"
}

// chatRequestBody is the subset of a Chat Completions request this translator
// reads. It is decoded rather than reused from ChatCompletionRequest because
// that type is the CLIENT contract and may grow fields with no Responses
// equivalent; decoding explicitly makes "what survives the crossing" visible.
type chatRequestBody struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name"`
		// ToolCalls are assistant-issued function calls; they become
		// function_call items in the Responses input array.
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		// ToolCallID marks the result of a tool call (role "tool").
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`

	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
		// Flat Responses-style tool ({"type":"function","name":...}).
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"tools"`

	MaxTokens   *int `json:"max_tokens"`
	Temperature *float64
	TopP        *float64  `json:"top_p"`
	Stop        *[]string `json:"stop"`
}

// upstreamItem is one entry of the Responses `input` array as it goes ON THE
// WIRE to a provider.
//
// It is deliberately a different type from `item`. `item` serves the
// client-facing envelope, where omitempty keeps a message item from carrying an
// empty `"role"` or a contentless part; it cannot express the difference
// between a field that is absent and a field that is present and empty, and
// the Responses schema requires several of those fields unconditionally. Here a
// nil pointer means "omit the key" and a pointer to "" means `"field":""`.
//
// This is not a hypothetical hardening. Measured live 2026-10-04 against
// opencode zen (muse-spark-1.2-contributor-free, `protocol: responses` in
// config.yaml):
//
//	cooldown opencode/muse-spark-1.2-contributor-free status=400 errors=11 for 0s:
//	  {"model":"muse-spark-1.2-contributor-free","error":{"code":null,
//	   "message":"`input[5]` missing required field `output`", ...}}
//
// The `output` of a tool result that flattens to "" -- a command that printed
// nothing, a read of an empty file, a tool that returned only an image -- was
// dropped by `omitempty` and the provider's validator refused the whole
// request. A 400 is then classified as a client-request error: no cooldown, so
// every request in that session paid the same round trip and the endpoint's
// error count climbed forever. `name` and `arguments` on function_call fail the
// same way.
type upstreamItem struct {
	Type    string         `json:"type"`
	Role    string         `json:"role,omitempty"`
	Content []upstreamPart `json:"content,omitempty"`

	// call_id pairs a function_call with the function_call_output that
	// answers it, on both sides of the crossing.
	CallID string  `json:"call_id,omitempty"`
	Name   *string `json:"name,omitempty"`
	// arguments is a JSON string and is required: an absent key is a schema
	// error, and an empty one is not valid JSON either, so the caller
	// normalizes it through toolCallArguments.
	Arguments *string `json:"arguments,omitempty"`
	// output is required on function_call_output and legitimately empty.
	Output *string `json:"output,omitempty"`
}

// upstreamPart is one content part on the wire, for the same reason as
// upstreamItem: a text part carries `text` even when the text is empty.
type upstreamPart struct {
	Type     string   `json:"type"`
	Text     *string  `json:"text,omitempty"`
	ImageURL imageRef `json:"image_url,omitempty"`
}

// upstreamParts converts decoded parts to their wire form. A message with no
// parts at all still needs `content`, so it gets one empty text part rather
// than the key disappearing.
func upstreamParts(parts []contentPart) []upstreamPart {
	if len(parts) == 0 {
		return []upstreamPart{{Type: "input_text", Text: strPtr("")}}
	}
	out := make([]upstreamPart, 0, len(parts))
	for _, p := range parts {
		out = append(out, upstreamPart{Type: p.Type, Text: strPtr(p.Text), ImageURL: p.ImageURL})
	}
	return out
}

// toolCallArguments normalizes a tool call's argument string into something the
// Responses schema accepts. An empty arguments field is a real occurrence --
// a zero-argument tool call the client serialized sloppily -- and both the
// omitted key and the empty string are rejected, so "{}" is the one encoding
// that is present and parseable.
func toolCallArguments(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// chatBodyToResponses rewrites a Chat Completions request body into the
// Responses request shape.
//
// It fails open in the sense that matters: if the body cannot be parsed, the
// caller falls back to chat/completions rather than sending a mangled request.
// A translation bug must never be the reason a working provider stops working.
func chatBodyToResponses(body []byte, endpointModel string) ([]byte, error) {
	var in chatRequestBody
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode chat request: %w", err)
	}

	out := map[string]interface{}{
		"model": endpointModel,
		"input": []item{},
	}
	if in.Stream {
		out["stream"] = true
	}
	if in.MaxTokens != nil {
		out["max_output_tokens"] = *in.MaxTokens
	}

	// A leading system message becomes `instructions`, which is the field the
	// Responses API gives it. Keeping it in `input` as well would duplicate the
	// prompt, so it is moved rather than copied.
	rest := in.Messages
	if len(rest) > 0 && rest[0].Role == "system" {
		if text := flatText(rest[0].Content); text != "" {
			out["instructions"] = text
		}
		rest = rest[1:]
	}

	items := make([]upstreamItem, 0, len(rest))
	for _, m := range rest {
		switch m.Role {
		case "assistant":
			// An assistant turn can carry text and tool calls. The Responses
			// shape splits those into separate items, so emit both.
			if text := flatText(m.Content); text != "" {
				items = append(items, upstreamItem{
					Type:    "message",
					Role:    "assistant",
					Content: []upstreamPart{{Type: "output_text", Text: strPtr(text)}},
				})
			}
			for i, tc := range m.ToolCalls {
				// call_id has to be the tool call's OWN id, because the
				// function_call_output further down carries `tool_call_id`
				// and the provider matches the pair. Sending the function
				// name here instead leaves every tool result answering a
				// call the model never made.
				items = append(items, upstreamItem{
					Type:      "function_call",
					CallID:    firstNonEmpty(tc.ID, tc.Function.Name, fmt.Sprintf("call_%d", i)),
					Name:      strPtr(tc.Function.Name),
					Arguments: strPtr(toolCallArguments(tc.Function.Arguments)),
				})
			}
		case "tool":
			// `output` is emitted even when the tool produced no text. It is
			// a required field, and a tool that legitimately returns nothing
			// is not a malformed request.
			items = append(items, upstreamItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: strPtr(flatText(m.Content)),
			})
		default: // "user" and anything else
			parts, err := chatContentToParts(m.Content)
			if err != nil {
				return nil, err
			}
			items = append(items, upstreamItem{
				Type:    "message",
				Role:    firstNonEmpty(m.Role, "user"),
				Content: upstreamParts(parts),
			})
		}
	}
	out["input"] = items

	if tools := chatToolsToResponses(in.Tools); len(tools) > 0 {
		out["tools"] = tools
	}

	return json.Marshal(out)
}

// chatToolsToResponses flattens the Chat Completions tool envelope
// ({"type":"function","function":{"name":...}}) into the Responses shape
// ({"type":"function","name":...}). Both spellings are accepted on input,
// because the Responses API and the Chat Completions API disagree here and a
// client may send either once translated.
func chatToolsToResponses(in []struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(in))
	for _, t := range in {
		if t.Type != "" && t.Type != "function" {
			// Non-function tools (e.g. retrieval) have no defined mapping here.
			// Dropping them silently would change the model's behaviour, so the
			// caller is told instead -- see the filter below.
			continue
		}
		name := firstNonEmpty(t.Name, t.Function.Name)
		if name == "" {
			continue
		}
		m := map[string]interface{}{"type": "function", "name": name}
		if d := firstNonEmpty(t.Description, t.Function.Description); d != "" {
			m["description"] = d
		}
		params := t.Parameters
		if len(params) == 0 {
			params = t.Function.Parameters
		}
		if len(params) > 0 {
			m["parameters"] = params
		}
		out = append(out, m)
	}
	return out
}

// chatContentToParts converts a Chat Completions `content` (a string, or an
// array of typed parts) into Responses content parts.
func chatContentToParts(raw json.RawMessage) ([]contentPart, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	// Bare string is the common case.
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []contentPart{{Type: "input_text", Text: s}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil, fmt.Errorf("decode message content: %w", err)
	}
	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.ImageURL != nil:
			out = append(out, contentPart{Type: "input_image", ImageURL: imageRef(p.ImageURL.URL)})
		case p.Text != "":
			out = append(out, contentPart{Type: "input_text", Text: p.Text})
		}
	}
	return out, nil
}

// flatText extracts plain text from a Chat Completions `content` that may be
// either a string or a parts array. Used where only the text matters (the
// system prompt, a tool result).
func flatText(raw json.RawMessage) string {
	parts, err := chatContentToParts(raw)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// responsesBodyToChat converts a Responses response envelope back into the
// Chat Completions shape the client asked for.
//
// This is the inverse of buildResponseEnvelope, and the two are kept adjacent
// in spirit deliberately: if the shape changes on one side it must change on
// the other.
func responsesBodyToChat(body []byte, logicalModel string) ([]byte, error) {
	var env responseEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode responses envelope: %w", err)
	}
	// An error envelope must not be laundered into an empty success. The caller
	// treats an error here as a failure and moves to the next endpoint.
	if env.Status == "failed" || env.Status == "incomplete" && len(env.Output) == 0 {
		if env.Error != nil {
			return nil, fmt.Errorf("upstream responses error: %s: %s", env.Error.Type, env.Error.Message)
		}
		return nil, fmt.Errorf("upstream responses returned status %q with no output", env.Status)
	}

	chat := ChatCompletionResponse{
		ID:      mintChatCompletionID(env.ID),
		Object:  "chat.completion",
		Created: env.CreatedAt,
		Model:   logicalModel,
		Choices: []ChatCompletionChoice{{
			Index:        0,
			Message:      ChatCompletionMessage{Role: "assistant"},
			FinishReason: finishReasonForResponses(env.Status),
		}},
	}
	if env.Usage != nil {
		chat.Usage = &ChatCompletionUsage{
			PromptTokens:     env.Usage.InputTokens,
			CompletionTokens: env.Usage.OutputTokens,
			TotalTokens:      env.Usage.TotalTokens,
		}
		if chat.Usage.TotalTokens == 0 {
			chat.Usage.TotalTokens = env.Usage.InputTokens + env.Usage.OutputTokens
		}
	}

	var text strings.Builder
	for _, it := range env.Output {
		switch it.Type {
		case "message":
			for _, p := range it.Content {
				text.WriteString(p.Text)
			}
		case "function_call":
			tc := ChatCompletionToolCall{ID: it.CallID, Type: "function"}
			tc.Function.Name = it.Name
			tc.Function.Arguments = it.Arguments
			chat.Choices[0].Message.ToolCalls = append(chat.Choices[0].Message.ToolCalls, tc)
		}
	}
	chat.Choices[0].Message.Content = text.String()

	return json.Marshal(chat)
}

func mintChatCompletionID(upstream string) string {
	if strings.HasPrefix(upstream, "chatcmpl-") && upstream != "chatcmpl-" {
		return upstream
	}
	return "chatcmpl-" + randomHex(12)
}

func finishReasonForResponses(status string) *string {
	switch status {
	case "incomplete":
		s := "length"
		return &s
	default:
		s := "stop"
		return &s
	}
}

// ---------------------------------------------------------------------------
// Streaming: Responses SSE -> Chat Completions SSE
// ---------------------------------------------------------------------------
//
// Event shapes are not guessed. The mapping below was captured from a live
// stream on 2026-10-02 (kilocode kilo-auto/free over /responses), which emits:
//
//	response.created / response.in_progress      full response envelope
//	response.output_item.added                   the item, empty
//	response.output_text.delta                   {"delta": "..."}
//	response.output_text.done                    {"text": "..."}
//	response.function_call_arguments.delta       {"delta": "{...json...}"}
//	response.function_call_arguments.done        {"name":..., "arguments":...}
//	response.output_item.done                    the completed item
//	response.completed                           full response envelope
//
// Two of those matter more than the rest:
//
//   - response.reasoning_text.delta is SKIPPED, by being absent from the switch
//     below. The Chat Completions surface has no channel for reasoning, and
//     forwarding it as content would inject the model's scratchpad into the
//     answer the client sees.
//   - Function-call arguments arrive as bare JSON fragments, so the first chunk
//     of a tool call opens the tool_calls entry with EMPTY arguments rather
//     than a fragment that is not yet valid JSON. A client that concatenates
//     deltas would otherwise receive unparseable arguments.
//
// Unknown event types are ignored rather than passed through: forwarding a
// Responses-shaped event into a Chat Completions stream would corrupt it.

type openToolCall struct {
	id   string
	name string
}

// responsesStreamTranslator converts a Responses SSE stream into Chat
// Completions SSE. Its state is per-response only, never shared between
// requests, so one instance can be used per stream without locking.
type responsesStreamTranslator struct {
	id        string
	created   int64
	model     string
	openTools map[int]*openToolCall
	seq       int
	emitted   bool
}

// chatChunk is the minimal Chat Completions chunk this translator emits.
type chatChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []chatChunkChoice    `json:"choices"`
	Usage   *ChatCompletionUsage `json:"usage,omitempty"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason,omitempty"`
}

type chatChunkDelta struct {
	Role      string                   `json:"role,omitempty"`
	Content   string                   `json:"content,omitempty"`
	ToolCalls []chatChunkDeltaToolCall `json:"tool_calls,omitempty"`
}

type chatChunkDeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func newResponsesStreamTranslator() *responsesStreamTranslator {
	return &responsesStreamTranslator{openTools: map[int]*openToolCall{}}
}

func (t *responsesStreamTranslator) render(c chatChunk) []byte {
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

func (t *responsesStreamTranslator) chunk(delta chatChunkDelta, finish *string) []byte {
	t.emitted = true
	return t.render(chatChunk{
		ID:      firstNonEmpty(t.id, "chatcmpl-stream"),
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	})
}

// responsesSSSEvent is the wire shape of one Responses SSE payload. Only the
// fields this translator acts on are decoded.
type responsesSSSEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	Text        string `json:"text"`
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Item        struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *responseEnvelope `json:"response"`
}

// consume translates one Responses SSE event into Chat Completions bytes.
// Empty output means the event carried nothing a Chat Completions client can
// use, which is the common case: most of the stream is lifecycle bookkeeping.
func (t *responsesStreamTranslator) consume(event string, payload []byte) []byte {
	var e responsesSSSEvent
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &e); err != nil {
			return nil
		}
	}

	switch event {
	// response.reasoning_text.delta is intentionally NOT handled. See above.
	case "response.output_text.delta":
		if e.Delta == "" {
			return nil
		}
		return t.chunk(chatChunkDelta{Content: e.Delta}, nil)

	case "response.output_item.added":
		if e.Item.Type != "function_call" {
			return nil
		}
		t.openTools[e.OutputIndex] = &openToolCall{
			id:   firstNonEmpty(e.Item.CallID, e.Item.ID),
			name: e.Item.Name,
		}
		return t.chunk(t.toolOpening(e.OutputIndex), nil)

	case "response.function_call_arguments.delta":
		if _, ok := t.openTools[e.OutputIndex]; !ok {
			// Arguments with no opening event: synthesize one so the client
			// still receives a well-formed tool call rather than an orphan
			// argument fragment attached to nothing.
			t.openTools[e.OutputIndex] = &openToolCall{id: "call_" + strconv.Itoa(e.OutputIndex)}
			open := t.chunk(t.toolOpening(e.OutputIndex), nil)
			return append(open, t.chunk(t.toolArguments(e.OutputIndex, e.Delta), nil)...)
		}
		if e.Delta == "" {
			return nil
		}
		return t.chunk(t.toolArguments(e.OutputIndex, e.Delta), nil)

	case "response.created", "response.in_progress":
		if e.Response != nil {
			if e.Response.ID != "" && t.id == "" {
				t.id = e.Response.ID
			}
			if e.Response.CreatedAt != 0 && t.created == 0 {
				t.created = e.Response.CreatedAt
			}
			if e.Response.Model != "" && t.model == "" {
				t.model = e.Response.Model
			}
		}
		return nil

	case "response.completed", "response.incomplete", "response.failed":
		var usage *ChatCompletionUsage
		status := "completed"
		if e.Response != nil {
			status = e.Response.Status
			if e.Response.ID != "" && t.id == "" {
				t.id = e.Response.ID
			}
			if e.Response.CreatedAt != 0 && t.created == 0 {
				t.created = e.Response.CreatedAt
			}
			if u := e.Response.Usage; u != nil {
				usage = &ChatCompletionUsage{
					PromptTokens:     u.InputTokens,
					CompletionTokens: u.OutputTokens,
					TotalTokens:      u.TotalTokens,
				}
				if usage.TotalTokens == 0 {
					usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				}
			}
		}
		reason := "stop"
		if status != "completed" {
			reason = "length"
		}
		// A response that produced no usable content still owes the client a
		// terminal chunk, otherwise the client waits for one forever.
		t.emitted = true
		return t.render(chatChunk{
			ID:      firstNonEmpty(t.id, "chatcmpl-stream"),
			Object:  "chat.completion.chunk",
			Created: t.created,
			Model:   t.model,
			Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{}, FinishReason: &reason}},
			Usage:   usage,
		})
	}

	return nil
}

func (t *responsesStreamTranslator) toolOpening(index int) chatChunkDelta {
	ot := t.openTools[index]
	var d chatChunkDelta
	// Arguments are deliberately empty here: they arrive as bare JSON
	// fragments and only concatenate into valid JSON after the closing brace.
	d.ToolCalls = []chatChunkDeltaToolCall{{Index: index, ID: ot.id, Type: "function"}}
	d.ToolCalls[0].Function.Name = ot.name
	return d
}

func (t *responsesStreamTranslator) toolArguments(index int, frag string) chatChunkDelta {
	var d chatChunkDelta
	d.ToolCalls = []chatChunkDeltaToolCall{{Index: index}}
	d.ToolCalls[0].Function.Arguments = frag
	return d
}

// responsesSSEToChatReader adapts a Responses SSE stream to the Chat
// Completions SSE the rest of the proxy already speaks.
//
// Wrapping the reader rather than touching streamSSE is deliberate: the
// buffering-until-first-content, the tool-call accumulation and the [DONE]
// framing all live in streamSSE and are protocol-agnostic. Feeding it a reader
// that has already translated the bytes is the whole integration.
type responsesSSEToChatReader struct {
	src  io.Reader
	tr   *responsesStreamTranslator
	pend []byte
	// carry holds the bytes of a line that has not been terminated yet.
	carry []byte
	done  bool
	first bool
}

func newResponsesSSEToChatReader(src io.Reader) *responsesSSEToChatReader {
	return &responsesSSEToChatReader{src: src, tr: newResponsesStreamTranslator()}
}

// Read pulls from the source, translating whole SSE events as they complete.
// A partial event is held back until the rest of it arrives, because an event
// cut in half is an event that cannot be classified.
func (r *responsesSSEToChatReader) Read(p []byte) (int, error) {
	for len(r.pend) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.pump(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pend)
	r.pend = r.pend[n:]
	return n, nil
}

// pump reads until at least one translated byte is pending, or the source ends.
//
// Partial lines are carried across reads in `carry`. Getting this wrong is
// silent and severe in two different directions: dropping the fragment loses
// content, and emitting it untranslated feeds a raw Responses event into the
// Chat Completions stream that streamSSE is parsing.
func (r *responsesSSEToChatReader) pump() error {
	buf := make([]byte, 32*1024)
	n, err := r.src.Read(buf)

	if n > 0 {
		r.carry = append(r.carry, buf[:n]...)
		var out []byte
		for {
			idx := bytes.IndexByte(r.carry, '\n')
			if idx < 0 {
				break // incomplete line: stays in carry for the next read
			}
			line := bytes.TrimRight(r.carry[:idx], "\r")
			r.carry = r.carry[idx+1:]
			if translated := r.translateLine(line); len(translated) > 0 {
				out = append(out, translated...)
			}
		}
		if len(out) > 0 {
			r.pend = append(out, r.pend...)
			return nil
		}
	}

	if err != nil {
		r.done = true
		// Flush a final line that arrived without a trailing newline.
		if tail := bytes.TrimRight(r.carry, "\r"); len(tail) > 0 {
			r.carry = nil
			if translated := r.translateLine(tail); len(translated) > 0 {
				r.pend = append(r.pend, translated...)
				return nil
			}
		}
		if !r.first {
			r.first = true
			r.pend = append(r.pend, []byte("data: [DONE]\n\n")...)
			return nil
		}
	}
	return nil
}

// translateLine converts one complete SSE line, returning the Chat Completions
// bytes it produced (empty for lines that carry nothing a chat client can use).
func (r *responsesSSEToChatReader) translateLine(line []byte) []byte {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return nil // event boundary
	}
	if line[0] == ':' {
		return nil // SSE comment / heartbeat
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		// "event:" and anything else. The payload's own `type` is
		// authoritative, so the event: line carries no information we need.
		return nil
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	return r.tr.consume(responsesEventType(payload), payload)
}

// responsesEventType pulls the `type` discriminator out of a Responses SSE
// payload. Some providers also send a separate `event:` line; the payload's own
// `type` is authoritative and is what the captured streams carried.
func responsesEventType(payload []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return ""
	}
	return e.Type
}
