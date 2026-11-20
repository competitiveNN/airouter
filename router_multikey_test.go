package main

import (
	"testing"
)

// TestExpandChainWithAPIKeys verifies that expandChainWithAPIKeys correctly
// creates virtual endpoints for each API key index of multi-key providers.
func TestExpandChainWithAPIKeys(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"single": {URL: "https://single", APIKeyEnvs: []string{"K1"}},
			"multi":  {URL: "https://multi", APIKeyEnvs: []string{"K1", "K2", "K3"}},
			"legacy": {URL: "https://legacy", APIKeyEnv: "K1"},
		},
	}

	r := NewRouter(cfg, "")
	defer r.Close()

	// Chain with single-key, multi-key, and legacy single-key providers
	chain := []ModelEndpoint{
		{Provider: "single", Model: "m1", Intelligence: f64(10)},
		{Provider: "multi", Model: "m2", Intelligence: f64(20)},
		{Provider: "legacy", Model: "m3", Intelligence: f64(30)},
	}

	expanded := r.expandChainWithAPIKeys(cfg, chain)

	// Expected: 1 (single) + 3 (multi) + 1 (legacy) = 5 endpoints
	if len(expanded) != 5 {
		t.Fatalf("expandChainWithAPIKeys returned %d endpoints, want 5", len(expanded))
	}

	// First endpoint: single-key provider, index 0
	if expanded[0].Provider != "single" || expanded[0].Model != "m1" || expanded[0].APIKeyIndex != 0 {
		t.Errorf("expanded[0] = %+v, want provider=single, model=m1, index=0", expanded[0])
	}

	// Next three: multi-key provider with indices 0, 1, 2
	for i := 0; i < 3; i++ {
		idx := 1 + i
		if expanded[idx].Provider != "multi" || expanded[idx].Model != "m2" || expanded[idx].APIKeyIndex != i {
			t.Errorf("expanded[%d] = %+v, want provider=multi, model=m2, index=%d", idx, expanded[idx], i)
		}
	}

	// Last: legacy single-key provider, index 0
	last := len(expanded) - 1
	if expanded[last].Provider != "legacy" || expanded[last].Model != "m3" || expanded[last].APIKeyIndex != 0 {
		t.Errorf("expanded[%d] = %+v, want provider=legacy, model=m3, index=0", last, expanded[last])
	}
}

// TestMultiKeyFallbackOrder verifies that when a provider has multiple API keys,
// the fallback order goes through all keys for the same model before advancing
// to the next model in the chain.
func TestMultiKeyFallbackOrder(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"multi": {
				URL:        "https://multi",
				APIKeyEnvs: []string{"K1", "K2", "K3"},
			},
			"next": {
				URL:        "https://next",
				APIKeyEnvs: []string{"K1"},
			},
		},
		Models: map[string]ModelConfig{
			"smart": {
				Chain: []ModelEndpoint{
					{Provider: "multi", Model: "model-a", Intelligence: f64(20)},
					{Provider: "next", Model: "model-b", Intelligence: f64(10)},
				},
			},
		},
	}

	r := NewRouter(cfg, "")
	defer r.Close()

	// Simulate the expanded chain for the "smart" model
	model := cfg.Models["smart"]
	expanded := r.expandChainWithAPIKeys(cfg, model.Chain)

	// Expected order: multi/model-a@idx0, multi/model-a@idx1, multi/model-a@idx2, next/model-b@idx0
	expected := []struct {
		provider  string
		model     string
		apiKeyIdx int
	}{
		{"multi", "model-a", 0},
		{"multi", "model-a", 1},
		{"multi", "model-a", 2},
		{"next", "model-b", 0},
	}

	if len(expanded) != len(expected) {
		t.Fatalf("expanded chain has %d entries, want %d", len(expanded), len(expected))
	}

	for i, exp := range expected {
		ep := expanded[i]
		if ep.Provider != exp.provider || ep.Model != exp.model || ep.APIKeyIndex != exp.apiKeyIdx {
			t.Errorf("expanded[%d] = provider=%s model=%s idx=%d, want provider=%s model=%s idx=%d",
				i, ep.Provider, ep.Model, ep.APIKeyIndex, exp.provider, exp.model, exp.apiKeyIdx)
		}
	}
}

// TestMultiKeyFallbackAfterRateLimit verifies that when the first API key
// is rate-limited (cooldown), the router falls back to the next API key for
// the same model.
func TestMultiKeyFallbackAfterRateLimit(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"multi": {
				URL:        "https://multi",
				APIKeyEnvs: []string{"K1", "K2", "K3"},
			},
		},
		Models: map[string]ModelConfig{
			"smart": {
				Chain: []ModelEndpoint{
					{Provider: "multi", Model: "model-a", Intelligence: f64(20)},
				},
			},
		},
	}

	r := NewRouter(cfg, "")
	defer r.Close()

	// Apply cooldown to the first API key (index 0)
	firstKey := ModelEndpoint{Provider: "multi", Model: "model-a", APIKeyIndex: 0}
	r.ApplyCooldownForSession(&firstKey, 429, "rate limit", "session-1", 0)

	// Select endpoint - should skip the cooled key and pick index 1
	ep, wait := r.SelectEndpointForTokens("smart", "session-2", false, map[string]bool{}, 1000)
	if ep == nil {
		t.Fatal("SelectEndpoint returned nil")
	}
	if ep.APIKeyIndex != 1 {
		t.Errorf("expected APIKeyIndex=1 (second key), got %d; wait=%v", ep.APIKeyIndex, wait)
	}

	// Apply cooldown to the second key too
	secondKey := ModelEndpoint{Provider: "multi", Model: "model-a", APIKeyIndex: 1}
	r.ApplyCooldownForSession(&secondKey, 429, "rate limit", "session-1", 0)

	// Should pick index 2
	ep, wait = r.SelectEndpointForTokens("smart", "session-3", false, map[string]bool{}, 1000)
	if ep == nil {
		t.Fatal("SelectEndpoint returned nil")
	}
	if ep.APIKeyIndex != 2 {
		t.Errorf("expected APIKeyIndex=2 (third key), got %d; wait=%v", ep.APIKeyIndex, wait)
	}

	// Apply cooldown to the third key - all keys for this model are cooled
	thirdKey := ModelEndpoint{Provider: "multi", Model: "model-a", APIKeyIndex: 2}
	r.ApplyCooldownForSession(&thirdKey, 429, "rate limit", "session-1", 0)

	// Should return nil with a wait time (all keys exhausted)
	ep, wait = r.SelectEndpointForTokens("smart", "session-4", false, map[string]bool{}, 1000)
	if ep != nil {
		t.Errorf("expected nil endpoint (all keys cooled), got %+v", ep)
	}
	if wait <= 0 {
		t.Errorf("expected positive wait time when all keys cooled, got %v", wait)
	}
}

// TestMultiKeySameModelKey verifies that Key() includes APIKeyIndex for
// per-key cooldown tracking, while ModelKey() is the same for all keys.
func TestMultiKeySameModelKey(t *testing.T) {
	ep1 := ModelEndpoint{Provider: "multi", Model: "model-a", APIKeyIndex: 0}
	ep2 := ModelEndpoint{Provider: "multi", Model: "model-a", APIKeyIndex: 1}
	ep3 := ModelEndpoint{Provider: "multi", Model: "model-a", APIKeyIndex: 2}

	// ModelKey should be the same for all keys of the same model
	if ep1.ModelKey() != ep2.ModelKey() || ep2.ModelKey() != ep3.ModelKey() {
		t.Errorf("ModelKey differs across API keys: %s, %s, %s", ep1.ModelKey(), ep2.ModelKey(), ep3.ModelKey())
	}

	// Key should differ for each API key index (for per-key cooldown tracking)
	if ep1.Key() == ep2.Key() || ep2.Key() == ep3.Key() || ep1.Key() == ep3.Key() {
		t.Errorf("Key should differ per API key index: %s, %s, %s", ep1.Key(), ep2.Key(), ep3.Key())
	}

	// Different models should have different ModelKeys
	ep4 := ModelEndpoint{Provider: "multi", Model: "model-b", APIKeyIndex: 0}
	if ep1.ModelKey() == ep4.ModelKey() {
		t.Errorf("different models should have different ModelKeys")
	}
}

// TestMandatoryProxyPerKeyIndex verifies that IsProxyMandatoryAt correctly
// evaluates the mandatory_index threshold per API key index.
// mandatory_index is 1-based: 2 means mandatory from the 2nd key onwards (index 1+).
func TestMandatoryProxyPerKeyIndex(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {
				URL:        "https://test",
				APIKeyEnvs: []string{"K1", "K2", "K3"},
				Proxies: ProxyConfig{
					MandatoryIndex: 2, // mandatory from index 1 onwards (2nd key, 1-based)
				},
			},
		},
	}

	r := NewRouter(cfg, "")
	defer r.Close()

	provider := cfg.Providers["test"]

	// Index 0 (first key): not mandatory
	if provider.IsProxyMandatoryAt(0) {
		t.Error("API key index 0 should not be mandatory when mandatory_index=2")
	}

	// Index 1 (second key): mandatory (mandatory_index=2 means 2nd key and onwards)
	if !provider.IsProxyMandatoryAt(1) {
		t.Error("API key index 1 should be mandatory when mandatory_index=2")
	}

	// Index 2 (third key): mandatory
	if !provider.IsProxyMandatoryAt(2) {
		t.Error("API key index 2 should be mandatory when mandatory_index=2")
	}
}

// TestSelectEndpointSkipsTriedAPIKeyPerRequest verifies that tried endpoint
// tracking is per-request, so concurrent requests for the same session don't
// share tried state for API keys.
func TestSelectEndpointSkipsTriedAPIKeyPerRequest(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"multi": {
				URL:        "https://multi",
				APIKeyEnvs: []string{"K1", "K2", "K3"},
			},
		},
		Models: map[string]ModelConfig{
			"smart": {
				Chain: []ModelEndpoint{
					{Provider: "multi", Model: "model-a", Intelligence: f64(20)},
				},
			},
		},
	}

	r := NewRouter(cfg, "")
	defer r.Close()

	sessionID := "shared-session"

	// Request 1 tries index 0, marks it as tried in its local map
	tried1 := map[string]bool{}
	ep1, _ := r.SelectEndpointForTokens("smart", sessionID, false, tried1, 1000)
	if ep1 == nil {
		t.Fatal("ep1 nil")
	}
	tried1[ep1.Key()] = true // Request 1 marks it tried

	// Request 2 (same session) should be able to pick index 0 since tried is per-request
	tried2 := map[string]bool{}
	ep2, _ := r.SelectEndpointForTokens("smart", sessionID, false, tried2, 1000)
	if ep2 == nil {
		t.Fatal("ep2 nil")
	}

	// Both requests should get the same sticky endpoint (index 0)
	if ep1.APIKeyIndex != ep2.APIKeyIndex {
		t.Errorf("concurrent same-session requests should get same sticky endpoint: ep1 idx=%d, ep2 idx=%d", ep1.APIKeyIndex, ep2.APIKeyIndex)
	}

	// Mark ep2 as tried for Request 2's fallback
	tried2[ep2.Key()] = true

	// But Request 1's fallback should skip to index 1
	ep1b, _ := r.SelectEndpointForTokens("smart", sessionID, false, tried1, 1000)
	if ep1b == nil {
		t.Fatal("ep1b nil")
	}
	if ep1b.APIKeyIndex != 1 {
		t.Errorf("Request 1 fallback should go to index 1, got %d", ep1b.APIKeyIndex)
	}

	// Request 2's fallback (if it retried) would also go to index 1 independently
	ep2b, _ := r.SelectEndpointForTokens("smart", sessionID, false, tried2, 1000)
	if ep2b == nil {
		t.Fatal("ep2b nil")
	}
	if ep2b.APIKeyIndex != 1 {
		t.Errorf("Request 2 fallback should go to index 1, got %d", ep2b.APIKeyIndex)
	}
}
