package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Responses streaming event names, per the Open Responses spec. The gateway
// emits these in place of the Chat Completions `chat.completion.chunk` shape
// the upstream produces.
const (
	evResponseCreated       = "response.created"
	evResponseInProgress    = "response.in_progress"
	evOutputItemAdded       = "response.output_item.added"
	evOutputTextDelta       = "response.output_text.delta"
	evOutputTextDone        = "response.output_text.done"
	evOutputItemDone        = "response.output_item.done"
	evContentPartAdded      = "response.content_part.added"
	evContentPartDone       = "response.content_part.done"
	evResponseCompleted     = "response.completed"
	evResponseIncomplete    = "response.incomplete"
	evResponseFailed        = "response.failed"
	evError                 = "error"
	evFunctionCallArgsDelta = "response.function_call_arguments.delta"
	evFunctionCallArgsDone  = "response.function_call_arguments.done"
)

// responsesStreamWriter translates an upstream Chat Completions SSE stream
// into Responses events on the client connection.
//
// The upstream is a Chat Completions endpoint, so its deltas arrive as
// `choices[].delta.content` and `choices[].delta.tool_calls`. We emit a
// lifecycle envelope (response.created -> output_item.added ->
// content_part.added -> deltas -> done -> output_item.done ->
// response.completed) around the text we forward.
//
// Error handling mirrors the chat stream path: the first real token is
// buffered, so an error that arrives before any content produces no client
// output and the caller can fall back to the next model with nothing
// half-written.
type responsesStreamWriter struct {
	w        io.Writer
	flusher  flushWriter
	respID   string
	model    string
	created  int64
	seq      int
	msgID    string
	itemOpen bool
	sawText  bool
	acc      strings.Builder
}

// flushWriter is the subset of http.Flusher the writer needs, so tests can
// pass a stub.
type flushWriter interface{ Flush() }

// newResponsesStreamWriter starts a Responses stream: it writes the
// response.created event and the response.in_progress event, so the client can
// attach deltas to a known response from the first token.
//
// Lifecycle events are emitted here, once. Callers that buffer an attempt
// construct the writer up front and must NOT open a second one per attempt,
// or the client sees two response.created events for one response.
func newResponsesStreamWriter(w io.Writer, flusher flushWriter, logicalModel string) *responsesStreamWriter {
	s := &responsesStreamWriter{
		w:       w,
		flusher: flusher,
		respID:  responseIDPrefix + newCorrelationID(""),
		model:   logicalModel,
		created: time.Now().Unix(),
	}
	s.emit(evResponseCreated, map[string]any{
		"response": s.skeleton("in_progress"),
	})
	s.emit(evResponseInProgress, map[string]any{
		"response": s.skeleton("in_progress"),
	})
	return s
}

// skeleton builds the response object embedded in lifecycle events. It
// carries just enough for a client to correlate events; the terminal event
// carries the full output.
func (s *responsesStreamWriter) skeleton(status string) map[string]any {
	return map[string]any{
		"id":         s.respID,
		"object":     "response",
		"created_at": s.created,
		"status":     status,
		"model":      s.model,
		"output":     []item{},
	}
}

// emit writes one Responses SSE event. The event name goes in the `event:`
// field and the payload in `data:`, matching the spec's framing; a `sequence_number`
// is included so a client can detect gaps.
func (s *responsesStreamWriter) emit(name string, payload map[string]any) {
	payload["type"] = name
	payload["sequence_number"] = s.seq
	s.seq++
	blob, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, blob)
	s.flusher.Flush()
}

// openTextItem lazily opens the assistant message + its text part. It is
// called on the first text delta so a tool-call-only turn never opens an
// empty message item.
func (s *responsesStreamWriter) openTextItem() {
	if s.itemOpen {
		return
	}
	s.itemOpen = true
	msgID := s.itemID()
	s.emit(evOutputItemAdded, map[string]any{
		"output_index": 0,
		"item": item{
			Type:    "message",
			ID:      msgID,
			Role:    "assistant",
			Content: []contentPart{},
		},
	})
	s.emit(evContentPartAdded, map[string]any{
		"item_id":    msgID,
		"part":       contentPart{Type: "output_text"},
		"part_index": 0,
	})
}

// textDelta forwards one text increment as a Responses delta event.
func (s *responsesStreamWriter) textDelta(text string) {
	if text == "" {
		return
	}
	s.openTextItem()
	s.sawText = true
	s.acc.WriteString(text)
	s.emit(evOutputTextDelta, map[string]any{
		"item_id": s.itemID(),
		"delta":   text,
	})
}

// itemID returns the id of the open message item. It is stable for the life
// of the stream; the first call mints it.
func (s *responsesStreamWriter) itemID() string {
	if s.msgID == "" {
		s.msgID = newCorrelationID("msg")
	}
	return s.msgID
}

// finish emits the terminal events. finishReason is the upstream
// Chat Completions finish reason; "length" maps to response.incomplete,
// anything else to response.completed. The client always gets a terminal
// event so its parser does not hang.
func (s *responsesStreamWriter) finish(finishReason string, toolCalls []streamToolCall) {
	output := []item{}
	if s.sawText {
		text := s.acc.String()
		msgID := s.itemID()
		s.emit(evOutputTextDone, map[string]any{
			"item_id": msgID,
			"text":    text,
		})
		s.emit(evContentPartDone, map[string]any{
			"item_id":    msgID,
			"part_index": 0,
			"part":       contentPart{Type: "output_text", Text: text},
		})
		done := item{
			Type:    "message",
			ID:      msgID,
			Role:    "assistant",
			Content: []contentPart{{Type: "output_text", Text: text}},
		}
		s.emit(evOutputItemDone, map[string]any{"output_index": 0, "item": done})
		output = append(output, done)
	}
	// Tool calls are announced then completed, mirroring the message item
	// lifecycle. The id and call_id are minted once and reused across the
	// added/args-done/done events so a client can correlate them. Output
	// indices start at 1 when a message item occupies index 0, else at 0.
	base := 0
	if s.sawText {
		base = 1
	}
	for i, tc := range toolCalls {
		itemID := newCorrelationID("fc")
		callID := firstNonEmpty(tc.ID, fmt.Sprintf("call_%d", i))
		fc := item{
			Type:      "function_call",
			ID:        itemID,
			CallID:    callID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		}
		s.emit(evOutputItemAdded, map[string]any{
			"output_index": base + i,
			"item":         fc,
		})
		s.emit(evFunctionCallArgsDone, map[string]any{
			"item_id":   itemID,
			"call_id":   callID,
			"name":      tc.Name,
			"arguments": tc.Arguments,
		})
		s.emit(evOutputItemDone, map[string]any{"output_index": base + i, "item": fc})
		output = append(output, fc)
	}

	status := "completed"
	event := evResponseCompleted
	if finishReason == "length" {
		status = "incomplete"
		event = evResponseIncomplete
	}
	s.emit(event, map[string]any{
		"response": map[string]any{
			"id":         s.respID,
			"object":     "response",
			"created_at": s.created,
			"status":     status,
			"model":      s.model,
			"output":     output,
		},
	})
}

// fail emits a response.failed event. Used when the upstream errors after the
// stream opened, where a bare SSE error would leave the client unable to
// correlate the failure with a response.
func (s *responsesStreamWriter) fail(code, message, errType string) {
	s.emit(evResponseFailed, map[string]any{
		"response": map[string]any{
			"id":         s.respID,
			"object":     "response",
			"created_at": s.created,
			"status":     "failed",
			"model":      s.model,
			"output":     []item{},
			"error":      &respError{Code: code, Message: message, Type: errType},
		},
	})
}

// translateChatSSE reads a Chat Completions SSE stream and re-emits it as
// Responses events on sw. It returns the accumulated text, the accumulated
// tool calls, and any error.
//
// The writer is supplied by the caller rather than created here, because the
// caller buffers each attempt in order to decide whether to replay it. The
// lifecycle events (response.created / response.in_progress) have already been
// written by newResponsesStreamWriter before this is called.
func translateChatSSE(r io.Reader, sw *responsesStreamWriter) (string, []streamToolCall, error) {
	var buffered bytes.Buffer
	released := false
	var acc strings.Builder
	tcs := []streamToolCall{}

	release := func() {
		if released {
			return
		}
		released = true
		if buffered.Len() > 0 {
			// Re-interpret the buffered prefix as a text delta.
			sw.textDelta(buffered.String())
			buffered.Reset()
		}
	}

	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != 'd' {
			continue
		}
		raw := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(raw) == 0 {
			continue
		}
		if bytes.Equal(raw, []byte("[DONE]")) {
			release()
			finish := lastFinishReason(tcs)
			sw.finish(finish, tcs)
			return acc.String(), tcs, nil
		}

		// An upstream SSE error event is a real failure, not a parse hiccup.
		// It must abort the attempt so the caller can fall back to the next
		// model: silently ignoring it would surface an empty response.completed
		// to the client, i.e. a successful-looking but contentless answer,
		// which hides the upstream error entirely.
		if code, msg, isErr := parseSSEError(raw); isErr {
			return acc.String(), tcs, &ProviderError{
				StatusCode: statusForStreamError(code),
				Body:       []byte(msg),
			}
		}

		// ChatCompletionStreamDelta has no ToolCalls field (the chat path
		// parses them from an anonymous struct in accumulateDelta), so we
		// decode into the same anonymous shape here.
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if d := choice.Delta.Content; d != "" {
			if !released {
				buffered.WriteString(d)
				continue
			}
			acc.WriteString(d)
			sw.textDelta(d)
		}
		for _, tc := range choice.Delta.ToolCalls {
			accumulateResponsesToolCall(&tcs, tc.Index, tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		return acc.String(), tcs, err
	}
	release()
	sw.finish(lastFinishReason(tcs), tcs)
	return acc.String(), tcs, nil
}

// parseSSEError reports whether an SSE data payload is an upstream error
// event, and returns its code and message. It tolerates the several shapes
// providers use: a top-level {"error":{...}}, or a bare {"message":...,
// "type":...} with no choices.
func parseSSEError(raw []byte) (code, message string, ok bool) {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", "", false
	}
	if probe.Error != nil {
		return firstNonEmpty(probe.Error.Code, probe.Error.Type), probe.Error.Message, true
	}
	// Some providers emit the error fields at the top level with no choices
	// and no error wrapper.
	if len(probe.Choices) == 0 && bytes.Contains(raw, []byte(`"message"`)) {
		var flat struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(raw, &flat); err == nil && flat.Message != "" {
			return firstNonEmpty(flat.Code, flat.Type), flat.Message, true
		}
	}
	return "", "", false
}

// statusForStreamError maps an upstream error code to an HTTP-ish status so
// the normal cooldown escalation applies to a mid-stream failure.
func statusForStreamError(code string) int {
	switch strings.ToLower(code) {
	case "rate_limit_exceeded", "rate_limit_error":
		return 429
	case "insufficient_quota":
		return 429
	case "server_error", "internal_server_error":
		return 500
	case "overloaded_error":
		return 503
	default:
		// 502: the upstream spoke SSE but not with anything we can use.
		return 502
	}
}

// lastFinishReason derives a finish reason from the accumulated tool calls:
// a turn that produced tool calls ends with "tool_calls", otherwise "stop".
func lastFinishReason(tcs []streamToolCall) string {
	if len(tcs) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// accumulateResponsesToolCall folds one Chat Completions tool_call delta into
// the running accumulator, mirroring the chat path's accumulation so a tool
// call split across many deltas arrives whole. Deltas are matched by their
// index field, not by slice position, because providers may skip or reorder
// them.
func accumulateResponsesToolCall(tcs *[]streamToolCall, idx int, id, typ, name, args string) {
	var target *streamToolCall
	for i := range *tcs {
		if (*tcs)[i].Index == idx {
			target = &(*tcs)[i]
			break
		}
	}
	if target == nil {
		*tcs = append(*tcs, streamToolCall{Index: idx})
		target = &(*tcs)[len(*tcs)-1]
	}
	if id != "" {
		target.ID = id
	}
	if typ != "" {
		target.Type = typ
	}
	if name != "" {
		target.Name = name
	}
	if args != "" {
		target.Arguments += args
	}
}
