package main

import (
	"encoding/json"
	"testing"
)

// FuzzPromptParsing drives arbitrary messages/content structures through prompt
// extraction and message marshaling helpers, asserting zero panics and robust
// type assertion.
func FuzzPromptParsing(f *testing.F) {
	seeds := []string{
		`{"model":"smart","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"work","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`,
		`{"model":"fast","messages":[{"role":"assistant","content":"world","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		`{"messages":null}`,
		`{"messages":"not an array"}`,
		`{}`,
		`invalid json`,
		`{"input":"hello","model":"smart"}`,
		`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"model":"smart"}`,
		`{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		data := []byte(raw)

		// ChatCompletionMessage: unmarshal + marshal + unmarshal round trip.
		var msg ChatCompletionMessage
		if err := json.Unmarshal(data, &msg); err == nil {
			enc, merr := msg.MarshalJSON()
			if merr != nil {
				t.Fatalf("MarshalJSON failed: %v", merr)
			}
			var msg2 ChatCompletionMessage
			_ = json.Unmarshal(enc, &msg2)
		}
		// Also try unmarshaling a single message object extracted from a body.
		var single map[string]any
		if err := json.Unmarshal(data, &single); err == nil {
			if rawMsg, ok := single["message"]; ok {
				b, _ := json.Marshal(rawMsg)
				var m2 ChatCompletionMessage
				_ = json.Unmarshal(b, &m2)
				_, _ = m2.MarshalJSON()
			}
		}

		// ChatCompletionRequest + prompt extraction helpers.
		var req ChatCompletionRequest
		_ = json.Unmarshal(data, &req)
		_ = estimateTokens(&req)
		_ = estimateTokensFromBody(data)
		_ = requestHasVision(data)

		// Session id derivation.
		_ = bodySessionID(data)

		// Chat proxy helpers.
		_, _, _, _ = chatSplitEndpoint(data)
		_ = chatResponsesBodyText(data)
		_ = chatEventText(data)
		_ = chatEventUsage(data)
		_ = chatUsageFromBody(data)
		_ = estimateChatTokens(data)

		// Responses helpers.
		var rr responsesRequest
		if err := json.Unmarshal(data, &rr); err == nil {
			_, _ = toChatMessages(&rr)
			_, _ = toChatCompletionRequest(&rr)
		}
		_, _ = parseInputItems(json.RawMessage(data))
		_, _ = chatContentToParts(json.RawMessage(data))
		_ = flatText(json.RawMessage(data))

		// imageRef normalization (bare string or {"url":...}).
		var ir imageRef
		_ = json.Unmarshal(data, &ir)
		_ = string(ir)

		// appendAssistantMessage must not panic on arbitrary bodies.
		func() {
			defer func() { recover() }()
			_ = appendAssistantMessage(data, raw, nil, raw)
		}()
	})
}
