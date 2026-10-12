package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"strings"
)

// PatchToolCall is a tool call extracted from a non-standard location
// (a thinking block) by a model patch.
type PatchToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// PatchStreamState is opaque per-request streaming state owned by the
// patch that created it. The framework only carries it between calls.
type PatchStreamState any

// ModelPatch rewrites one upstream model's output to work around a
// provider-side bug. Patches are hotfixes: they are registered in
// modelPatches and applied at every surface the model's output can
// reach (chat completions and responses, streaming and not). Removing
// a patch is deleting its registry entry -- every application site is
// nil-guarded, so an unregistered model is untouched.
type ModelPatch interface {
	// Matches reports whether the patch applies to an upstream model
	// id (e.g. "dots-studio/dots-3-note-preview:free").
	Matches(upstreamModel string) bool
	// BeginStream starts per-request state for one streamed response.
	BeginStream() PatchStreamState
	// TransformStreamChunk rewrites one chat-completions SSE chunk
	// payload (the JSON after "data:"), returning the cleaned payload
	// and any tool calls the chunk completed.
	TransformStreamChunk(st PatchStreamState, payload []byte) (cleaned []byte, calls []PatchToolCall)
	// FlushStream closes a stream: it returns a final chunk payload
	// carrying any text the patch still holds back (nil when nothing
	// is held) and any calls completed by flushing.
	FlushStream(st PatchStreamState) (final []byte, calls []PatchToolCall)
	// TransformCompletion rewrites a complete chat-completions JSON
	// body.
	TransformCompletion(body []byte) []byte
}

// modelPatches is the registry of model hotfixes. Add a patch here to
// enable it, delete the entry to deprecate it.
var modelPatches = []ModelPatch{&dotsFunctionCallPatch{}}

// modelPatchFor returns the patch registered for an upstream model, or
// nil when the model has none.
func modelPatchFor(upstreamModel string) ModelPatch {
	for _, p := range modelPatches {
		if p.Matches(upstreamModel) {
			return p
		}
	}
	return nil
}

// normalizePatchModel canonicalizes an upstream model id for matching:
// lowercase, and strip the provider suffix (":free", ":paid", ...).
func normalizePatchModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, ":"); i >= 0 {
		m = m[:i]
	}
	return m
}

// rewriteSSEEventData replaces the JSON payload of an SSE event's data
// line, preserving any other lines in the event. Returns raw unchanged
// when the event carries no data line.
func rewriteSSEEventData(raw, newPayload []byte) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		prefix := line[:len(line)-len(trimmed)]
		lines[i] = append(prefix, append([]byte("data: "), newPayload...)...)
		break
	}
	return bytes.Join(lines, []byte("\n"))
}

// takePendingToolCallIndex returns the smallest tool call index not
// already used in the pending map, so patch-extracted calls never
// collide with upstream tool call deltas.
func takePendingToolCallIndex(pending map[int]*pendingTC) int {
	for i := 0; ; i++ {
		if _, ok := pending[i]; !ok {
			return i
		}
	}
}

// dotsOpenTag is the opening tag the model wraps its misplaced tool
// calls in. It is matched as a prefix so the state machine can detect
// the tag while it streams in pieces.
const dotsOpenTag = "<dots_function_call"

// dotsCloseTag terminates a dots_function_call block.
const dotsCloseTag = "</dots_function_call>"

// dotsFunctionCallPatch is a hotfix for dots-studio/dots-3-note-preview,
// which sometimes emits its tool calls inside the thinking block as
// Anthropic-style XML instead of proper tool call deltas:
//
//	<dots_function_call>
//	  <invoke name="bg_status">
//	    <parameter name="read">true</parameter>
//	  </invoke>
//	</dots_function_call>
//
// When the reply is only a thinking block and an empty message, the
// client would receive an empty response and the tool call would be
// lost. The patch strips the tags from the thinking stream and
// re-emits the calls as regular tool calls, so the client executes
// them. The tags can stream in pieces, so the streaming side holds
// text back until a `<` resolves as either the tag opener or ordinary
// text.
//
// Delete the registry entry in modelPatches to deprecate this hotfix
// once the provider fixes the model.
type dotsFunctionCallPatch struct{}

func (p *dotsFunctionCallPatch) Matches(upstreamModel string) bool {
	return normalizePatchModel(upstreamModel) == "dots-studio/dots-3-note-preview"
}

func (p *dotsFunctionCallPatch) BeginStream() PatchStreamState {
	return &dotsStreamState{}
}

// dotsParsedCall is a tool call parsed from one block, before the
// framework assigns it an id.
type dotsParsedCall struct {
	name      string
	arguments string
}

// dotsFieldState is the holdback buffer for one delta text field.
// Text is emitted as soon as it can no longer be the prefix of a
// dots_function_call tag; a `<` is held until it either grows into
// the opener or is exposed as ordinary text.
type dotsFieldState struct {
	pending string // text not yet emitted
	inTag   bool   // inside an open <dots_function_call> block
	openTag string // raw opening tag of the block being scanned
}

// dotsStreamState is the per-request state of the dots patch.
type dotsStreamState struct {
	content          dotsFieldState
	reasoning        dotsFieldState
	reasoningContent dotsFieldState
	nextID           int
}

// mint assigns a stable tool call id to a parsed call.
func (s *dotsStreamState) mint(c dotsParsedCall) PatchToolCall {
	s.nextID++
	return PatchToolCall{
		ID:        fmt.Sprintf("call_dots_%d", s.nextID),
		Name:      c.name,
		Arguments: c.arguments,
	}
}

// feed consumes newly arrived text for one field, returning the text
// safe to emit and any completed tool calls.
func (f *dotsFieldState) feed(text string) (string, []dotsParsedCall) {
	f.pending += text
	var emit strings.Builder
	var calls []dotsParsedCall
	for {
		if !f.inTag {
			i := strings.Index(f.pending, "<")
			if i < 0 {
				emit.WriteString(f.pending)
				f.pending = ""
				return emit.String(), calls
			}
			emit.WriteString(f.pending[:i])
			rest := f.pending[i:]
			if strings.HasPrefix(rest, dotsOpenTag) {
				after := rest[len(dotsOpenTag):]
				if after == "" {
					// Exactly the opener so far; the next byte decides
					// whether this is a tag or ordinary text.
					f.pending = rest
					return emit.String(), calls
				}
				if !isTagBoundaryByte(after[0]) {
					// e.g. "<dots_function_callX": ordinary text.
					emit.WriteString("<")
					f.pending = rest[1:]
					continue
				}
				j := strings.Index(rest, ">")
				if j < 0 {
					// Opening tag still streaming in.
					f.pending = rest
					return emit.String(), calls
				}
				openTag := rest[:j+1]
				f.pending = rest[j+1:]
				if strings.HasSuffix(openTag, "/>") {
					// Self-closing attribute-form call.
					if c, ok := parseDotsAttrCall(openTag); ok {
						calls = append(calls, c)
					}
					continue
				}
				f.inTag = true
				f.openTag = openTag
				continue
			}
			if len(rest) < len(dotsOpenTag) && strings.HasPrefix(dotsOpenTag, rest) {
				// A prefix of the opener: it might still grow into it.
				f.pending = rest
				return emit.String(), calls
			}
			// An ordinary '<'.
			emit.WriteString("<")
			f.pending = rest[1:]
			continue
		}
		k := strings.Index(f.pending, dotsCloseTag)
		if k < 0 {
			// Block body still streaming in.
			return emit.String(), calls
		}
		inner := f.pending[:k]
		f.pending = f.pending[k+len(dotsCloseTag):]
		f.inTag = false
		calls = append(calls, parseDotsBlock(inner, f.openTag)...)
		f.openTag = ""
	}
}

// isTagBoundaryByte reports whether b can directly follow a tag name
// inside an opening angle bracket.
func isTagBoundaryByte(b byte) bool {
	switch b {
	case '>', '/', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// TransformStreamChunk rewrites one chunk payload, stripping any
// dots_function_call tags from the delta's thinking fields and
// completing any tool call whose closing tag arrived in this chunk.
func (p *dotsFunctionCallPatch) TransformStreamChunk(st PatchStreamState, payload []byte) ([]byte, []PatchToolCall) {
	state, _ := st.(*dotsStreamState)
	if state == nil {
		return payload, nil
	}
	held := state.content.pending != "" || state.content.inTag ||
		state.reasoning.pending != "" || state.reasoning.inTag ||
		state.reasoningContent.pending != "" || state.reasoningContent.inTag
	// Fast path: a tag can only start with '<', so a chunk
	// without one and with nothing held back cannot change
	// anything. The marker itself is not a usable guard: a
	// tag streaming in pieces splits the marker across
	// chunks, none of which contains it whole.
	if !held && !bytes.Contains(payload, []byte("<")) {
		return payload, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return payload, nil
	}
	choices, _ := doc["choices"].([]any)
	if len(choices) == 0 {
		return payload, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return payload, nil
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return payload, nil
	}
	content, _ := delta["content"].(string)
	reasoning, _ := delta["reasoning"].(string)
	reasoningContent, _ := delta["reasoning_content"].(string)
	emitContent, c1 := state.content.feed(content)
	emitReasoning, c2 := state.reasoning.feed(reasoning)
	emitRC, c3 := state.reasoningContent.feed(reasoningContent)
	var parsed []dotsParsedCall
	parsed = append(parsed, c1...)
	parsed = append(parsed, c2...)
	parsed = append(parsed, c3...)
	if emitContent == content && emitReasoning == reasoning && emitRC == reasoningContent && len(parsed) == 0 {
		return payload, nil
	}
	if _, ok := delta["content"]; ok || emitContent != "" {
		delta["content"] = emitContent
	}
	if _, ok := delta["reasoning"]; ok || emitReasoning != "" {
		delta["reasoning"] = emitReasoning
	}
	if _, ok := delta["reasoning_content"]; ok || emitRC != "" {
		delta["reasoning_content"] = emitRC
	}
	cleaned, err := json.Marshal(doc)
	if err != nil {
		return payload, nil
	}
	var calls []PatchToolCall
	for _, c := range parsed {
		calls = append(calls, state.mint(c))
	}
	if len(calls) > 0 {
		log.Printf("[debug] dots patch: extracted %d tool call(s) from a thinking block", len(calls))
	}
	return cleaned, calls
}

// FlushStream emits any text still held back by the streaming state
// (a `<` that never resolved, or a block whose closer never arrived)
// as a final chunk payload, so no thinking text is lost.
func (p *dotsFunctionCallPatch) FlushStream(st PatchStreamState) ([]byte, []PatchToolCall) {
	state, _ := st.(*dotsStreamState)
	if state == nil {
		return nil, nil
	}
	delta := map[string]any{}
	if state.content.pending != "" {
		delta["content"] = state.content.pending
		state.content.pending = ""
	}
	if state.reasoning.pending != "" {
		delta["reasoning"] = state.reasoning.pending
		state.reasoning.pending = ""
	}
	if state.reasoningContent.pending != "" {
		delta["reasoning_content"] = state.reasoningContent.pending
		state.reasoningContent.pending = ""
	}
	if len(delta) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": delta}},
	})
	if err != nil {
		return nil, nil
	}
	return payload, nil
}

// TransformCompletion rewrites a complete chat-completions body,
// moving any dots_function_call blocks out of the message's thinking
// and content fields and into regular tool calls.
func (p *dotsFunctionCallPatch) TransformCompletion(body []byte) []byte {
	if !bytes.Contains(body, []byte("dots_function_call")) {
		return body
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	choices, _ := doc["choices"].([]any)
	changed := false
	nextID := 0
	for _, ch := range choices {
		choice, _ := ch.(map[string]any)
		if choice == nil {
			continue
		}
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		var calls []PatchToolCall
		for _, field := range []string{"reasoning_content", "content"} {
			text, _ := msg[field].(string)
			if text == "" {
				continue
			}
			cleaned, extracted := extractDotsCalls(text)
			if len(extracted) == 0 {
				continue
			}
			msg[field] = cleaned
			changed = true
			for _, c := range extracted {
				nextID++
				calls = append(calls, PatchToolCall{
					ID:        fmt.Sprintf("call_dots_%d", nextID),
					Name:      c.name,
					Arguments: c.arguments,
				})
			}
		}
		if len(calls) == 0 {
			continue
		}
		existing, _ := msg["tool_calls"].([]any)
		for _, c := range calls {
			existing = append(existing, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Arguments,
				},
			})
		}
		msg["tool_calls"] = existing
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	log.Printf("[debug] dots patch: extracted tool call(s) from a completion thinking block")
	return out
}

// extractDotsCalls scans a complete text for dots_function_call
// blocks, returning the cleaned text and any calls found. Text that
// is not a well-formed block is preserved verbatim.
func extractDotsCalls(text string) (string, []dotsParsedCall) {
	var (
		out    strings.Builder
		calls  []dotsParsedCall
		search = text
	)
	for {
		i := strings.Index(search, dotsOpenTag)
		if i < 0 {
			out.WriteString(search)
			break
		}
		after := search[i+len(dotsOpenTag):]
		if after == "" || !isTagBoundaryByte(after[0]) {
			// A '<' that cannot start a dots_function_call tag.
			out.WriteString(search[:i+1])
			search = search[i+1:]
			continue
		}
		j := strings.Index(search[i:], ">")
		if j < 0 {
			// Unterminated opening tag: keep the rest verbatim.
			out.WriteString(search[i:])
			break
		}
		openTag := search[i : i+j+1]
		rest := search[i+j+1:]
		if strings.HasSuffix(openTag, "/>") {
			out.WriteString(search[:i])
			if c, ok := parseDotsAttrCall(openTag); ok {
				calls = append(calls, c)
			}
			search = rest
			continue
		}
		k := strings.Index(rest, dotsCloseTag)
		if k < 0 {
			// Unterminated block: keep the rest verbatim.
			out.WriteString(search[i:])
			break
		}
		inner := rest[:k]
		out.WriteString(search[:i])
		calls = append(calls, parseDotsBlock(inner, openTag)...)
		search = rest[k+len(dotsCloseTag):]
	}
	return out.String(), calls
}

// parseDotsBlock extracts the tool call(s) from the body of one
// dots_function_call block. openTag is the block's raw opening tag,
// used only by the attribute-form fallback.
func parseDotsBlock(inner, openTag string) []dotsParsedCall {
	// Primary form, observed live:
	//   <invoke name="bg_status"><parameter name="read">true</parameter></invoke>
	var block struct {
		Invokes []struct {
			Name       string `xml:"name,attr"`
			Parameters []struct {
				Name string `xml:"name,attr"`
				Text string `xml:",chardata"`
			} `xml:"parameter"`
		} `xml:"invoke"`
	}
	if err := xml.Unmarshal([]byte("<root>"+inner+"</root>"), &block); err == nil {
		var calls []dotsParsedCall
		for _, inv := range block.Invokes {
			if inv.Name == "" {
				continue
			}
			args := make(map[string]string, len(inv.Parameters))
			for _, prm := range inv.Parameters {
				if prm.Name != "" {
					args[prm.Name] = strings.TrimSpace(prm.Text)
				}
			}
			if b, err := json.Marshal(args); err == nil {
				calls = append(calls, dotsParsedCall{name: inv.Name, arguments: string(b)})
			}
		}
		if len(calls) > 0 {
			return calls
		}
	}
	// Fallbacks for other shapes the model might emit: a JSON body,
	// then attributes on the opening tag.
	if c, ok := parseDotsJSONCall(inner); ok {
		return []dotsParsedCall{c}
	}
	if c, ok := parseDotsAttrCall(openTag); ok {
		return []dotsParsedCall{c}
	}
	return nil
}

// parseDotsJSONCall parses a JSON body:
// {"name": "...", "arguments": {...}} (optionally nested under
// "function"). Arguments may be a JSON value or a pre-encoded string.
func parseDotsJSONCall(inner string) (dotsParsedCall, bool) {
	trimmed := strings.TrimSpace(inner)
	if !strings.HasPrefix(trimmed, "{") {
		return dotsParsedCall{}, false
	}
	var obj struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Function  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return dotsParsedCall{}, false
	}
	name := obj.Name
	rawArgs := obj.Arguments
	if name == "" {
		name = obj.Function.Name
		rawArgs = obj.Function.Arguments
	}
	if name == "" {
		return dotsParsedCall{}, false
	}
	args := normalizeDotsArguments(rawArgs)
	if args == "" {
		args = "{}"
	}
	return dotsParsedCall{name: name, arguments: args}, true
}

// normalizeDotsArguments accepts an arguments value that is either a
// JSON value (returned compactly) or a pre-encoded JSON string
// (returned verbatim).
func normalizeDotsArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// parseDotsAttrCall parses the attribute form of the opening tag:
// <dots_function_call name="x" arguments='{"a":1}'>.
func parseDotsAttrCall(openTag string) (dotsParsedCall, bool) {
	tag := strings.TrimSpace(openTag)
	switch {
	case strings.HasSuffix(tag, "/>"):
		tag = tag[:len(tag)-2] + "></dots_function_call>"
	case strings.HasSuffix(tag, ">"):
		tag = tag + "</dots_function_call>"
	default:
		return dotsParsedCall{}, false
	}
	var el struct {
		Name      string `xml:"name,attr"`
		Arguments string `xml:"arguments,attr"`
		Args      string `xml:"args,attr"`
	}
	if err := xml.Unmarshal([]byte(tag), &el); err != nil {
		return dotsParsedCall{}, false
	}
	raw := el.Arguments
	if raw == "" {
		raw = el.Args
	}
	if el.Name == "" || raw == "" {
		return dotsParsedCall{}, false
	}
	args := normalizeDotsArguments(json.RawMessage(raw))
	if args == "" {
		args = "{}"
	}
	return dotsParsedCall{name: el.Name, arguments: args}, true
}
