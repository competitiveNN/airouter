package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		name     string
		provider string
		body     string
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

func TestRouterSessionAssignment(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "test-session-1"

	// First call should assign to first model in chain
	ep, wait := router.SelectEndpoint("smart", sessionID)
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
	ep2, _ := router.SelectEndpoint("smart", sessionID)
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

	// First consecutive error -> 60s cooldown
	router.ApplyCooldown(ep, 429, "rate limited")
	if router.IsAvailable(ep) {
		t.Error("expected model to be unavailable")
	}

	// The stored cooldown should reflect the first schedule entry (~60s).
	cds := router.GetAllCooldowns()
	cd, ok := cds[ep.Key()]
	if !ok {
		t.Fatal("expected cooldown entry")
	}
	if cd.ErrorCount != 1 {
		t.Errorf("expected error count 1, got %d", cd.ErrorCount)
	}
	if d := time.Until(cd.Expiry); d < 55*time.Second || d > 65*time.Second {
		t.Errorf("expected ~60s cooldown, got %v", d)
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

	want := []time.Duration{
		60 * time.Second,         // 1st consecutive error
		time.Hour,                // 2nd
		8 * time.Hour,            // 3rd
		24 * time.Hour,           // 4th
		3 * 24 * time.Hour,       // 5th (3 days)
		7 * 24 * time.Hour,       // 6th (7 days)
	}
	for i, w := range want {
		router.ApplyCooldown(ep, 500, "server error")
		cd, ok := router.GetAllCooldowns()[ep.Key()]
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

	// Consecutive errors beyond the schedule stay capped at the last entry.
	router.ApplyCooldown(ep, 500, "server error")
	cd := router.GetAllCooldowns()[ep.Key()]
	capDur := 7 * 24 * time.Hour
	if got := time.Until(cd.Expiry); got < capDur-5*time.Second || got > capDur+5*time.Second {
		t.Errorf("expected cooldown capped at 7d, got %v", got)
	}
}

func TestRouterModelFallback(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")
	sessionID := "test-session-2"

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	router.ApplyCooldown(ep, 500, "server error")

	// Should select next model in chain
	ep2, wait := router.SelectEndpoint("smart", sessionID)
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
	ep, _ := router.SelectEndpoint("smart", sessionID)
	if ep == nil {
		t.Fatal("expected endpoint")
	}
	if !ep.Equal(chain[1]) {
		t.Errorf("expected %v, got %v", chain[1], *ep)
	}

	// Now fail this model and select next
	router.ApplyCooldown(ep, 500, "server error")
	ep2, _ := router.SelectEndpoint("smart", sessionID)
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
	ep, wait := router.SelectEndpoint("smart", sessionID)
	if ep != nil {
		t.Error("expected nil endpoint when all in cooldown")
	}
	if wait <= 0 {
		t.Error("expected positive wait duration")
	}
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
		{429, 10 * time.Second},   // low cooldown
		{500, 30 * time.Second},   // medium
		{502, 30 * time.Second},   // medium
		{503, 30 * time.Second},   // medium
		{504, 60 * time.Second},   // medium-long
		{404, 300 * time.Second},  // high
		{401, 300 * time.Second},  // high
		{403, 300 * time.Second},  // high
		{0, 30 * time.Second},     // default (connection error)
		{999, 30 * time.Second},   // unknown
	}

	for _, tt := range tests {
		got := baseCooldownForError(tt.statusCode)
		if got != tt.expected {
			t.Errorf("baseCooldownForError(%d) = %v, want %v", tt.statusCode, got, tt.expected)
		}
	}
}

func TestRouterGetAllSessions(t *testing.T) {
	cfg := loadTestConfig(t)
	router := NewRouter(cfg, "")

	ep1, _ := router.SelectEndpoint("smart", "session-1")
	ep2, _ := router.SelectEndpoint("fast", "session-2")

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
	ep, _ := router.SelectEndpoint("smart", "test-session")
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

	err := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), ep, 30*time.Second)
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
	ep, _ := router.SelectEndpoint("smart", sessionID)
	err := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), *ep, 30*time.Second)
	if err == nil {
		t.Fatal("expected error from backend1")
	}

	// Apply cooldown and try next
	router.ApplyCooldownFromError(ep, err)
	ep2, _ := router.SelectEndpoint("smart", sessionID)
	if ep2 == nil {
		t.Fatal("expected second endpoint")
	}

	// Second attempt: backend2 should succeed
	err2 := proxy.StreamToClient(context.Background(), &output, flusher, []byte(body), *ep2, 30*time.Second)
	if err2 != nil {
		t.Fatalf("expected success from backend2, got: %v", err2)
	}

	outputStr := output.String()
	if !strings.Contains(outputStr, "Recovered") {
		t.Error("expected 'Recovered' in output")
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
	req.Header.Set("X-Session-ID", "test-session")
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
	req.Header.Set("X-Session-ID", "fallback-test")
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
	req.Header.Set("X-Session-ID", "stream-fallback-test")
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
	ep1, _ := router.SelectEndpoint("smart", sessionID)
	if ep1 == nil {
		t.Fatal("expected endpoint")
	}

	// Subsequent request should get the same model
	ep2, _ := router.SelectEndpoint("smart", sessionID)
	if ep2 == nil {
		t.Fatal("expected endpoint")
	}

	if !ep1.Equal(*ep2) {
		t.Errorf("session not persistent: %v != %v", *ep1, *ep2)
	}

	// Different session should get first available (may be different)
	ep3, _ := router.SelectEndpoint("smart", "different-session")
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

