package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Gemini 3.x requires a thought_signature on every functionCall part it
// replays and answers 400 INVALID_ARGUMENT when one is missing:
//
//	Function call is missing a thought_signature in functionCall parts.
//
// A routed conversation can contain tool calls made by a non-Google model, so
// there is no real signature to send. Google documents a dummy value for that
// case; the gateway must attach it or every tool-using session that fails over
// to Gemini burns its Gemini keys on a 400 and can end in a client 503
// (measured 2026-11-07).

func toolCallBody(extra string) string {
	call := `{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}` + extra + `}`
	return `{"model":"x","messages":[` +
		`{"role":"user","content":"run it"},` +
		`{"role":"assistant","tool_calls":[` + call + `]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"ok"}` +
		`]}`
}

func assistantToolCall(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, raw)
	}
	msgs := doc["messages"].([]any)
	for _, m := range msgs {
		msg := m.(map[string]any)
		if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
			return calls[0].(map[string]any)
		}
	}
	t.Fatalf("no tool calls in output: %s", raw)
	return nil
}

func TestGeminiInjectsDummySignatureOnReplayedToolCalls(t *testing.T) {
	out := sanitizeRequestBody([]byte(toolCallBody("")), "gemini")
	call := assistantToolCall(t, out)
	sig := call["extra_content"].(map[string]any)["google"].(map[string]any)["thought_signature"]
	if sig != geminiSkipThoughtSignature {
		t.Fatalf("thought_signature = %v, want %q", sig, geminiSkipThoughtSignature)
	}
}

func TestGeminiKeepsARealThoughtSignature(t *testing.T) {
	body := toolCallBody(`,"extra_content":{"google":{"thought_signature":"real-sig"}}`)
	out := sanitizeRequestBody([]byte(body), "gemini")
	call := assistantToolCall(t, out)
	sig := call["extra_content"].(map[string]any)["google"].(map[string]any)["thought_signature"]
	if sig != "real-sig" {
		t.Fatalf("a real signature was overwritten: %v", sig)
	}
	if strings.Contains(string(out), geminiSkipThoughtSignature) {
		t.Fatal("the sentinel was injected on top of a real signature")
	}
}

func TestGeminiRecognisesTheVertexSignatureNamespace(t *testing.T) {
	body := toolCallBody(`,"extra_content":{"vertex":{"thought_signature":"vertex-sig"}}`)
	out := sanitizeRequestBody([]byte(body), "gemini2")
	if strings.Contains(string(out), geminiSkipThoughtSignature) {
		t.Fatalf("the sentinel replaced a Vertex signature: %s", out)
	}
}

func TestGeminiLeavesABodyWithoutToolCallsByteIdentical(t *testing.T) {
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
	if out := sanitizeRequestBody([]byte(body), "gemini"); string(out) != body {
		t.Fatalf("a tool-call-free body was rewritten:\n got %s\nwant %s", out, body)
	}
}

func TestNonGeminiProvidersDoNotGetTheSignature(t *testing.T) {
	body := toolCallBody("")
	out := sanitizeRequestBody([]byte(body), "opencode")
	if string(out) != body {
		t.Fatalf("a non-Gemini body was rewritten: %s", out)
	}
}

func TestGeminiSignatureAndFieldStrippingCompose(t *testing.T) {
	body := `{"model":"x","thinking":{"type":"enabled"},"messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"bash","arguments":"{}"}}]}]}`
	out := sanitizeRequestBody([]byte(body), "gemini")
	if strings.Contains(string(out), `"thinking"`) {
		t.Fatalf("unsupported field survived: %s", out)
	}
	call := assistantToolCall(t, out)
	if call["extra_content"] == nil {
		t.Fatalf("signature missing after field stripping: %s", out)
	}
}
