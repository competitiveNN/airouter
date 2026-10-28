package main

// Context-window eligibility tests.
//
// The router used to hand any endpoint to any request, whatever its advertised
// window. A prompt larger than the chain head's window then produced exactly
// the failure mode the fallback machinery exists to prevent: the upstream
// accepted the connection and stayed silent, the attempt timeout fired, the
// interruption budget was spent on one or two attempts that could never
// succeed, and the client got an error while endpoints that COULD hold the
// prompt sat lower in the chain untouched.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestContextFits(t *testing.T) {
	ep := func(ctx int) *ModelEndpoint { return &ModelEndpoint{Provider: "p", Model: "m", ContextLength: ctx} }
	cases := []struct {
		name   string
		window int
		tokens int
		want   bool
	}{
		{"unknown request size fits", 131072, 0, true},
		{"unknown window stays eligible", 0, 1_000_000, true},
		{"window exactly the prompt", 131072, 131072, true},
		{"prompt past the window", 131072, 131073, false},
		{"prompt inside the window", 1_000_000, 340000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextFits(ep(tc.window), tc.tokens); got != tc.want {
				t.Fatalf("contextFits(window=%d, tokens=%d) = %v, want %v", tc.window, tc.tokens, got, tc.want)
			}
		})
	}
}

func contextTestRouter(t *testing.T, chains map[string][]ModelEndpoint) *Router {
	t.Helper()
	cfg := &Config{Providers: map[string]ProviderConfig{}, Models: map[string]ModelConfig{}}
	for name, chain := range chains {
		cfg.Models[name] = ModelConfig{Chain: chain}
	}
	for _, chain := range chains {
		for _, ep := range chain {
			if _, ok := cfg.Providers[ep.Provider]; !ok {
				cfg.Providers[ep.Provider] = ProviderConfig{URL: "http://127.0.0.1:1"}
			}
		}
	}
	r := NewRouter(cfg, "")
	t.Cleanup(r.Close)
	return r
}

// The head of the chain must be skipped when its window cannot hold the prompt,
// and the first endpoint that CAN hold it must be chosen instead.
func TestSelectEndpointSkipsTooSmallContext(t *testing.T) {
	small := ModelEndpoint{Provider: "p", Model: "small", ContextLength: 131072}
	big := ModelEndpoint{Provider: "p", Model: "big", ContextLength: 1000000}
	r := contextTestRouter(t, map[string][]ModelEndpoint{
		"smart": {small, big},
	})

	// A small request still prefers the head of the chain.
	got, _ := r.SelectEndpointForTokens("smart", "s-small", false, map[string]bool{}, 1000)
	if got == nil || got.Model != "small" {
		t.Fatalf("small request chose %+v, want the chain head (small)", got)
	}

	got, _ = r.SelectEndpointForTokens("smart", "s-big", false, map[string]bool{}, 340000)
	if got == nil || got.Model != "big" {
		t.Fatalf("large request chose %+v, want big (the only window that fits)", got)
	}
}

// An endpoint with no advertised window cannot be proven too small, so it stays
// eligible: dropping it would silently remove every model the fetcher has no
// metadata for.
func TestSelectEndpointUnknownWindowStaysEligible(t *testing.T) {
	unknown := ModelEndpoint{Provider: "p", Model: "unknown"}
	big := ModelEndpoint{Provider: "p", Model: "big", ContextLength: 1000000}
	r := contextTestRouter(t, map[string][]ModelEndpoint{"smart": {unknown, big}})

	got, _ := r.SelectEndpointForTokens("smart", "s", false, map[string]bool{}, 5_000_000)
	if got == nil || got.Model != "unknown" {
		t.Fatalf("chose %+v, want the head (unknown window is not a verdict)", got)
	}
}

// When the requested chain cannot hold the prompt, the existing cross-chain
// substitution must take over rather than the request failing.
func TestSelectEndpointSubstitutesWhenChainTooSmall(t *testing.T) {
	small := ModelEndpoint{Provider: "p", Model: "work-only-131k", ContextLength: 131072}
	big := ModelEndpoint{Provider: "p", Model: "large-1m", ContextLength: 1000000}
	r := contextTestRouter(t, map[string][]ModelEndpoint{
		"work":  {small},
		"large": {big},
	})

	got, _ := r.SelectEndpointForTokens("work", "s", false, map[string]bool{}, 340000)
	if got == nil {
		t.Fatal("no endpoint selected; the large chain should have been substituted in")
	}
	if got.Model != "large-1m" {
		t.Fatalf("chose %+v, want the substitute chain's endpoint", got)
	}
	// The swap is durable: the session now lives on the substitute chain.
	pinned, ok := r.GetSession("s")
	if !ok || pinned.Model != "large-1m" {
		t.Fatalf("session not re-pinned to the substitute: %+v (found=%v)", pinned, ok)
	}
}

// ContextCapacity answers "is this request possible here at all", across every
// chain the walk may substitute into, and does not care about cooldowns.
func TestContextCapacity(t *testing.T) {
	small := ModelEndpoint{Provider: "p", Model: "small", ContextLength: 131072}
	big := ModelEndpoint{Provider: "p", Model: "big", ContextLength: 1000000}
	r := contextTestRouter(t, map[string][]ModelEndpoint{
		"work":  {small},
		"large": {big},
	})

	if fits, largest := r.ContextCapacity("work", 340000); !fits || largest != 1000000 {
		t.Fatalf("ContextCapacity(work, 340k) = (%v, %d), want (true, 1000000) via the large chain", fits, largest)
	}
	if fits, largest := r.ContextCapacity("work", 2_000_000); fits || largest != 1000000 {
		t.Fatalf("ContextCapacity(work, 2M) = (%v, %d), want (false, 1000000)", fits, largest)
	}

	// Cool every endpoint down: the request is still possible, just not now.
	r.ApplyCooldownForSession(&small, 429, "rate limited", "", 0)
	r.ApplyCooldownForSession(&big, 429, "rate limited", "", 0)
	if fits, _ := r.ContextCapacity("work", 340000); !fits {
		t.Fatal("a transient cooldown must not make a possible request look impossible")
	}
}

// A request nobody can hold is rejected up front with a 400 that names the
// largest window, instead of spending the interruption budget and failing.
func TestChatRejectsOversizedContext(t *testing.T) {
	var upstreamCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(backend.Close)

	cfg := &Config{
		Providers: map[string]ProviderConfig{"p": {URL: backend.URL, APIKeyEnv: "TEST_KEY"}},
		Models: map[string]ModelConfig{
			"work": {Chain: []ModelEndpoint{
				{Provider: "p", Model: "m", ContextLength: 131072},
			}},
		},
	}
	t.Setenv("TEST_KEY", "sk-test")
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	proxy := NewProxy(cfg)
	proxy.SetProtocolResolver(router.ProtocolFor)
	g := NewGatewayContext(router, proxy, cfg, "", testGatewayKey, false)

	srv := httptest.NewServer(http.HandlerFunc(g.HandleChatCompletions))
	t.Cleanup(srv.Close)

	// 300k characters of prompt: about 150k estimated tokens, past the only
	// window in the config (131072).
	body, err := json.Marshal(map[string]any{
		"model":    "work",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": strings.Repeat("x", 300000)}},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "context_length_exceeded") {
		t.Fatalf("error code missing: %s", out)
	}
	if !strings.Contains(string(out), "131072") {
		t.Fatalf("error should name the largest available window: %s", out)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream was called %d times; an impossible request must not be sent", upstreamCalls)
	}
}

// The guard must not fire for a request that fits: the same config with a short
// prompt has to reach the upstream normally.
func TestChatAcceptsRequestThatFits(t *testing.T) {
	var upstreamCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(backend.Close)

	cfg := &Config{
		Providers: map[string]ProviderConfig{"p": {URL: backend.URL, APIKeyEnv: "TEST_KEY"}},
		Models: map[string]ModelConfig{
			"work": {Chain: []ModelEndpoint{{Provider: "p", Model: "m", ContextLength: 131072}}},
		},
	}
	t.Setenv("TEST_KEY", "sk-test")
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	proxy := NewProxy(cfg)
	proxy.SetProtocolResolver(router.ProtocolFor)
	g := NewGatewayContext(router, proxy, cfg, "", testGatewayKey, false)
	// Keep the test fast if the attempt ever fails.
	g.SetTestCooldown(time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(g.HandleChatCompletions))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader(`{"model":"work","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}
}

// A session that starts on a small-window endpoint and then grows past that
// window must move to an endpoint that fits. This is the shape of a long
// conversation outgrowing the chain head mid-session, and it is why the sticky
// fast path has to consult the window too.
func TestStickySessionMovesWhenPromptOutgrowsWindow(t *testing.T) {
	small := ModelEndpoint{Provider: "p", Model: "small", ContextLength: 131072}
	big := ModelEndpoint{Provider: "p", Model: "big", ContextLength: 1000000}
	r := contextTestRouter(t, map[string][]ModelEndpoint{"smart": {small, big}})

	first, _ := r.SelectEndpointForTokens("smart", "s", false, map[string]bool{}, 1000)
	if first == nil || first.Model != "small" {
		t.Fatalf("initial pick = %+v, want the chain head (small)", first)
	}
	got, _ := r.SelectEndpointForTokens("smart", "s", false, map[string]bool{}, 340000)
	if got == nil || got.Model != "big" {
		t.Fatalf("grown session chose %+v, want big (the pinned head no longer fits)", got)
	}
	pinned, ok := r.GetSession("s")
	if !ok || pinned.Model != "big" {
		t.Fatalf("session still pinned to %+v (found=%v), want big", pinned, ok)
	}
}

// New-session rotation derives its starting depth from the session ID, so a
// prompt that only one endpoint can hold must still be handed to that endpoint
// for EVERY session: a rotation filter that ignored the window would send some
// of them straight to the too-small head.
func TestRotationNeverPicksTooSmallHead(t *testing.T) {
	small := ModelEndpoint{Provider: "p", Model: "small", ContextLength: 131072}
	big := ModelEndpoint{Provider: "p", Model: "big", ContextLength: 1000000}
	r := contextTestRouter(t, map[string][]ModelEndpoint{"smart": {small, big}})

	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("rot-%d", i)
		got, _ := r.SelectEndpointForTokens("smart", id, false, map[string]bool{}, 340000)
		if got == nil || got.Model != "big" {
			t.Fatalf("session %s chose %+v, want big", id, got)
		}
	}
}

// The Responses surface must reject an impossible prompt the same way the chat
// surface does, before any upstream call.
func TestResponsesRejectsOversizedContext(t *testing.T) {
	var upstreamCalls int32
	srv := responsesGatewayWith(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","object":"response","status":"completed","output":[]}`))
	}, nil, ModelEndpoint{Provider: "openai", Model: "m", ContextLength: 131072})

	body := `{"model":"smart","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` +
		strings.Repeat("x", 300000) + `"}]}]}`
	resp := postResponses(t, srv, body)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "context_length_exceeded") {
		t.Fatalf("error code missing: %s", out)
	}
	if !strings.Contains(string(out), "131072") {
		t.Fatalf("error should name the largest available window: %s", out)
	}
	if n := atomic.LoadInt32(&upstreamCalls); n != 0 {
		t.Fatalf("upstream was called %d times; an impossible request must not be sent", n)
	}
}
