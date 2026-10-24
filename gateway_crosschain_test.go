package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file pins the cross-chain failover END TO END: through the real
// request handler, not just through the router's selection function.
//
// The scenario is the one that actually happens. The `smart` chain's backend
// starts failing, the handler cools that endpoint down, and the request is
// still owed an answer. The session is then tied to a chain that has nothing
// left in it, which used to be a dead end: the request failed with
// "all models unavailable" even though the `work` chain was serving traffic
// normally. Now the session moves to `work` and the client gets its answer.

// TestGatewayServesFromAnotherChainWhenTheRequestedChainFails drives the real
// chat handler: the `smart` backend fails, and the request must still be
// answered by the `work` chain.
func TestGatewayServesFromAnotherChainWhenTheRequestedChainFails(t *testing.T) {
	// The `smart` chain's only backend is down.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	// The `work` chain's backend answers.
	var servedBy string
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedBy = "work"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"work-model",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"served by the other chain"},` +
			`"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"dead":    {URL: dead.URL},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "dead", Model: "dead-model"}}},
			"work":  {Chain: []ModelEndpoint{{Provider: "healthy", Model: "work-model"}}},
		},
	}
	router := NewRouter(cfg, "")
	defer router.Close()
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: the session must be served by the other chain, body: %s", rec.Code, rec.Body.String())
	}
	if servedBy != "work" {
		t.Fatalf("expected the request to be served by the `work` chain, got %q", servedBy)
	}
	if !strings.Contains(rec.Body.String(), "served by the other chain") {
		t.Fatalf("expected the healthy chain's answer, got %s", rec.Body.String())
	}

	// The swap must be durable: the session now lives on the chain that
	// served it, so the NEXT turn does not re-enter the failing chain.
	sessionID := bodySessionID([]byte(body))
	router.mu.Lock()
	chain := router.sessions[sessionID].chain
	router.mu.Unlock()
	if chain != "work" {
		t.Fatalf("expected the session to be pinned to the `work` chain, got %q", chain)
	}
}

// TestGatewayKeepsServingFromTheSubstituteChain drives the SECOND turn of the
// same session: it must be served by the substitute chain too, without
// re-entering the chain whose backend is failing.
func TestGatewayKeepsServingFromTheSubstituteChain(t *testing.T) {
	var deadCalls, healthyCalls int
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"work-model",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"dead":    {URL: dead.URL},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "dead", Model: "dead-model"}}},
			"work":  {Chain: []ModelEndpoint{{Provider: "healthy", Model: "work-model"}}},
		},
	}
	router := NewRouter(cfg, "")
	defer router.Close()
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)

	// The same session id on both turns, so the second turn is the SAME
	// conversation and must not re-descend into the failing chain.
	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"session_id":"conv-1"}`
	for turn := 1; turn <= 2; turn++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		gateway.HandleChatCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d: expected 200, got %d: %s", turn, rec.Code, rec.Body.String())
		}
	}

	if healthyCalls != 2 {
		t.Fatalf("expected the substitute chain to serve both turns, got %d healthy calls", healthyCalls)
	}
	// The failing chain is tried once (it was healthy enough to be picked on
	// the first turn) and then cooled down. If the session re-entered it on
	// the second turn, the dead backend would have been called twice.
	if deadCalls != 1 {
		t.Fatalf("expected the failing chain to be tried once, got %d calls: the session re-entered the dead chain", deadCalls)
	}
}
