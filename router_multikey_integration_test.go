package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMultiKeyFallbackIntegration verifies end-to-end that when the first API key
// for a provider returns a 429 rate limit, the router and gateway automatically
// fall back to the second API key for the SAME model before moving on to other models.
func TestMultiKeyFallbackIntegration(t *testing.T) {
	noRotation := 0

	// Track which API key received requests
	var receivedKeys []string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		receivedKeys = append(receivedKeys, auth)
		mu.Unlock()

		if auth == "Bearer key-1" {
			// First key rate-limited
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"rate limit key 1"}}`))
			return
		}

		// Second key succeeds
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"success from key 2"},"finish_reason":"stop"}]}`)
	}))
	defer backend.Close()

	t.Setenv("TEST_KEY_1", "key-1")
	t.Setenv("TEST_KEY_2", "key-2")

	cfg := &Config{
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Providers: map[string]ProviderConfig{
			"myprovider": {
				URL:        backend.URL,
				APIKeyEnvs: []string{"TEST_KEY_1", "TEST_KEY_2"},
			},
		},
		Models: map[string]ModelConfig{
			"smart": {
				Chain: []ModelEndpoint{
					{Provider: "myprovider", Model: "my-model"},
				},
			},
		},
	}

	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)
	gateway.SetTestCooldown(100 * time.Millisecond)

	body := `{"model":"smart","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected HTTP 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	mu.Lock()
	keys := receivedKeys
	mu.Unlock()

	if len(keys) < 2 {
		t.Fatalf("expected at least 2 requests (key 1 fail, key 2 success), got %d: %v", len(keys), keys)
	}

	if keys[0] != "Bearer key-1" || keys[1] != "Bearer key-2" {
		t.Errorf("expected request order [Bearer key-1, Bearer key-2], got %v", keys)
	}
}
