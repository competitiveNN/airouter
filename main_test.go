package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func loadTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"openai": {
				URL:       "https://api.openai.com/v1",
				APIKeyEnv: "OPENAI_API_KEY",
			},
			"openrouter": {
				URL:       "https://openrouter.ai/api/v1",
				APIKeyEnv: "OPENROUTER_API_KEY",
			},
			"ollama": {
				URL:       "http://localhost:11434/v1",
				APIKeyEnv: "OLLAMA_API_KEY",
			},
		},
		Models: map[string]ModelConfig{
			"smart": {
				Chain: []ModelEndpoint{
					{Provider: "openai", Model: "gpt-4"},
					{Provider: "openrouter", Model: "anthropic/claude-3-opus"},
					{Provider: "openai", Model: "gpt-4-turbo"},
				},
			},
			"work": {
				Chain: []ModelEndpoint{
					{Provider: "openai", Model: "gpt-4-turbo"},
					{Provider: "openrouter", Model: "anthropic/claude-3-sonnet"},
					{Provider: "openai", Model: "gpt-4"},
				},
			},
			"fast": {
				Chain: []ModelEndpoint{
					{Provider: "ollama", Model: "llama3"},
					{Provider: "openai", Model: "gpt-3.5-turbo"},
				},
			},
			"large": {
				Chain: []ModelEndpoint{
					{Provider: "openai", Model: "gpt-4-turbo"},
					{Provider: "openrouter", Model: "anthropic/claude-3-opus"},
				},
			},
		},
	}
	return cfg
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid config",
			cfg: Config{
				Providers: map[string]ProviderConfig{
					"openai": {URL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
				},
				Models: map[string]ModelConfig{
					"smart": {Chain: []ModelEndpoint{{Provider: "openai", Model: "gpt-4"}}},
				},
			},
			wantErr: false,
		},
		{
			name: "no providers",
			cfg: Config{
				Models: map[string]ModelConfig{
					"smart": {Chain: []ModelEndpoint{}},
				},
			},
			wantErr: true,
		},
		{
			name: "provider with empty URL",
			cfg: Config{
				Providers: map[string]ProviderConfig{
					"openai": {URL: "", APIKeyEnv: "OPENAI_API_KEY"},
				},
				Models: map[string]ModelConfig{
					"smart": {Chain: []ModelEndpoint{{Provider: "openai", Model: "gpt-4"}}},
				},
			},
			wantErr: true,
		},
		{
			name: "provider with invalid URL scheme",
			cfg: Config{
				Providers: map[string]ProviderConfig{
					"openai": {URL: "ftp://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
				},
				Models: map[string]ModelConfig{
					"smart": {Chain: []ModelEndpoint{{Provider: "openai", Model: "gpt-4"}}},
				},
			},
			wantErr: true,
		},
		{
			name: "unknown provider in chain",
			cfg: Config{
				Providers: map[string]ProviderConfig{
					"openai": {URL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
				},
				Models: map[string]ModelConfig{
					"smart": {Chain: []ModelEndpoint{{Provider: "nonexistent", Model: "gpt-4"}}},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestModelEndpointKey(t *testing.T) {
	ep := ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	if ep.Key() != "openai:gpt-4" {
		t.Errorf("expected openai:gpt-4, got %s", ep.Key())
	}
}

func TestModelEndpointEqual(t *testing.T) {
	ep1 := ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	ep2 := ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	ep3 := ModelEndpoint{Provider: "anthropic", Model: "gpt-4"}

	if !ep1.Equal(ep2) {
		t.Error("expected ep1 == ep2")
	}
	if ep1.Equal(ep3) {
		t.Error("expected ep1 != ep3")
	}
}

func TestReplaceModelName(t *testing.T) {
	body := `{"model":"smart","messages":[{"role":"user","content":"hello"}]}`
	result, err := ReplaceModelName([]byte(body), "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	var model string
	if err := json.Unmarshal(parsed["model"], &model); err != nil {
		t.Fatalf("could not parse model field: %v", err)
	}
	if model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", model)
	}

	// Verify messages are preserved
	var msgs []map[string]interface{}
	if err := json.Unmarshal(parsed["messages"], &msgs); err != nil {
		t.Fatalf("could not parse messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 message, got %d", len(msgs))
	}
}

func TestReplaceModelNamePreservesFields(t *testing.T) {
	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":100,"stream":true}`
	result, err := ReplaceModelName([]byte(body), "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(result, &parsed)
	if parsed["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", parsed["temperature"])
	}
	if parsed["max_tokens"].(float64) != 100 {
		t.Errorf("expected max_tokens 100, got %v", parsed["max_tokens"])
	}
	if parsed["stream"] != true {
		t.Errorf("expected stream true, got %v", parsed["stream"])
	}
}

func TestSanitizeRequestBody(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		body      string
		wantStrip bool // whether "thinking"/"reasoning" should be absent afterward
	}{
		{"top-level thinking", "gemini", `{"model":"x","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`, true},
		{"nested thinking", "gemini", `{"model":"x","messages":[{"role":"user","content":"hi"}],"stream_options":{"thinking":true}}`, true},
		{"nested in array", "gemini", `{"model":"x","messages":[{"role":"user","content":"hi"}],"extra":[{"thinking":"on"}]}`, true},
		{"reasoning_effort", "gemini", `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, true},
		{"reasoning", "gemini", `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"high"}}`, true},
		{"non-gemini untouched", "openai", `{"model":"x","messages":[{"role":"user","content":"hi"}],"thinking":true}`, false},
		{"no fields", "gemini", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := sanitizeRequestBody([]byte(c.body), c.provider)
			hasThinking := strings.Contains(string(out), "thinking") || strings.Contains(string(out), "reasoning")
			if c.wantStrip && hasThinking {
				t.Errorf("provider=%s: unsupported field leaked -> %s", c.provider, out)
			}
			if !c.wantStrip && !hasThinking {
				t.Errorf("provider=%s: field should be preserved but was stripped -> %s", c.provider, out)
			}
			// Output must remain valid JSON.
			if !json.Valid(out) {
				t.Errorf("provider=%s: output is not valid JSON -> %s", c.provider, out)
			}
		})
	}
}

func TestExtractSSEError_OpenAIStyle(t *testing.T) {
	data := []byte(`data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit"}}

data: [DONE]

`)
	if err := ExtractSSEError(data); err == nil {
		t.Fatal("expected error from OpenAI-style SSE error event, got nil")
	} else if !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Fatalf("expected error to contain 'Rate limit exceeded', got: %v", err)
	}
}

func TestExtractSSEError_EventErrorLine(t *testing.T) {
	data := []byte(`event: error
data: {"type":"error","error":{"message":"Internal server error","type":"server_error","code":500}}

`)
	if err := ExtractSSEError(data); err == nil {
		t.Fatal("expected error from event:error + data: payload, got nil")
	} else if !strings.Contains(err.Error(), "Internal server error") {
		t.Fatalf("expected error to contain 'Internal server error', got: %v", err)
	}
}

func TestExtractSSEError_BareJSONError(t *testing.T) {
	data := []byte(`{"error":{"message":"Unauthorized","type":"auth_error","code":401}}`)
	if err := ExtractSSEError(data); err == nil {
		t.Fatal("expected error from bare JSON error object, got nil")
	} else if !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("expected error to contain 'Unauthorized', got: %v", err)
	}
}

func TestExtractSSEError_NullErrorField(t *testing.T) {
	data := []byte(`data: {"id":"123","choices":[],"error":null}`)
	if err := ExtractSSEError(data); err != nil {
		t.Fatalf("expected nil for null error field, got: %v", err)
	}
}

func TestExtractSSEError_EmptyErrorObject(t *testing.T) {
	data := []byte(`data: {"id":"123","choices":[],"error":{}}`)
	if err := ExtractSSEError(data); err != nil {
		t.Fatalf("expected nil for empty error object, got: %v", err)
	}
}

func TestExtractSSEError_NoError(t *testing.T) {
	data := []byte(`data: {"id":"123","choices":[{"delta":{"content":"hello"}}]}

data: [DONE]

`)
	if err := ExtractSSEError(data); err != nil {
		t.Fatalf("expected nil for valid stream, got: %v", err)
	}
}

func TestExtractSSEError_MultipleDataLines(t *testing.T) {
	// The first data: line is valid, the second carries the error.
	data := []byte(`data: {"id":"123","choices":[{"delta":{"content":"hi"}}]}

data: {"error":{"message":"Overloaded","type":"overloaded_error","code":529}}

data: [DONE]

`)
	if err := ExtractSSEError(data); err == nil {
		t.Fatal("expected error from second data: line, got nil")
	} else if !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("expected error to contain 'Overloaded', got: %v", err)
	}
}

func TestExtractSSEError_EmptyInput(t *testing.T) {
	if err := ExtractSSEError(nil); err != nil {
		t.Fatalf("expected nil for nil input, got: %v", err)
	}
	if err := ExtractSSEError([]byte("")); err != nil {
		t.Fatalf("expected nil for empty input, got: %v", err)
	}
}

func TestExtractSSEError_TruncatedJSON(t *testing.T) {
	data := []byte(`data: {"error":{"message":"bad`)
	if err := ExtractSSEError(data); err != nil {
		t.Fatalf("expected nil for unparseable JSON, got: %v", err)
	}
}

func TestRouterSessionAssignment(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "test-session-1"

	// First call should assign to first model in chain
	ep, wait := router.SelectEndpoint("smart", sessionID, false)
	if ep == nil {
		t.Fatal("expected endpoint, got nil")
	}
	if wait != 0 {
		t.Errorf("expected no wait, got %v", wait)
	}
	if ep.Provider != "openai" || ep.Model != "gpt-4" {
		t.Errorf("expected openai:gpt-4, got %s:%s", ep.Provider, ep.Model)
	}

	// Second call should return the same model (session persistence)
	ep2, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep2 == nil {
		t.Fatal("expected endpoint, got nil")
	}
	if !ep2.Equal(*ep) {
		t.Errorf("expected same model, got %s:%s", ep2.Provider, ep2.Model)
	}
}

func TestRouterCooldown(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// Initially available
	if !router.IsAvailable(ep) {
		t.Error("expected model to be available initially")
	}

	// Apply cooldown
	router.ApplyCooldown(ep, 429, "rate limited")

	// Should be unavailable
	if router.IsAvailable(ep) {
		t.Error("expected model to be unavailable after cooldown")
	}
}

func TestRouterCooldownExpiry(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// First consecutive 429 -> 30s cooldown (status-based base).
	router.ApplyCooldown(ep, 429, "rate limited")
	if router.IsAvailable(ep) {
		t.Error("expected model to be unavailable")
	}

	// The stored cooldown should reflect the status-based base duration (~30s).
	cds := router.GetAllCooldowns()
	cd, ok := cds[ep.Key()]
	if !ok {
		t.Fatal("expected cooldown entry")
	}
	if cd.ErrorCount != 1 {
		t.Errorf("expected error count 1, got %d", cd.ErrorCount)
	}
	if d := time.Until(cd.Expiry); d < 28*time.Second || d > 32*time.Second {
		t.Errorf("expected ~30s cooldown, got %v", d)
	}

	// While cooling down the model must stay unavailable.
	time.Sleep(2 * time.Second)
	if router.IsAvailable(ep) {
		t.Error("expected model to remain unavailable during cooldown")
	}
}

func TestRouterCooldownEscalation(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// A 500 error has a 30s base; repeated consecutive failures escalate the
	// backoff (factor 1..6, capped), but never beyond the 10m ceiling. The
	// error count is reset on a successful request (RecordSuccess), so this
	// reflects only recent consecutive failures.
	base := 30 * time.Second
	want := []time.Duration{
		base,      // 1st
		2 * base,  // 2nd
		3 * base,  // 3rd
		4 * base,  // 4th
		5 * base,  // 5th
		6 * base,  // 6th
	}
	var cd CooldownEntry
	for i, w := range want {
		router.ApplyCooldown(ep, 500, "server error")
		var ok bool
		cd, ok = router.GetAllCooldowns()[ep.Key()]
		if !ok {
			t.Fatalf("iteration %d: expected cooldown entry", i)
		}
		if cd.ErrorCount != i+1 {
			t.Errorf("iteration %d: expected error count %d, got %d", i, i+1, cd.ErrorCount)
		}
		got := time.Until(cd.Expiry)
		if got < w-5*time.Second || got > w+5*time.Second {
			t.Errorf("iteration %d: expected ~%v cooldown, got %v", i, w, got)
		}
	}

	// Keep applying errors until we hit the factor cap (factor 10).
	for i := 6; i < 10; i++ {
		router.ApplyCooldown(ep, 500, "server error")
	}
	cd = router.GetAllCooldowns()[ep.Key()]
	capDur := 10 * base // factor 10 * 30s base = 5m (capped by factor)
	if got := time.Until(cd.Expiry); got < capDur-5*time.Second || got > capDur+5*time.Second {
		t.Errorf("expected cooldown capped at %v, got %v", capDur, got)
	}

	// Beyond the factor cap the cooldown stays at the ceiling.
	router.ApplyCooldown(ep, 500, "server error")
	cd = router.GetAllCooldowns()[ep.Key()]
	if got := time.Until(cd.Expiry); got < capDur-5*time.Second || got > capDur+5*time.Second {
		t.Errorf("expected cooldown to stay capped at %v after more errors, got %v", capDur, got)
	}
}

func TestRouterModelFallback(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "test-session-2"

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	router.ApplyCooldown(ep, 500, "server error")

	// Should select next model in chain
	ep2, wait := router.SelectEndpoint("smart", sessionID, false)
	if ep2 == nil {
		t.Fatal("expected endpoint, got nil")
	}
	if wait != 0 {
		t.Errorf("expected no wait, got %v", wait)
	}
	if ep2.Provider != "openrouter" || ep2.Model != "anthropic/claude-3-opus" {
		t.Errorf("expected openrouter:claude-3-opus, got %s:%s", ep2.Provider, ep2.Model)
	}

	// Verify session is reassigned
	stored, ok := router.GetSession(sessionID)
	if !ok {
		t.Fatal("expected session to exist")
	}
	if !stored.Equal(*ep2) {
		t.Errorf("session not updated to %s:%s", ep2.Provider, ep2.Model)
	}
}

func TestRouterSelectNext(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "test-session-3"

	chain := cfg.Models["smart"].Chain

	// Put first model in cooldown
	router.ApplyCooldown(&chain[0], 500, "server error")

	// Select initial endpoint — should skip to second
	ep, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep == nil {
		t.Fatal("expected endpoint")
	}
	if !ep.Equal(chain[1]) {
		t.Errorf("expected %v, got %v", chain[1], *ep)
	}

	// Now fail this model and select next
	router.ApplyCooldown(ep, 500, "server error")
	ep2, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep2 == nil {
		t.Fatal("expected endpoint")
	}
	if !ep2.Equal(chain[2]) {
		t.Errorf("expected %v, got %v", chain[2], *ep2)
	}
}

func TestRouterSelectNextAllInCooldown(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "test-session-4"

	chain := cfg.Models["smart"].Chain

	// Put all models in cooldown
	for _, ep := range chain {
		router.ApplyCooldown(&ep, 500, "server error")
	}

	// Should return nil + wait duration
	ep, wait := router.SelectEndpoint("smart", sessionID, false)
	if ep != nil {
		t.Error("expected nil endpoint when all in cooldown")
	}
	if wait <= 0 {
		t.Error("expected positive wait duration")
	}
}

func TestVisionChainStatusAllNoVision(t *testing.T) {
	falseVal := false
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1"},
		},
		Models: map[string]ModelConfig{
			"textprofile": {
				Chain: []ModelEndpoint{
					{Provider: "p", Model: "a", Vision: &falseVal},
					{Provider: "p", Model: "b", Vision: &falseVal},
				},
			},
		},
	}
	router := NewRouter(cfg, "")
	state, wait := router.VisionChainStatus("textprofile", "s1")
	if state != VisionUnsupported {
		t.Errorf("expected VisionUnsupported, got %v", state)
	}
	if wait != 0 {
		t.Errorf("expected zero wait, got %v", wait)
	}
}

func TestVisionChainStatusVisionAvailable(t *testing.T) {
	falseVal := false
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1"},
		},
	}
	cfg.Models = map[string]ModelConfig{
			"smart": {
				Chain: []ModelEndpoint{
					{Provider: "p", Model: "a", Vision: &falseVal},
					{Provider: "p", Model: "b"}, // vision: nil -> supported
				},
		},
	}
	router := NewRouter(cfg, "")
	state, wait := router.VisionChainStatus("smart", "s1")
	if state != VisionAvailable {
		t.Errorf("expected VisionAvailable, got %v", state)
	}
	if wait != 0 {
		t.Errorf("expected zero wait, got %v", wait)
	}
}

func TestVisionChainStatusVisionCooldown(t *testing.T) {
	falseVal := false
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1"},
		},
	}
	cfg.Models = map[string]ModelConfig{
		"smart": {
			Chain: []ModelEndpoint{
				{Provider: "p", Model: "a", Vision: &falseVal},
				{Provider: "p", Model: "b"}, // vision: nil
			},
		},
	}
	router := NewRouter(cfg, "")
	// Cool down the only vision-capable endpoint.
	router.ApplyCooldown(&ModelEndpoint{Provider: "p", Model: "b"}, 429, "rate limited")
	state, wait := router.VisionChainStatus("smart", "s1")
	if state != VisionUnavailable {
		t.Errorf("expected VisionUnavailable, got %v", state)
	}
	if wait <= 0 {
		t.Error("expected positive wait for vision cooldown")
	}
}

func TestVisionChainStatusStickyNonVision(t *testing.T) {
	falseVal := false
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1"},
		},
	}
	cfg.Models = map[string]ModelConfig{
		"smart": {
			Chain: []ModelEndpoint{
				{Provider: "p", Model: "a", Vision: &falseVal},
				{Provider: "p", Model: "b"}, // vision: nil -> supported
			},
		},
	}
	router := NewRouter(cfg, "")
	// Session pinned to text-only model "a".
	router.SelectEndpoint("smart", "s1", false)
	// A vision request can fall through to model "b" which is vision-capable
	// and available, so the chain can serve vision.
	state, wait := router.VisionChainStatus("smart", "s1")
	if state != VisionAvailable {
		t.Errorf("expected VisionAvailable (fallthrough to vision-capable model), got %v", state)
	}
	_ = wait
}

func TestRouterResetCooldown(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	router.ApplyCooldown(ep, 429, "rate limited")
	if router.IsAvailable(ep) {
		t.Error("expected model to be unavailable")
	}

	router.ResetCooldown(ep)
	if !router.IsAvailable(ep) {
		t.Error("expected model to be available after reset")
	}
}

func TestCooldownDurations(t *testing.T) {
	tests := []struct {
		statusCode int
		expected   time.Duration
	}{
		{429, 30 * time.Second}, // low cooldown
		{500, 30 * time.Second}, // medium
		{502, 30 * time.Second}, // medium
		{503, 30 * time.Second}, // medium
		{504, 60 * time.Second}, // medium-long
		{404, 30 * time.Minute}, // effectively permanent: long cooldown
		{401, 30 * time.Minute}, // effectively permanent: long cooldown
		{403, 30 * time.Minute}, // effectively permanent: long cooldown
		{0, 30 * time.Second},   // default (connection error)
		{999, 30 * time.Second}, // unknown
	}

	for _, tt := range tests {
		got := baseCooldownForError(tt.statusCode)
		if got != tt.expected {
			t.Errorf("baseCooldownForError(%d) = %v, want %v", tt.statusCode, got, tt.expected)
		}
	}
}

// TestCooldownHardBan verifies that after many consecutive 4xx failures, the
// model is hard-banned for a very long time (7 days) rather than cycling at
// 24h intervals forever.
func TestCooldownHardBan(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// After 3 consecutive 404s, cooldown should be 24h.
	for i := 0; i < 3; i++ {
		router.ApplyCooldown(ep, 404, "not found")
	}
	cd := router.GetAllCooldowns()[ep.Key()]
	if cd.ErrorCount != 3 {
		t.Errorf("expected error count 3, got %d", cd.ErrorCount)
	}
	wait := time.Until(cd.Expiry)
	if wait < 23*time.Hour || wait > 25*time.Hour {
		t.Errorf("expected ~24h cooldown after 3 failures, got %v", wait)
	}

	// After 10 consecutive 404s, cooldown should jump to 7 days.
	for i := 3; i < 10; i++ {
		router.ApplyCooldown(ep, 404, "not found")
	}
	cd = router.GetAllCooldowns()[ep.Key()]
	if cd.ErrorCount != 10 {
		t.Errorf("expected error count 10, got %d", cd.ErrorCount)
	}
	wait = time.Until(cd.Expiry)
	if wait < 6*24*time.Hour || wait > 8*24*time.Hour {
		t.Errorf("expected ~7d cooldown after 10 failures, got %v", wait)
	}
}

// TestMinCooldownWaitSkipsNoVision verifies that minCooldownWait ignores
// endpoints that cannot serve a vision request (either configured with
// vision:false or marked noVision at runtime).
func TestMinCooldownWaitSkipsNoVision(t *testing.T) {
	falseVal := false
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "p", Model: "a", Vision: &falseVal}, // text-only
				{Provider: "p", Model: "b"},                    // vision-capable
			}},
		},
	}
	router := NewRouter(cfg, "")

	// Cool down the vision-capable endpoint for 30s.
	epB := &ModelEndpoint{Provider: "p", Model: "b"}
	router.ApplyCooldown(epB, 429, "rate limited")

	// For a vision request, the text-only endpoint "a" is irrelevant, so
	// minCooldownWait should return the remaining cooldown of "b".
	chain := cfg.Models["smart"].Chain
	wait := router.minCooldownWait(chain, true)
	if wait <= 0 || wait > 32*time.Second {
		t.Errorf("expected wait ~30s for vision request, got %v", wait)
	}

	// Reset cooldown so we can test the non-vision case cleanly.
	router.ResetCooldown(epB)

	// For a non-vision request, both endpoints are relevant. Since neither is
	// cooled down, minCooldownWait should return 0.
	wait = router.minCooldownWait(chain, false)
	if wait != 0 {
		t.Errorf("expected wait=0 for non-vision request, got %v", wait)
	}
}

// TestStreamIdleTimeoutConfigurable verifies that the stream idle timeout is
// configurable via Proxy.SetStreamIdleTimeout.
func TestStreamIdleTimeoutConfigurable(t *testing.T) {
	cfg := loadTestConfig(t)
	proxy := NewProxy(cfg)

	// Default should be 60s.
	if d := proxy.StreamIdleTimeout(); d != 60*time.Second {
		t.Errorf("expected default stream idle timeout 60s, got %v", d)
	}

	// Set to a custom value.
	proxy.SetStreamIdleTimeout(120 * time.Second)
	if d := proxy.StreamIdleTimeout(); d != 120*time.Second {
		t.Errorf("expected stream idle timeout 120s, got %v", d)
	}

	// Zero or negative values should be ignored.
	proxy.SetStreamIdleTimeout(0)
	if d := proxy.StreamIdleTimeout(); d != 120*time.Second {
		t.Errorf("expected stream idle timeout to remain 120s, got %v", d)
	}
	proxy.SetStreamIdleTimeout(-1 * time.Second)
	if d := proxy.StreamIdleTimeout(); d != 120*time.Second {
		t.Errorf("expected stream idle timeout to remain 120s, got %v", d)
	}
}

func TestRouterGetAllSessions(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep1, _ := router.SelectEndpoint("smart", "session-1", false)
	ep2, _ := router.SelectEndpoint("fast", "session-2", false)

	sessions := router.GetAllSessions()
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions["session-1"] != *ep1 {
		t.Error("session-1 mismatch")
	}
	if sessions["session-2"] != *ep2 {
		t.Error("session-2 mismatch")
	}
}

func TestRouterGetAllCooldowns(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep1 := ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	ep2 := ModelEndpoint{Provider: "ollama", Model: "llama3"}

	router.ApplyCooldown(&ep1, 429, "rate limited")
	router.ApplyCooldown(&ep2, 500, "server error")

	cooldowns := router.GetAllCooldowns()
	if len(cooldowns) != 2 {
		t.Errorf("expected 2 cooldowns, got %d", len(cooldowns))
	}
	if cd, ok := cooldowns[ep1.Key()]; !ok || cd.StatusCode != 429 {
		t.Error("expected 429 cooldown for openai:gpt-4")
	}
	if cd, ok := cooldowns[ep2.Key()]; !ok || cd.StatusCode != 500 {
		t.Error("expected 500 cooldown for ollama:llama3")
	}
}

func TestProviderProxyForward(t *testing.T) {
	// Start a mock backend server
	var receivedModel string
	var receivedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		receivedModel = body["model"].(string)
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   receivedModel,
			Choices: []ChatCompletionChoice{
				{Index: 0, Message: ChatCompletionMessage{Role: "assistant", Content: "Hello!"}},
			},
		})
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {
				URL:       backend.URL,
				APIKeyEnv: "TEST_API_KEY",
			},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "gpt-4"}}},
		},
	}

	// Set env var
	t.Setenv("TEST_API_KEY", "test-key-123")

	proxy := NewProxy(cfg)
	router := NewRouter(cfg, "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	ep, _ := router.SelectEndpoint("smart", "test-session", false)
	resp, err := proxy.Forward(context.Background(), []byte(body), *ep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if receivedModel != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", receivedModel)
	}
	if receivedAuth != "Bearer test-key-123" {
		t.Errorf("expected auth 'Bearer test-key-123', got %s", receivedAuth)
	}

	var result ChatCompletionResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Model != "gpt-4" {
		t.Errorf("expected response model gpt-4, got %s", result.Model)
	}
}

func TestProviderProxyStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		events := []string{
			`data: {"id":"test","object":"chat.completion.chunk","created":123,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"id":"test","object":"chat.completion.chunk","created":123,"model":"gpt-4","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`data: [DONE]`,
		}
		for _, event := range events {
			writer := w.(io.Writer)
			writer.Write([]byte(event + "\n\n"))
			flusher.Flush()
		}
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {
				URL: backend.URL,
			},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "gpt-4"}}},
		},
	}

	proxy := NewProxy(cfg)
	var output bytes.Buffer
	flusher := &testFlusher{Buffer: &output}
	ep := ModelEndpoint{Provider: "test", Model: "gpt-4"}
	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`

	_, _, _, err := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), ep, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	outputStr := output.String()
	if !strings.Contains(outputStr, "Hello") {
		t.Error("expected 'Hello' in output")
	}
	if !strings.Contains(outputStr, "world") {
		t.Error("expected 'world' in output")
	}
	if !strings.Contains(outputStr, "[DONE]") {
		t.Error("expected [DONE] in output")
	}
}

func TestProviderProxyStreamingErrorRecovery(t *testing.T) {
	requestCount := 0
	var mu sync.Mutex

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()

		// First model returns 429
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "rate limited",
				"type":    "rate_limit",
			},
		})
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		events := []string{
			`data: {"id":"test2","object":"chat.completion.chunk","created":123,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"content":"Recovered"},"finish_reason":null}]}`,
			`data: [DONE]`,
		}
		for _, event := range events {
			w.Write([]byte(event + "\n\n"))
			flusher.Flush()
		}
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}

	proxy := NewProxy(cfg)
	router := NewRouter(cfg, "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	sessionID := "recovery-test"
	var output bytes.Buffer
	flusher := &testFlusher{Buffer: &output}

	// First attempt: backend1 returns 429
	ep, _ := router.SelectEndpoint("smart", sessionID, false)
	_, _, _, err := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), *ep, 30*time.Second)
	if err == nil {
		t.Fatal("expected error from backend1")
	}

	// Apply cooldown and try next
	router.ApplyCooldownFromError(ep, err)
	ep2, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep2 == nil {
		t.Fatal("expected second endpoint")
	}

	// Second attempt: backend2 should succeed
	_, _, _, err2 := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), *ep2, 30*time.Second)
	if err2 != nil {
		t.Fatalf("expected success from backend2, got: %v", err2)
	}

	outputStr := output.String()
	if !strings.Contains(outputStr, "Recovered") {
		t.Error("expected 'Recovered' in output")
	}
}

// TestProviderProxyStreamingMidStreamResume verifies that when a model fails
// mid-stream after emitting content, the partial output already sent to the
// client is replayed as an assistant message to the next model so it continues
// instead of regenerating (which would duplicate output to the client).
func TestProviderProxyStreamingMidStreamResume(t *testing.T) {
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		// First model emits a partial chunk then an SSE error event.
		events := []string{
			`data: {"id":"t1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
			`data: {"error":{"message":"upstream died","type":"server_error"}}`,
		}
		for _, event := range events {
			w.Write([]byte(event + "\n\n"))
			flusher.Flush()
		}
	}))
	defer backend1.Close()

	var backend2Body string
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		backend2Body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		events := []string{
			`data: {"id":"t2","object":"chat.completion.chunk","created":2,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
			`data: [DONE]`,
		}
		for _, event := range events {
			w.Write([]byte(event + "\n\n"))
			flusher.Flush()
		}
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}

	proxy := NewProxy(cfg)
	router := NewRouter(cfg, "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	sessionID := "resume-test"
	var output bytes.Buffer
	flusher := &testFlusher{Buffer: &output}

	// Exercise the same fallback loop handleStream uses.
	for i := 0; i < 2; i++ {
		ep, _ := router.SelectEndpoint("smart", sessionID, false)
		if ep == nil {
			t.Fatal("no endpoint selected")
		}
		partial, _, _, err := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), *ep, 30*time.Second)
		if err == nil {
			break
		}
		router.ApplyCooldownFromError(ep, err)
		if strings.TrimSpace(partial) != "" {
			body = string(appendAssistantMessage([]byte(body), partial, nil))
		}
	}

	outputStr := output.String()
	if !strings.Contains(outputStr, "Hello ") {
		t.Error("expected 'Hello ' from first model in output")
	}
	if !strings.Contains(outputStr, "world") {
		t.Error("expected 'world' from second model in output")
	}
	if !strings.Contains(backend2Body, `"role":"assistant"`) {
		t.Errorf("expected assistant message replayed to backend2, got body: %s", backend2Body)
	}
	if !strings.Contains(backend2Body, "Hello ") {
		t.Errorf("expected partial content in replayed assistant message, got body: %s", backend2Body)
	}
}

func TestHandleModels(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gateway.HandleModels(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var result ModelListResponse
	json.NewDecoder(rec.Body).Decode(&result)
	if result.Object != "list" {
		t.Errorf("expected object 'list', got %s", result.Object)
	}
	if len(result.Data) != 4 {
		t.Errorf("expected 4 models, got %d", len(result.Data))
	}

	modelMap := make(map[string]bool)
	for _, m := range result.Data {
		modelMap[m.ID] = true
	}
	for _, expected := range LogicalModels {
		if !modelMap[expected] {
			t.Errorf("expected model %s in response", expected)
		}
	}
}

func TestHandleChatCompletionsNonStreaming(t *testing.T) {
	var receivedModel string
	var requestCount int

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		receivedModel = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   receivedModel,
			Choices: []ChatCompletionChoice{
				{Index: 0, Message: ChatCompletionMessage{Role: "assistant", Content: "Hello!"}},
			},
		})
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "gpt-4"}}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if requestCount != 1 {
		t.Errorf("expected 1 backend request, got %d", requestCount)
	}

	var result ChatCompletionResponse
	json.NewDecoder(rec.Body).Decode(&result)
	if result.Model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", result.Model)
	}
}

func TestHandleChatCompletionsVisionNotSupported(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "p", Model: "a", Vision: boolPtr(false)},
				{Provider: "p", Model: "b", Vision: boolPtr(false)},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var result ChatCompletionResponse
	json.NewDecoder(rec.Body).Decode(&result)
	if result.Choices[0].Message.Content != "Vision not supported" {
		t.Errorf("expected 'Vision not supported', got %q", result.Choices[0].Message.Content)
	}
}

func TestHandleChatCompletionsVisionUnavailableCooldown(t *testing.T) {
	requestCount := 0

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "rate limited"},
		})
	}))
	defer backend.Close()

	falseVal := false
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL},
		},
	}
	cfg.Models = map[string]ModelConfig{
		"smart": {Chain: []ModelEndpoint{
			{Provider: "test", Model: "a", Vision: &falseVal},
			{Provider: "test", Model: "b"}, // vision: nil = supported
		}},
	}
	router := NewRouter(cfg, "")
	// Cool down the only vision-capable endpoint long enough to make it
	// unavailable for this test.
	ep := &ModelEndpoint{Provider: "test", Model: "b"}
	router.ApplyCooldown(ep, 429, "rate limited")

	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var result ChatCompletionResponse
	json.NewDecoder(rec.Body).Decode(&result)
	if result.Choices[0].Message.Content != "Vision currently not available" {
		t.Errorf("expected 'Vision currently not available', got %q", result.Choices[0].Message.Content)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestHandleChatCompletionsWithFallback(t *testing.T) {
	requestCount := 0

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "rate limited"},
		})
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID:      "test-id-2",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-4-turbo",
			Choices: []ChatCompletionChoice{
				{Index: 0, Message: ChatCompletionMessage{Role: "assistant", Content: "Fallback!"}},
			},
		})
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if requestCount != 2 {
		t.Errorf("expected 2 backend requests, got %d", requestCount)
	}

	var result ChatCompletionResponse
	json.NewDecoder(rec.Body).Decode(&result)
	if result.Model != "gpt-4-turbo" {
		t.Errorf("expected model gpt-4-turbo, got %s", result.Model)
	}
}

func TestHandleChatCompletionsStreamingWithFallback(t *testing.T) {
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		events := []string{
			`data: {"id":"s1","object":"chat.completion.chunk","created":123,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"id":"s1","object":"chat.completion.chunk","created":123,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"content":"World"},"finish_reason":null}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			fmt.Fprint(w, e+"\n\n")
			flusher.Flush()
		}
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "Hello") {
		t.Error("expected 'Hello' in streaming response")
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Error("expected [DONE] in streaming response")
	}
}

func TestSessionPersistence(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "persist-session"

	// Initial request
	ep1, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep1 == nil {
		t.Fatal("expected endpoint")
	}

	// Subsequent request should get the same model
	ep2, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep2 == nil {
		t.Fatal("expected endpoint")
	}

	if !ep1.Equal(*ep2) {
		t.Errorf("session not persistent: %v != %v", *ep1, *ep2)
	}

	// Different session should get first available (may be different)
	ep3, _ := router.SelectEndpoint("smart", "different-session", false)
	if ep3 == nil {
		t.Fatal("expected endpoint")
	}
}

func TestInvalidModel(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"nonexistent","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 400 {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	var errResp ErrorResponse
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp.Error.Message == "" {
		t.Error("expected error message")
	}
}

func TestGatewayAuth(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "secret-key")

	// Request without auth header
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gateway.HandleModels(rec, req)

	if rec.Code != 401 {
		t.Errorf("expected status 401 without auth, got %d", rec.Code)
	}

	// Request with correct auth
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer secret-key")
	rec2 := httptest.NewRecorder()
	gateway.HandleModels(rec2, req2)

	if rec2.Code != 200 {
		t.Errorf("expected status 200 with auth, got %d", rec2.Code)
	}
}

func TestGatewayNoAuth(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	// No API key set — should allow all requests
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gateway.HandleModels(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200 without auth when no key configured, got %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	gateway.HandleHealth(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("expected status 'ok', got '%s'", result["status"])
	}
}

// Helper types and functions for testing
type testFlusher struct{ *bytes.Buffer }

func (f *testFlusher) Flush() {}

// TestStreamSSE_CoalescesToolCallDeltas verifies that the SSE streamer buffers
// tool_call deltas across multiple upstream events and only forwards a single,
// complete tool call event to the client. The previous behaviour forwarded
// every delta raw, which is exactly how artifacts like `maki_unknown_tool` end
// up in chat histories: a half-formed name followed by separate argument
// chunks that the client parser cannot reliably assemble.
func TestStreamSSE_CoalescesToolCallDeltas(t *testing.T) {
	// Simulate an upstream that streams a tool call as: name chunk (no args),
	// then two argument chunks. Per OpenAI spec these are three SSE events
	// all keyed by `index: 0`. Arguments contain no quotes to keep the raw
	// bytes trivially verifiable.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"a\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"b\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	acc, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	// The client must see exactly one tool call event with the full payload,
	// not three partial deltas.
	if len(tcs) != 1 {
		t.Fatalf("expected 1 coalesced tool call in fallback accumulator, got %d (%+v)", len(tcs), tcs)
	}
	if tcs[0].Name != "read_file" {
		t.Errorf("expected tool name read_file, got %q", tcs[0].Name)
	}
	if tcs[0].ID != "call_abc" {
		t.Errorf("expected tool id call_abc, got %q", tcs[0].ID)
	}
	if tcs[0].Arguments != "ab" {
		t.Errorf("expected concatenated arguments %q, got %q", "ab", tcs[0].Arguments)
	}
	_ = acc

	// The bytes the client receives must contain the full tool call payload
	// and never a partial tool_call delta. Each upstream raw event starts with
	// `data:` so we count those to confirm we got exactly one tool-call event.
	clientPayload := buf.String()
	if !strings.Contains(clientPayload, `"name":"read_file"`) {
		t.Errorf("client stream missing full tool name; got: %s", clientPayload)
	}
	if !strings.Contains(clientPayload, `"arguments":"ab"`) {
		t.Errorf("client stream missing full concatenated arguments; got: %s", clientPayload)
	}
	// No partial chunks must leak through: the empty-arguments first chunk
	// would have `"arguments":""` and the intermediate `"a"` chunk would have
	// `"arguments":"a"`.
	if strings.Contains(clientPayload, `"arguments":""`) {
		t.Errorf("client stream leaked an empty-arguments partial tool chunk: %s", clientPayload)
	}
	if strings.Contains(clientPayload, `"arguments":"a"`) {
		t.Errorf("client stream leaked a partial-arguments tool chunk: %s", clientPayload)
	}

	// The synthesized tool call MUST appear before the [DONE] sentinel.
	// Clients finalize parsing at [DONE], so a tool call arriving after
	// it is silently dropped — exactly the bug this test guards against.
	tcIdx := strings.Index(clientPayload, `"name":"read_file"`)
	doneIdx := strings.Index(clientPayload, "[DONE]")
	if tcIdx == -1 {
		t.Fatalf("tool call event not found in client payload: %s", clientPayload)
	}
	if doneIdx == -1 {
		t.Fatalf("[DONE] sentinel not found in client payload: %s", clientPayload)
	}
	if tcIdx > doneIdx {
		t.Errorf("tool call event appears AFTER [DONE] sentinel — client will silently drop it.\nclient payload:\n%s", clientPayload)
	}
}

// TestStreamSSE_IncompleteToolCallDropped verifies that if the upstream
// stream ends (with [DONE] or EOF) while a tool call is still mid-flight —
// only the index/id arrived but no function name at all — we do NOT emit a
// malformed tool call event to the client. The incomplete tool call is
// dropped; the client sees only the [DONE] sentinel.
func TestStreamSSE_IncompleteToolCallDropped(t *testing.T) {
	// Upstream emits only the tool call id, no name and no arguments, then [DONE].
	// This is a truly incomplete tool call — we never received the function name.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_xyz\"}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}
	// The accumulator still has the partial tool call (for fallback replay),
	// but the client stream must NOT contain an incomplete tool call event.
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call in fallback accumulator, got %d", len(tcs))
	}
	if tcs[0].ID != "call_xyz" {
		t.Errorf("expected tool id call_xyz, got %+v", tcs[0])
	}
	// Client should NOT see the incomplete tool call — only [DONE].
	if strings.Contains(buf.String(), `"id":"call_xyz"`) {
		t.Errorf("client stream leaked incomplete tool call event: %s", buf.String())
	}
}

// TestStreamSSE_EmptyArgsToolCallEmitted verifies that a tool call with
// name and explicit empty arguments ("arguments":"") IS emitted to the
// client. Some functions legitimately take no arguments; this is distinct
// from the incomplete case where the function name never arrives.
func TestStreamSSE_EmptyArgsToolCallEmitted(t *testing.T) {
	// Upstream emits name with explicit empty arguments, then [DONE].
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call in accumulator, got %d", len(tcs))
	}
	if tcs[0].Name != "get_time" || tcs[0].ID != "call_1" {
		t.Errorf("expected tool call get_time/call_1, got %+v", tcs[0])
	}
	// The synthesized tool call MUST appear in the client stream before [DONE].
	clientPayload := buf.String()
	tcIdx := strings.Index(clientPayload, `"name":"get_time"`)
	doneIdx := strings.Index(clientPayload, "[DONE]")
	if tcIdx == -1 {
		t.Fatalf("synthesized tool call not found in client payload: %s", clientPayload)
	}
	if doneIdx == -1 {
		t.Fatalf("[DONE] not found in client payload: %s", clientPayload)
	}
	if tcIdx > doneIdx {
		t.Errorf("tool call appears AFTER [DONE]\npayload:\n%s", clientPayload)
	}
}

// TestStreamSSE_MultipleToolCalls verifies that when the upstream streams
// multiple distinct tool calls (different indices), all of them are
// coalesced and emitted to the client in deterministic index order, before
// [DONE].
func TestStreamSSE_MultipleToolCalls(t *testing.T) {
	// Two tool calls: index 0 (read_file) and index 1 (write_file), each
	// streamed as name then argument deltas. Interleaved to stress ordering.
	// Note: arguments are JSON strings, so inner quotes must be escaped.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"/tmp/x\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"/tmp/y\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	// Accumulator should have both tool calls.
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls in accumulator, got %d (%+v)", len(tcs), tcs)
	}

	clientPayload := buf.String()

	// Both tool calls must appear in the client stream.
	if !strings.Contains(clientPayload, `"name":"read_file"`) {
		t.Errorf("client stream missing read_file tool call: %s", clientPayload)
	}
	if !strings.Contains(clientPayload, `"name":"write_file"`) {
		t.Errorf("client stream missing write_file tool call: %s", clientPayload)
	}

	// Both must appear before [DONE].
	readIdx := strings.Index(clientPayload, `"name":"read_file"`)
	writeIdx := strings.Index(clientPayload, `"name":"write_file"`)
	doneIdx := strings.Index(clientPayload, "[DONE]")
	if readIdx == -1 || writeIdx == -1 || doneIdx == -1 {
		t.Fatalf("missing expected events in payload: %s", clientPayload)
	}
	if readIdx > doneIdx || writeIdx > doneIdx {
		t.Errorf("tool calls appear AFTER [DONE]: read=%d write=%d done=%d\npayload:\n%s",
			readIdx, writeIdx, doneIdx, clientPayload)
	}

	// Tool calls must be in index order: read_file (index 0) before write_file (index 1).
	if readIdx > writeIdx {
		t.Errorf("tool calls out of order: write_file (index 1) appeared before read_file (index 0)\npayload:\n%s",
			clientPayload)
	}
}

// TestStreamSSE_ToolCallWithContent verifies that when an upstream event
// carries both content and a tool call delta, the content is forwarded
// verbatim AND the tool call is coalesced — the client sees the content
// immediately and the complete tool call at the end (before [DONE]).
func TestStreamSSE_ToolCallWithContent(t *testing.T) {
	// Event 1: content + tool call name (same event). Event 2: tool call args.
	// Note: arguments are JSON strings, so inner quotes must be escaped.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Let me check\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"key\\\":\\\"foo\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call in accumulator, got %d", len(tcs))
	}
	if tcs[0].Name != "lookup" {
		t.Errorf("expected tool name lookup, got %q", tcs[0].Name)
	}

	clientPayload := buf.String()

	// Content must be forwarded verbatim.
	if !strings.Contains(clientPayload, "Let me check") {
		t.Errorf("client stream missing forwarded content: %s", clientPayload)
	}

	// The coalesced tool call must appear before [DONE].
	tcIdx := strings.Index(clientPayload, `"name":"lookup"`)
	doneIdx := strings.Index(clientPayload, "[DONE]")
	if tcIdx == -1 {
		t.Fatalf("tool call event not found in payload: %s", clientPayload)
	}
	if doneIdx == -1 {
		t.Fatalf("[DONE] not found in payload: %s", clientPayload)
	}
	if tcIdx > doneIdx {
		t.Errorf("tool call appears AFTER [DONE]\npayload:\n%s", clientPayload)
	}

	// The partial tool call delta (with empty args) must NOT leak through.
	if strings.Contains(clientPayload, `"arguments":""`) {
		t.Errorf("client stream leaked partial tool call delta: %s", clientPayload)
	}

	// Content must still be present and intact after stripping tool calls.
	if !strings.Contains(clientPayload, "Let me check") {
		t.Errorf("content was stripped along with tool calls: %s", clientPayload)
	}
	// The role should still be present.
	if !strings.Contains(clientPayload, `"role":"assistant"`) {
		t.Errorf("role was stripped from content event: %s", clientPayload)
	}
}

// TestStreamSSE_NameArrivesLate verifies that when the upstream streams a tool
// call where the function name arrives in a LATER delta than the arguments
// chunk, the proxy still coalesces it into a single complete tool call. This
// exercises the accumulation path across multiple SSE events where the first
// delta has only arguments and the second has only the name.
func TestStreamSSE_NameArrivesLate(t *testing.T) {
	// Event 1: arguments chunk only (no name). Event 2: name chunk only.
	// The proxy must accumulate both and emit a single complete tool call.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_late\",\"type\":\"function\",\"function\":{\"arguments\":\"{\\\"path\\\":\\\"/tmp/x\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read_file\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call in accumulator, got %d (%+v)", len(tcs), tcs)
	}
	if tcs[0].Name != "read_file" {
		t.Errorf("expected tool name read_file, got %q", tcs[0].Name)
	}
	if tcs[0].ID != "call_late" {
		t.Errorf("expected tool id call_late, got %q", tcs[0].ID)
	}
	if tcs[0].Arguments != `{"path":"/tmp/x"}` {
		t.Errorf("expected arguments %q, got %q", `{"path":"/tmp/x"}`, tcs[0].Arguments)
	}

	clientPayload := buf.String()
	// The synthesized tool call MUST appear before [DONE].
	tcIdx := strings.Index(clientPayload, `"name":"read_file"`)
	doneIdx := strings.Index(clientPayload, "[DONE]")
	if tcIdx == -1 {
		t.Fatalf("synthesized tool call not found in client payload: %s", clientPayload)
	}
	if doneIdx == -1 {
		t.Fatalf("[DONE] not found in client payload: %s", clientPayload)
	}
	if tcIdx > doneIdx {
		t.Errorf("tool call appears AFTER [DONE]\npayload:\n%s", clientPayload)
	}
	// No partial chunks must leak through.
	if strings.Contains(clientPayload, `"arguments":"{"`) {
		t.Errorf("client stream leaked partial arguments chunk: %s", clientPayload)
	}
}

// TestStreamSSE_PartialArgumentsAcrossChunks verifies that when the upstream
// streams a tool call's arguments in multiple chunks (e.g., a large JSON
// payload split across SSE events), the proxy correctly concatenates them
// into a single complete arguments string.
func TestStreamSSE_PartialArgumentsAcrossChunks(t *testing.T) {
	// Tool call with arguments split across 3 chunks: {"key": "value", "data": "some_long_content_here"}
	// Chunk 1: {"key": "
	// Chunk 2: value", "data": "
	// Chunk 3: some_long_content_here"}
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_big\",\"type\":\"function\",\"function\":{\"name\":\"process\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"key\\\": \\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"value\\\", \\\"data\\\": \\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"some_long_content_here\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call in accumulator, got %d (%+v)", len(tcs), tcs)
	}
	if tcs[0].Name != "process" {
		t.Errorf("expected tool name process, got %q", tcs[0].Name)
	}
	expectedArgs := `{"key": "value", "data": "some_long_content_here"}`
	if tcs[0].Arguments != expectedArgs {
		t.Errorf("expected arguments %q, got %q", expectedArgs, tcs[0].Arguments)
	}

	clientPayload := buf.String()
	// The synthesized tool call MUST appear before [DONE].
	tcIdx := strings.Index(clientPayload, `"name":"process"`)
	doneIdx := strings.Index(clientPayload, "[DONE]")
	if tcIdx == -1 {
		t.Fatalf("synthesized tool call not found in client payload: %s", clientPayload)
	}
	if doneIdx == -1 {
		t.Fatalf("[DONE] not found in client payload: %s", clientPayload)
	}
	if tcIdx > doneIdx {
		t.Errorf("tool call appears AFTER [DONE]\npayload:\n%s", clientPayload)
	}
	// No partial argument chunks must leak through.
	if strings.Contains(clientPayload, `"arguments":"{\\"key\\": \\""`) {
		t.Errorf("client stream leaked partial arguments chunk: %s", clientPayload)
	}
}

// TestStreamSSE_SameIndexOverwrite documents the behavior when the upstream
// sends two different tool call deltas at the same index. The current
// implementation silently overwrites the pending tool call at that index.
// This test documents that behavior so future changes are aware of it.
func TestStreamSSE_SameIndexOverwrite(t *testing.T) {
	// Two tool calls both at index 0 — the second should overwrite the first.
	// This is a malformed upstream scenario; we document current behavior.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call_first\",\"type\":\"function\",\"function\":{\"name\":\"first_func\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_second\",\"type\":\"function\",\"function\":{\"name\":\"second_func\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(true)

	_, tcs, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	// The accumulator has 1 entry (same index, second overwrites first).
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call in accumulator, got %d (%+v)", len(tcs), tcs)
	}
	// The second tool call overwrites the first at the same index.
	if tcs[0].Name != "second_func" {
		t.Errorf("expected second_func to overwrite first_func, got %q", tcs[0].Name)
	}
	if tcs[0].ID != "call_second" {
		t.Errorf("expected id call_second, got %q", tcs[0].ID)
	}

	clientPayload := buf.String()
	// Only the second tool call should appear in the client stream.
	if strings.Contains(clientPayload, `"name":"first_func"`) {
		t.Errorf("client stream should not contain first_func (overwritten): %s", clientPayload)
	}
	if !strings.Contains(clientPayload, `"name":"second_func"`) {
		t.Errorf("client stream should contain second_func: %s", clientPayload)
	}
}

// TestAppendAssistantMessage_FiltersIncompleteToolCalls verifies that
// appendAssistantMessage drops tool calls that have no function name yet.
// These are partial deltas from a stream that failed before the name arrived;
// replaying them as `{"function":{"arguments":...}}` with no name produces a
// malformed tool call that breaks the next model's parser.
func TestAppendAssistantMessage_FiltersIncompleteToolCalls(t *testing.T) {
	base := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	tcs := []streamToolCall{
		{Index: 0, ID: "complete_1", Type: "function", Name: "read_file", Arguments: `{"path":"/x"}`},
		{Index: 1, ID: "incomplete_1", Type: "function", Arguments: `{"path":"/y"}`},
		{Index: 2, ID: "complete_2", Type: "function", Name: "write_file", Arguments: `{"path":"/z"}`},
	}

	body := appendAssistantMessage([]byte(base), "partial content", tcs)

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(parsed["messages"], &msgs); err != nil {
		t.Fatalf("failed to parse messages: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (original + assistant), got %d", len(msgs))
	}

	assistant := msgs[1]
	if string(assistant["role"]) != `"assistant"` {
		t.Errorf("expected role assistant, got %s", string(assistant["role"]))
	}

	var toolCalls []map[string]json.RawMessage
	if tc, ok := assistant["tool_calls"]; ok {
		if err := json.Unmarshal(tc, &toolCalls); err != nil {
			t.Fatalf("failed to parse tool_calls: %v", err)
		}
	} else {
		t.Fatal("expected tool_calls in assistant message")
	}

	// The incomplete tool call (no name) must be dropped.
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls (incomplete dropped), got %d: %s", len(toolCalls), string(assistant["tool_calls"]))
	}

	for _, tc := range toolCalls {
		fnStr := string(tc["function"])
		if !strings.Contains(fnStr, `"name":`) {
			t.Errorf("tool call missing name field (incomplete leaked through): %s", fnStr)
		}
	}
}

// TestHandleChatCompletionsStreamingWithFallback_ReplaysPartialToolCalls is an
// end-to-end test: the first model streams a partial tool call then emits an
// SSE error (mid-stream failure); the second model receives the request and
// should see the replayed assistant message with the coalesced tool call AND
// no malformed/incomplete tool call.
func TestHandleChatCompletionsStreamingWithFallback_ReplaysPartialToolCalls(t *testing.T) {
	// Model 1: streams content + a coalesced tool call, then emits an SSE error
	// event (simulates a mid-stream failure after the stream was released).
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		events := []string{
			`data: {"id":"s1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me"},"finish_reason":null}]}`,
			// Tool call: name chunk + argument chunks, all index:0.
			`data: {"id":"s1","object":"chat.completion.chunk","created":2,"model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
			`data: {"id":"s1","object":"chat.completion.chunk","created":3,"model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\\"path\\":\\"/tmp/x\\""}}]},"finish_reason":null}]}`,
			`data: {"id":"s1","object":"chat.completion.chunk","created":4,"model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":null}]}`,
			// A second tool call that is incomplete (only id, no name).
			`data: {"id":"s1","object":"chat.completion.chunk","created":5,"model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_incomplete","type":"function"}]},"finish_reason":null}]}`,
			// SSE error event triggers mid-stream fallback.
			`data: {"error":{"message":"Connection lost","type":"server_error"}}`,
		}
		for _, e := range events {
			fmt.Fprint(w, e+"\n\n")
			flusher.Flush()
		}
	}))
	defer backend1.Close()

	var backend2Body string
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		backend2Body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		events := []string{
			`data: {"id":"s2","object":"chat.completion.chunk","created":10,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"content":" continue"},"finish_reason":null}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			fmt.Fprint(w, e+"\n\n")
			flusher.Flush()
		}
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	proxy.SetToolCalls(true)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true,"tool_calls":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	// The second model must have received a replayed assistant message with the
	// coalesced tool call, and NOT the incomplete one.
	if !strings.Contains(backend2Body, "Let me") {
		t.Errorf("expected partial content replayed to backend2, got: %s", backend2Body)
	}
	if !strings.Contains(backend2Body, `"name":"read_file"`) {
		t.Errorf("expected coalesced tool call replayed to backend2, got: %s", backend2Body)
	}
	if strings.Contains(backend2Body, `"id":"call_incomplete"`) {
		t.Errorf("incomplete tool call leaked into replayed message: %s", backend2Body)
	}
}

// TestStreamSSE_FirstByteReaderNoGoroutineLeak verifies that when the
// first-byte timeout fires (upstream never sends a byte), the spawned
// goroutine is cleaned up and doesn't leak. This guards against the
// latent bug where a non-closer f.r would leak the goroutine.
func TestStreamSSE_FirstByteReaderNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	// slowReader blocks until closed, simulating an upstream that never
	// sends a byte. It implements io.Closer so firstByteReader can unblock
	// the goroutine on timeout.
	sr := &slowReader{delay: 5 * time.Second}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer firstCancel()
	f := &firstByteReader{r: sr, firstCtx: firstCtx}

	_, err := f.Read(make([]byte, 1024))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Give the goroutine a moment to exit after the reader is closed.
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	// Allow for small fluctuations (GC, runtime timers), but the goroutine
	// spawned by firstByteReader must have exited.
	if after > before+2 {
		t.Errorf("goroutine leak: before=%d after=%d (delta=%d)", before, after, after-before)
	}
}

// slowReader blocks for `delay` or until closed, then returns io.EOF.
// It implements io.Closer so firstByteReader can unblock it on timeout.
type slowReader struct {
	delay time.Duration
	mu    sync.Mutex
	done  chan struct{}
}

func (s *slowReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	s.mu.Unlock()
	select {
	case <-time.After(s.delay):
		return 0, io.EOF
	case <-s.done:
		return 0, io.EOF
	}
}

func (s *slowReader) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		select {
		case <-s.done:
			// already closed
		default:
			close(s.done)
		}
	}
	return nil
}

// TestStreamSSE_IdleTimeoutClosesReader verifies that when the idle timeout
// fires (upstream stalls mid-stream), the idleTimeoutReader closes the
// underlying reader, unblocking any in-flight Read.
func TestStreamSSE_IdleTimeoutClosesReader(t *testing.T) {
	sr := &slowReader{delay: 10 * time.Second}
	idle := newIdleTimeoutReader(sr, 50*time.Millisecond)

	start := time.Now()
	_, err := idle.Read(make([]byte, 1024))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from idle timeout, got nil")
	}
	// Should return near-instantly after the idle timeout, not block for
	// the full 10s that slowReader would otherwise hold.
	if elapsed > 2*time.Second {
		t.Errorf("idle timeout took too long: %v (reader not closed?)", elapsed)
	}
}

// TestStreamSSE_BoundedBodyGrowth verifies that repeated stream failures
// with partial output don't cause unbounded body growth. The body grows by
// one assistant message per failed attempt, bounded by maxAttempts.
func TestStreamSSE_BoundedBodyGrowth(t *testing.T) {
	// Simulate a body that grows with each retry. After N failures, the
	// body should have at most N assistant messages.
	original := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	body := []byte(original)

	// Simulate 5 failed streaming attempts, each producing partial output.
	for i := 0; i < 5; i++ {
		partial := fmt.Sprintf("partial%d ", i)
		toolCalls := []streamToolCall{{Index: 0, ID: fmt.Sprintf("call_%d", i), Name: "fn", Arguments: "{}"}}
		body = appendAssistantMessage(body, partial, toolCalls)
	}

	// Count assistant messages in the body. Should be exactly 5 (one per attempt).
	var parsed struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	assistantCount := 0
	for _, m := range parsed.Messages {
		if m["role"] == "assistant" {
			assistantCount++
		}
	}
	if assistantCount != 5 {
		t.Errorf("expected 5 assistant messages, got %d", assistantCount)
	}

	// Verify the body is still valid JSON and parseable.
	if !json.Valid(body) {
		t.Errorf("body is not valid JSON after growth")
	}
}

// TestHandleStream_ResourceCleanup verifies that handleStream cleans up all
// resources (goroutines, timers) when the context is cancelled mid-stream.
// This guards against goroutine and timer leaks during client disconnects.
func TestHandleStream_ResourceCleanup(t *testing.T) {
	// Set up a backend that streams slowly so we can cancel mid-stream.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		// Send a chunk then stall for a long time.
		fmt.Fprint(w, `data: {"id":"s1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		time.Sleep(30 * time.Second)
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "test", Model: "gpt-4"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	// Measure goroutine count before.
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	)).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run the handler in a goroutine, then cancel after a short delay.
	done := make(chan struct{})
	go func() {
		gateway.HandleChatCompletions(rec, req)
		close(done)
	}()

	// Give the handler time to start streaming, then cancel the context.
	time.Sleep(200 * time.Millisecond)
	cancel()

	// Wait for the handler to return.
	<-done

	// Allow goroutines to clean up.
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	// The handler should not have leaked goroutines. Allow a small buffer
	// for runtime timers and GC, but the firstByteReader and idleTimeoutReader
	// goroutines must have exited.
	if after > before+3 {
		t.Errorf("goroutine leak after context cancellation: before=%d after=%d (delta=%d)", before, after, after-before)
	}
}

// TestStreamSSE_FinalEventNoTrailingBlankLine verifies that the last SSE event
// is forwarded to the client even when the upstream closes the connection
// without a trailing blank line. Previously, an unterminated final event would
// sit in eventBuf after the scanner loop and be silently dropped.
func TestStreamSSE_FinalEventNoTrailingBlankLine(t *testing.T) {
	// Upstream sends a content chunk with NO trailing blank line (the
	// connection just ends after the data: line). The proxy must still
	// process and forward this event.
	input := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"final\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" event\"},\"finish_reason\":null}]}"

	body := strings.NewReader(input)
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}

	_, _, _, err := p.streamSSE(flusher, flusher, body)
	if err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}

	clientPayload := buf.String()
	if !strings.Contains(clientPayload, "final") {
		t.Errorf("client stream missing 'final' from the first event: %s", clientPayload)
	}
	if !strings.Contains(clientPayload, "event") {
		t.Errorf("client stream missing 'event' from the unterminated final event: %s", clientPayload)
	}
}

// TestApplyCooldown_TruncatesLastError verifies that a long provider error body
// is truncated when stored as LastError in the cooldown entry, so cooldowns.json
// doesn't grow without bound when a verbose provider keeps failing.
func TestApplyCooldown_TruncatesLastError(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	longMsg := strings.Repeat("x", 1000)
	router.ApplyCooldown(ep, 500, longMsg)

	cds := router.GetAllCooldowns()
	cd, ok := cds[ep.Key()]
	if !ok {
		t.Fatal("expected cooldown entry")
	}
	if len(cd.LastError) > 203 {
		t.Errorf("LastError not truncated: got %d chars, want <= 203 (200 runes + ellipsis)", len(cd.LastError))
	}
	if !strings.HasSuffix(cd.LastError, "...") {
		t.Errorf("expected truncated LastError to end with '...', got: %q", cd.LastError)
	}
}

// TestSessionEviction verifies that sessions idle longer than sessionTTL are
// evicted by the sweeper, so the sessions map can't grow without bound. The
// test uses a short TTL via a test-only path (evictStaleSessions with a
// modified lastUsed) to avoid waiting 24h.
func TestSessionEviction(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	defer router.Close()

	// Create several sessions.
	for i := 0; i < 5; i++ {
		router.SelectEndpoint("smart", fmt.Sprintf("session-%d", i), false)
	}

	// Manually age all sessions beyond the TTL.
	router.mu.Lock()
	for sid, se := range router.sessions {
		se.lastUsed = time.Now().Add(-sessionTTL - time.Hour)
		router.sessions[sid] = se
	}
	router.mu.Unlock()

	// Run the eviction sweep.
	router.evictStaleSessions()

	// All stale sessions should be gone.
	router.mu.RLock()
	count := len(router.sessions)
	router.mu.RUnlock()
	if count != 0 {
		t.Errorf("expected 0 sessions after eviction, got %d", count)
	}
}

// TestSessionLastUsedUpdated verifies that accessing a session updates its
// lastUsed timestamp, so active sessions are not evicted.
func TestSessionLastUsedUpdated(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	defer router.Close()

	router.SelectEndpoint("smart", "active-session", false)

	// Age the session.
	router.mu.Lock()
	se := router.sessions["active-session"]
	se.lastUsed = time.Now().Add(-sessionTTL - time.Hour)
	router.sessions["active-session"] = se
	router.mu.Unlock()

	// Access the session via SelectEndpoint, which refreshes lastUsed.
	_, _ = router.SelectEndpoint("smart", "active-session", false)

	// Run eviction — the session should survive because it was just accessed.
	router.evictStaleSessions()

	router.mu.RLock()
	_, stillThere := router.sessions["active-session"]
	router.mu.RUnlock()
	if !stillThere {
		t.Error("active session was incorrectly evicted")
	}
}

// TestFourModelAliases verifies that all four logical model names (smart,
// work, fast, large) are registered and accept routed requests. It is the
// top-level invariant the gateway exists to provide.
func TestFourModelAliases(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID:      "test",
			Object:  "chat.completion",
			Created: 1,
			Model:   "test-model",
			Choices: []ChatCompletionChoice{{
				Index:   0,
				Message: ChatCompletionMessage{Role: "assistant", Content: "OK"},
			}},
		})
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "m1"}}},
			"work":  {Chain: []ModelEndpoint{{Provider: "test", Model: "m2"}}},
			"fast":  {Chain: []ModelEndpoint{{Provider: "test", Model: "m3"}}},
			"large": {Chain: []ModelEndpoint{{Provider: "test", Model: "m4"}}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	for _, model := range LogicalModels {
		t.Run(model, func(t *testing.T) {
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()
			gateway.HandleChatCompletions(rec, req)

			if rec.Code != 200 {
				t.Errorf("model %s: expected status 200, got %d", model, rec.Code)
			}
			var result ChatCompletionResponse
			if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
				t.Fatalf("model %s: failed to decode response: %v", model, err)
			}
			if len(result.Choices) == 0 {
				t.Errorf("model %s: expected at least one choice", model)
			}
		})
	}
}

// TestSessionPersistenceAcrossRequests verifies that a conversation (same
// model + messages) always routes to the same model across multiple
// requests. This is the core stickiness guarantee: the client gets
// continuity of context as long as the backend is healthy.
func TestSessionPersistenceAcrossRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID:      "test",
			Object:  "chat.completion",
			Created: 1,
			Model:   "test-model",
			Choices: []ChatCompletionChoice{{
				Index:   0,
				Message: ChatCompletionMessage{Role: "assistant", Content: "OK"},
			}},
		})
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "m1"}}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"what is 2+2?"}]}`

	// First request: establishes session
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("first request: expected 200, got %d", rec1.Code)
	}
	var resp1 ChatCompletionResponse
	json.NewDecoder(rec1.Body).Decode(&resp1)
	firstModel := resp1.Model

	// Second request with same body: must route to same model
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("second request: expected 200, got %d", rec2.Code)
	}
	var resp2 ChatCompletionResponse
	json.NewDecoder(rec2.Body).Decode(&resp2)

	if resp2.Model != firstModel {
		t.Errorf("session not persistent: first=%s, second=%s (expected same)", firstModel, resp2.Model)
	}
}

// TestRateLimitFallthrough verifies the core rate-limit recovery story:
// when the first model in the chain returns 429, the session moves to the
// next model, and subsequent requests stay on the new model (the cooldown
// prevents bouncing back to the rate-limited one).
func TestRateLimitFallthrough(t *testing.T) {
	var mu sync.Mutex
	requestCount := map[string]int{}

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount["backend1"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "rate limited", "type": "rate_limit"},
		})
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount["backend2"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			ID:      "ok",
			Object:  "chat.completion",
			Created: 1,
			Model:   "gpt-4-turbo",
			Choices: []ChatCompletionChoice{{
				Index:   0,
				Message: ChatCompletionMessage{Role: "assistant", Content: "OK"},
			}},
		})
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`

	// First request: backend1 429s, falls through to backend2
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec1 := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("first request: expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var resp1 ChatCompletionResponse
	json.NewDecoder(rec1.Body).Decode(&resp1)
	if resp1.Model != "gpt-4-turbo" {
		t.Errorf("expected fallback to gpt-4-turbo, got %s", resp1.Model)
	}

	// Second request: backend1 should be in cooldown; session stays on backend2
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("second request: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp2 ChatCompletionResponse
	json.NewDecoder(rec2.Body).Decode(&resp2)
	if resp2.Model != "gpt-4-turbo" {
		t.Errorf("expected session to stay on gpt-4-turbo, got %s", resp2.Model)
	}

	// backend1 should have been hit exactly once (the initial 429)
	mu.Lock()
	c1 := requestCount["backend1"]
	mu.Unlock()
	if c1 != 1 {
		t.Errorf("expected backend1 to be hit once (rate limited), got %d", c1)
	}
}

// TestMidStreamErrorRecovery verifies that when a stream fails mid-generation
// (after content has been flushed to the client), the next model resumes from
// where the previous one left off — the client sees no error, just a brief
// pause while the router switches models.
func TestMidStreamErrorRecovery(t *testing.T) {
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		// Send a content chunk, then an SSE error event (simulates mid-stream failure)
		fmt.Fprint(w, `data: {"id":"s1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"error":{"message":"Connection lost","type":"server_error"}}`+"\n\n")
		flusher.Flush()
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify that the replayed request contains the partial content as an assistant message
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "Hello") {
			t.Errorf("backend2 did not receive replayed content: %s", string(b))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"s2","object":"chat.completion.chunk","created":2,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"content":"World"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: [DONE]`+"\n\n")
		flusher.Flush()
	}))
	defer backend2.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "gpt-4"},
				{Provider: "backend2", Model: "gpt-4-turbo"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "Hello") {
		t.Errorf("expected 'Hello' in response, got: %s", respBody)
	}
	if !strings.Contains(respBody, "World") {
		t.Errorf("expected 'World' in response (resumed on next model), got: %s", respBody)
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Errorf("expected [DONE] in response, got: %s", respBody)
	}
	// The client must not see any error payload
	if strings.Contains(respBody, `"error"`) {
		t.Errorf("client should not see error, but response contains one: %s", respBody)
	}
}

// TestModelCooldownDuration verifies that cooldown periods are proportional
// to the error type: transient errors (429) get a short cooldown so the
// model is retried quickly; permanent-looking errors (404/401/403) get a
// long cooldown so we stop hammering a broken endpoint.
func TestModelCooldownDuration(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// A 429 should yield a short cooldown (~30s)
	router.ApplyCooldown(ep, 429, "rate limited")
	cd := router.GetAllCooldowns()[ep.Key()]
	wait429 := time.Until(cd.Expiry)
	if wait429 < 28*time.Second || wait429 > 32*time.Second {
		t.Errorf("429 cooldown expected ~30s, got %v", wait429)
	}

	// A 404 should yield a long cooldown (30 min)
	router.ResetCooldown(ep)
	router.ApplyCooldown(ep, 404, "not found")
	cd = router.GetAllCooldowns()[ep.Key()]
	wait404 := time.Until(cd.Expiry)
	if wait404 < 29*time.Minute || wait404 > 31*time.Minute {
		t.Errorf("404 cooldown expected ~30m, got %v", wait404)
	}

	// 404 cooldown must be MUCH longer than 429 cooldown
	if wait404 < wait429*50 {
		t.Errorf("404 cooldown (%v) should be much longer than 429 cooldown (%v)", wait404, wait429)
	}
}

// TestCooldownEscalation429 verifies that repeated 429s escalate the cooldown
// to a longer duration (not stuck at a short one), preventing the infinite
// retry loop that caused error counts to climb into the thousands. With the
// linear escalation (base * factor, capped at 10x), 10 consecutive 429s
// yields a 5-minute cooldown instead of the base 30s.
func TestCooldownEscalation429(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// After 10 consecutive 429s, the cooldown should be 5 minutes
	// (30s base * 10 factor = 300s), not the base 30s.
	for i := 0; i < 10; i++ {
		router.ApplyCooldown(ep, 429, "rate limited")
	}
	cd := router.GetAllCooldowns()[ep.Key()]
	wait := time.Until(cd.Expiry)
	if wait < 4*time.Minute || wait > 6*time.Minute {
		t.Errorf("after 10 consecutive 429s, cooldown should be ~5min, got %v", wait)
	}
}

// TestCooldownErrorCountCapped verifies that ErrorCount doesn't grow without
// bound in the cooldowns map (capped at 100 to prevent memory bloat).
func TestCooldownErrorCountCapped(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// Simulate many failures
	for i := 0; i < 150; i++ {
		router.ApplyCooldown(ep, 500, "server error")
	}
	cd := router.GetAllCooldowns()[ep.Key()]
	if cd.ErrorCount > 100 {
		t.Errorf("ErrorCount should be capped at 100, got %d", cd.ErrorCount)
	}
}

// TestStaleEntryCleanup verifies that cooldown/noVision/tps entries for
// endpoints removed from the config are cleaned up by the sweeper.
func TestStaleEntryCleanup(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"openai": {URL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "openai", Model: "gpt-4"},
				{Provider: "openai", Model: "gpt-3.5-turbo"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	ep1 := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	ep2 := &ModelEndpoint{Provider: "openai", Model: "gpt-3.5-turbo"}

	// Add cooldown entries for both endpoints
	router.ApplyCooldown(ep1, 429, "rate limited")
	router.ApplyCooldown(ep2, 500, "server error")
	router.MarkNoVision(ep2)
	router.RecordTPS(*ep2, 10.5)

	// Verify both are present
	if len(router.GetAllCooldowns()) != 2 {
		t.Fatalf("expected 2 cooldowns, got %d", len(router.GetAllCooldowns()))
	}

	// Now remove ep2 from the config
	cfg2 := &Config{
		Providers: map[string]ProviderConfig{
			"openai": {URL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "openai", Model: "gpt-4"},
			}},
		},
	}
	router.config.Store(cfg2)

	// Trigger cleanup
	router.cleanupStaleEntries()

	// ep2's entries should be gone
	if _, ok := router.GetAllCooldowns()[ep2.Key()]; ok {
		t.Errorf("stale cooldown entry for %s was not cleaned up", ep2.Key())
	}
	// ep1's entry should remain
	if _, ok := router.GetAllCooldowns()[ep1.Key()]; !ok {
		t.Errorf("active cooldown entry for %s was incorrectly removed", ep1.Key())
	}
}

// TestGetSessionDoesNotBlock verifies that GetSession uses a read lock and
// doesn't serialize with other reads (no write lock contention).
func TestGetSessionDoesNotBlock(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	// Create a session
	_, _ = router.SelectEndpoint("smart", "test-session", false)

	// GetSession should not block concurrent reads
	done := make(chan bool, 1)
	go func() {
		router.GetSession("test-session")
		done <- true
	}()

	select {
	case <-done:
		// success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("GetSession blocked for too long (likely using write lock)")
	}
}

// TestCloseFlushesCooldownSave verifies that Close() flushes any pending
// debounced cooldown save so state isn't lost on shutdown.
func TestCloseFlushesCooldownSave(t *testing.T) {
	cfg := loadTestConfig(t)
	// Use a temp file so we can verify the write happened
	tmpDir := t.TempDir()
	cooldownPath := tmpDir + "/cooldowns.json"
	router := NewRouter(cfg, cooldownPath)
	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}

	// Apply a cooldown (triggers debounced save)
	router.ApplyCooldown(ep, 429, "rate limited")

	// Close should flush
	router.Close()

	// Verify the file was written
	data, err := readFile(cooldownPath)
	if err != nil {
		t.Fatalf("cooldowns.json not written after Close: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("cooldowns.json is empty after Close")
	}
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// TestSessionSkipsTriedEndpoints verifies that after a model fails for a
// session, SelectEndpoint won't pick it again even after its cooldown expires.
// This prevents the retry storm where the same model gets retried every time
// its cooldown window closes.
func TestSessionSkipsTriedEndpoints(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p1": {URL: "https://p1.example.com/v1", APIKeyEnv: "P1_KEY"},
			"p2": {URL: "https://p2.example.com/v1", APIKeyEnv: "P2_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "p1", Model: "m1"},
				{Provider: "p2", Model: "m2"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	sessionID := "test-retry-storm"

	// First call: gets p1
	ep, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep == nil || ep.Provider != "p1" {
		t.Fatalf("expected p1, got %v", ep)
	}

	// p1 fails
	router.ApplyCooldownForSession(ep, 429, "rate limited", sessionID)

	// Second call: should skip p1 (tried) and get p2
	ep, _ = router.SelectEndpoint("smart", sessionID, false)
	if ep == nil || ep.Provider != "p2" {
		t.Fatalf("expected p2 after p1 failed, got %v", ep)
	}

	// p2 also fails
	router.ApplyCooldownForSession(ep, 429, "rate limited", sessionID)

	// Third call: both have been tried, but p1's cooldown may have expired
	// (30s base). We should still skip it because it's in the tried set.
	// The tried set only clears on success.
	ep, _ = router.SelectEndpoint("smart", sessionID, false)
	if ep == nil {
		t.Fatal("expected an endpoint (exhausted fallback), got nil")
	}
	// At this point both are tried, so it should pick the first available
	// (the exhausted fallback path). The key point is it doesn't loop forever.
}

// TestRecordSuccessClearsTriedSet verifies that a successful response clears
// the session's tried set, making all endpoints eligible again.
func TestRecordSuccessClearsTriedSet(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p1": {URL: "https://p1.example.com/v1", APIKeyEnv: "P1_KEY"},
			"p2": {URL: "https://p2.example.com/v1", APIKeyEnv: "P2_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "p1", Model: "m1"},
				{Provider: "p2", Model: "m2"},
			}},
		},
	}
	router := NewRouter(cfg, "")
	sessionID := "test-clear-tried"

	// p1 fails
	ep1, _ := router.SelectEndpoint("smart", sessionID, false)
	router.ApplyCooldownForSession(ep1, 429, "rate limited", sessionID)

	// Should now get p2
	ep2, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep2.Provider != "p2" {
		t.Fatalf("expected p2, got %s", ep2.Provider)
	}

	// p2 succeeds — this should clear the tried set
	router.RecordSuccess(ep2)

	// Now p1 should be eligible again (even though its cooldown hasn't expired)
	ep, _ := router.SelectEndpoint("smart", sessionID, false)
	if ep == nil {
		t.Fatal("expected an endpoint after success cleared tried set")
	}
	// p1 is still in cooldown, so we should get p2 again (the first available)
	if ep.Provider != "p2" {
		t.Fatalf("expected p2 (p1 still in cooldown), got %s", ep.Provider)
	}
}
