package main

import (
	"encoding/json"
	"testing"
)

// TestCCConvertMessageAlwaysEchoesReasoning pins the Command Code requirement
// that every earlier assistant turn carries reasoning: the live endpoint
// rejects a conversation without it ("Missing required parameter:
// 'messages[N].reasoning_content'"), so a message whose client never got any
// reasoning gets a placeholder rather than failing the whole request.
func TestCCConvertMessageAlwaysEchoesReasoning(t *testing.T) {
	cases := []struct {
		name      string
		msg       ccClientMessage
		want      string
		wantParts int
	}{
		{
			name:      "real reasoning is echoed",
			msg:       ccClientMessage{Role: "assistant", Content: json.RawMessage(`"hi"`), ReasoningContent: "because"},
			want:      "because",
			wantParts: 2,
		},
		{
			name:      "missing reasoning gets the placeholder",
			msg:       ccClientMessage{Role: "assistant", Content: json.RawMessage(`"hi"`)},
			want:      ccMissingReasoning,
			wantParts: 2,
		},
		{
			name: "a reasoning part inside content is preferred over the placeholder",
			msg: ccClientMessage{
				Role:    "assistant",
				Content: json.RawMessage(`[{"type":"reasoning","text":"old"},{"type":"text","text":"hi"}]`),
			},
			want:      "old",
			wantParts: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := ccConvertMessage(tc.msg, nil)
			parts, _ := out["content"].([]any)
			if len(parts) != tc.wantParts {
				t.Fatalf("parts = %d (%v), want %d", len(parts), parts, tc.wantParts)
			}
			first, _ := parts[0].(map[string]any)
			if first["type"] != "reasoning" {
				t.Fatalf("first part = %v, want a reasoning part", first)
			}
			if first["text"] != tc.want {
				t.Fatalf("reasoning text = %v, want %q", first["text"], tc.want)
			}
		})
	}
}
