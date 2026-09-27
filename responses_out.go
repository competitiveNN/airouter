package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// responseIDPrefix namespaces gateway-minted response ids so a client can
// tell a gateway-originated id from one the provider issued.
const responseIDPrefix = "resp_"

// mintResponseID builds a response id. The provider's own id is used when it
// looks like a real Responses id; otherwise we mint one, since the upstream
// is a Chat Completions endpoint and its id is not a Responses id at all.
func mintResponseID(upstream string) string {
	if strings.HasPrefix(upstream, responseIDPrefix) && upstream != responseIDPrefix {
		return upstream
	}
	return responseIDPrefix + newCorrelationID("chatcmpl")
}

// newCorrelationID mints a short unique id with the given stem. It reuses the
// request-id machinery the proxy already has so ids are consistent in shape
// across the two surfaces.
func newCorrelationID(stem string) string {
	if stem == "" {
		stem = "resp"
	}
	return stem + "_" + randomHex(8)
}

// buildResponseEnvelope converts an upstream Chat Completions response into
// the Responses envelope the client expects.
//
// The mapping is mechanical: the first choice's message becomes a single
// output message item, its tool calls become function_call items, usage is
// renamed, and the finish reason becomes the response status. A response with
// no choices is surfaced as status "incomplete" rather than an empty success,
// since an empty output array would look like a valid but contentless answer.
func buildResponseEnvelope(chat *ChatCompletionResponse, logicalModel string) *responseEnvelope {
	env := &responseEnvelope{
		ID:        mintResponseID(chat.ID),
		Object:    "response",
		CreatedAt: chat.Created,
		Status:    "completed",
		Model:     logicalModel,
		Output:    []item{},
	}
	if env.CreatedAt == 0 {
		env.CreatedAt = time.Now().Unix()
	}

	if chat.Usage != nil {
		env.Usage = &respUsage{
			InputTokens:  chat.Usage.PromptTokens,
			OutputTokens: chat.Usage.CompletionTokens,
			TotalTokens:  chat.Usage.TotalTokens,
		}
		// Providers occasionally report a zero total alongside non-zero parts.
		// Recompute rather than report an internally inconsistent block.
		if env.Usage.TotalTokens == 0 {
			env.Usage.TotalTokens = env.Usage.InputTokens + env.Usage.OutputTokens
		}
	}

	if len(chat.Choices) == 0 {
		env.Status = "incomplete"
		return env
	}

	choice := chat.Choices[0]
	if choice.FinishReason != nil && *choice.FinishReason == "length" {
		// The upstream ran out of budget mid-answer. "incomplete" is the
		// Responses spelling of that; the text we did get is still returned.
		env.Status = "incomplete"
	}

	if text := choice.Message.Content; text != "" {
		env.Output = append(env.Output, item{
			Type:    "message",
			ID:      newCorrelationID("msg"),
			Role:    "assistant",
			Content: []contentPart{{Type: "output_text", Text: text}},
		})
	}

	// Tool calls are separate output items, not nested in the message, per the
	// Responses spec. They are emitted even when the message text is empty,
	// which is the normal shape for a pure tool-call turn.
	for i, tc := range choice.Message.ToolCalls {
		env.Output = append(env.Output, item{
			Type:      "function_call",
			ID:        newCorrelationID("fc"),
			CallID:    firstNonEmpty(tc.ID, fmt.Sprintf("call_%d", i)),
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	return env
}

// firstNonEmpty returns s if it is non-empty, otherwise fallback.
func firstNonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// parseChatResponse decodes an upstream Chat Completions body. A body that is
// not a recognizable completion response is returned as an error so the
// caller can fall back to the next model rather than emitting an empty
// success to the client.
func parseChatResponse(body []byte) (*ChatCompletionResponse, error) {
	var chat ChatCompletionResponse
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, fmt.Errorf("decode upstream response: %w", err)
	}
	if chat.Object == "error" || (len(chat.Choices) == 0 && chat.Usage == nil && chat.ID == "") {
		return nil, fmt.Errorf("upstream returned no completion payload")
	}
	return &chat, nil
}
