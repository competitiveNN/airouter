package main

import (
	"bytes"
	"testing"
)

// FuzzSSEEventParsing drives arbitrary SSE event streams through the internal
// processEvent/sseEventIsRelease logic.
func FuzzSSEEventParsing(f *testing.F) {
	seeds := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n",
		"data: [DONE]\n\n",
		": keepalive\n\n",
		"\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]",
		"data: {not json at all\n\n",
		"event: error\ndata: {\"error\":\"something\"}\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rawEvent string) {
		raw := []byte(rawEvent)
		isRelease := sseEventIsRelease(raw)
		_ = isRelease

		payload := sseDataPayload(raw)
		if bytes.Equal(payload, []byte("[DONE]")) {
			if !isRelease {
				t.Fatalf("sseEventIsRelease returned false for [DONE] sentinel: %q", rawEvent)
			}
		}

		_ = sseDataPayload(raw)
		_ = ExtractSSEError(raw)

		if isRelease != sseEventIsRelease(raw) {
			t.Fatalf("sseEventIsRelease is non-deterministic for input %q", rawEvent)
		}
	})
}
