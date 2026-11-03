package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNormalizeToolCallIndicesAddsTheMissingIndex pins the compatibility fix
// for Gemini's complete-tool-call chunk: the OpenAI schema requires an index
// and maki (a strict parser) drops the whole chunk without it, which is how a
// turn ends with the stream simply stopping.
func TestNormalizeToolCallIndicesAddsTheMissingIndex(t *testing.T) {
	raw := []byte(`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"extra_content":{"google":{"thought_signature":"sig"}},"function":{"arguments":"{\"command\":\"git status\"}","name":"bash"},"id":"call_1","type":"function"}]},"index":0}],"model":"gemini-flash-lite-latest","object":"chat.completion.chunk"}` + "\n")

	out := normalizeToolCallIndices(raw)
	if string(out) == string(raw) {
		t.Fatal("the index-less tool call was returned unchanged")
	}
	payload := sseDataPayload(out)
	var obj struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    *int           `json:"index"`
					ID       string         `json:"id"`
					Type     string         `json:"type"`
					Extra    map[string]any `json:"extra_content"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		t.Fatalf("rewritten chunk is not valid JSON: %v", err)
	}
	tc := obj.Choices[0].Delta.ToolCalls[0]
	if tc.Index == nil || *tc.Index != 0 {
		t.Fatalf("index = %v, want 0", tc.Index)
	}
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "bash" {
		t.Fatalf("tool call fields were rewritten: %+v", tc)
	}
	if !strings.Contains(string(payload), "thought_signature") {
		t.Fatal("extra_content was dropped")
	}
}

func TestNormalizeToolCallIndicesLeavesGoodChunksAlone(t *testing.T) {
	withIndex := []byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}` + "\n")
	if got := normalizeToolCallIndices(withIndex); string(got) != string(withIndex) {
		t.Fatalf("a chunk with indices was rewritten:\n%s", got)
	}

	plain := []byte(`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n")
	if got := normalizeToolCallIndices(plain); string(got) != string(plain) {
		t.Fatalf("a content chunk was rewritten:\n%s", got)
	}
}
