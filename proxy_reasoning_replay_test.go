package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamSSECollectsReasoningForReplay: Command Code's thinking models
// reject a replayed assistant turn that does not echo its reasoning
// ("Missing required parameter: 'messages[N].reasoning_content'"), so the
// stream has to hand the gateway the thinking it forwarded to the client.
// Both spellings in the wild are read; an object-valued field is ignored
// rather than failing the whole event, which would drop the content and tool
// calls that share it.
func TestStreamSSECollectsReasoningForReplay(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning\":\"more\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning\":{\"opaque\":true},\"content\":\"answer\"}}]}\n\n" +
		"data: [DONE]\n\n"

	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	var reasoning strings.Builder
	acc, _, _, err := p.streamSSE(flusher, flusher, strings.NewReader(input), "", &reasoning)
	if err != nil {
		t.Fatalf("streamSSE: %v", err)
	}
	if acc != "answer" {
		t.Fatalf("content = %q, want %q", acc, "answer")
	}
	if got := reasoning.String(); got != "think more" {
		t.Fatalf("reasoning = %q, want %q", got, "think more")
	}

	body := appendAssistantMessage([]byte(`{"model":"work","messages":[]}`), acc, nil, reasoning.String())
	var parsed struct {
		Messages []struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("replayed body is not JSON: %v\n%s", err, body)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(parsed.Messages))
	}
	m := parsed.Messages[0]
	if m.Role != "assistant" || m.Content != "answer" || m.ReasoningContent != "think more" {
		t.Fatalf("replayed message = %+v", m)
	}
}

// TestAppendAssistantMessageOmitsEmptyReasoning keeps the replay unchanged for
// every ordinary failover: only an attempt that actually thought gets the
// field, so non-reasoning models are not handed a field they never produced.
func TestAppendAssistantMessageOmitsEmptyReasoning(t *testing.T) {
	body := appendAssistantMessage([]byte(`{"model":"work","messages":[]}`), "answer", nil, "")
	if bytes.Contains(body, []byte("reasoning_content")) {
		t.Fatalf("empty reasoning must not be written:\n%s", body)
	}
}
