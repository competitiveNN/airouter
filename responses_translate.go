package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toChatMessages converts a Responses request into the Chat Completions
// messages array the upstream provider understands. The conversion is
// lossy by design: Responses-only fields (reasoning, text.format,
// previous_response_id) are not representable in chat completions and are
// dropped here; they are handled by the gateway's session system instead.
//
// instructions become a leading system message; input items are flattened
// into alternating user/assistant turns; function_call items become
// assistant tool_calls; function_call_output items become a tool role
// message. The result is a valid chat-completions messages array.
func toChatMessages(req *responsesRequest) ([]ChatCompletionMessage, error) {
	var msgs []ChatCompletionMessage

	if req.Instructions != "" {
		msgs = append(msgs, ChatCompletionMessage{Role: "system", Content: req.Instructions})
	}

	items, err := parseInputItems(req.Input)
	if err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}

	var pendingToolCalls []ChatCompletionToolCall
	var pendingOutput strings.Builder

	flushAssistant := func() error {
		if len(pendingToolCalls) > 0 || pendingOutput.Len() > 0 {
			msgs = append(msgs, ChatCompletionMessage{
				Role:      "assistant",
				Content:   pendingOutput.String(),
				ToolCalls: pendingToolCalls,
			})
		}
		pendingToolCalls = nil
		pendingOutput.Reset()
		return nil
	}

	for _, it := range items {
		switch it.Type {
		case "message":
			role := it.Role
			if role == "" {
				role = "user"
			}
			if err := flushAssistant(); err != nil {
				return nil, err
			}
			// A message carrying image parts must be emitted with array
			// content so the upstream sees the multimodal form and the
			// gateway's vision detection (`"type":"image_url"`) fires. A
			// text-only message keeps the plain string form, which is what
			// every provider expects and what token estimation assumes.
			if parts := it.imageParts(); len(parts) > 0 {
				msgs = append(msgs, ChatCompletionMessage{
					Role:      role,
					Content:   itemText(it),
					ImageURLs: parts,
				})
			} else {
				msgs = append(msgs, ChatCompletionMessage{Role: role, Content: itemText(it)})
			}
		case "function_call":
			pendingToolCalls = append(pendingToolCalls, ChatCompletionToolCall{
				ID:   it.CallID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: it.Name, Arguments: it.Arguments},
			})
		case "function_call_output":
			if err := flushAssistant(); err != nil {
				return nil, err
			}
			msgs = append(msgs, ChatCompletionMessage{
				Role:    "tool",
				Content: it.Output,
				Name:    it.CallID,
			})
		case "reasoning":
			// Reasoning is not representable in chat completions; drop it
			// from the upstream messages. The gateway preserves it in the
			// Responses output envelope only.
		default:
			// Unknown item types are dropped from the chat-completions
			// translation (they may be provider extensions). They are
			// preserved in the Responses output when echoing.
		}
	}
	if err := flushAssistant(); err != nil {
		return nil, err
	}
	return msgs, nil
}

// itemText extracts the concatenated text content of a message item. Image
// parts contribute no text, so they are skipped here; item.imageParts is what
// surfaces them.
func itemText(it item) string {
	var sb strings.Builder
	for _, c := range it.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// imageParts returns the image references carried by a message item, in order.
// Both the string form ("image_url": "https://...") and the object form
// ("image_url": {"url": "..."}) are accepted, since both appear in the wild.
func (it item) imageParts() []string {
	var out []string
	for _, c := range it.Content {
		if c.ImageURL == "" {
			continue
		}
		switch c.Type {
		case "input_image", "image", "image_url", "":
			out = append(out, string(c.ImageURL))
		}
	}
	return out
}

// parseInputItems parses the Responses input field, which may be a plain
// string (treated as a single user message) or an array of items.
func parseInputItems(raw json.RawMessage) ([]item, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []item{{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: s}}}}, nil
	}
	var items []item
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// toChatCompletionRequest builds a ChatCompletionRequest from a Responses
// request, translating the input items into messages. Provider-level
// overrides (temperature, top_p, max tokens) are mapped 1:1.
func toChatCompletionRequest(req *responsesRequest) (*ChatCompletionRequest, error) {
	msgs, err := toChatMessages(req)
	if err != nil {
		return nil, err
	}
	return &ChatCompletionRequest{
		Model:            req.Model,
		Messages:         msgs,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		PresencePenalty:  req.PresencePenalty,
		FrequencyPenalty: req.FrequencyPenalty,
		MaxTokens:        req.MaxOutputTokens,
		Stream:           req.Stream,
	}, nil
}

// buildChatBody marshals a ChatCompletionRequest to JSON, preserving the
// Responses-only fields the upstream doesn't understand by dropping them
// (chat completions providers reject unknown fields).
func buildChatBody(req *ChatCompletionRequest) ([]byte, error) {
	return json.Marshal(req)
}
