package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deadProxyInfo returns a ProxyInfo whose ports refuse every dial, so a request
// routed through it fails at the transport-layer CONNECT the proxy retry loop
// classifies as a ProxyError. Generating the refusal beats fabricating it: the
// error shape Go records for a refused CONNECT is the whole reason proxyFault
// exists (see deadProxyClient in proxy_fault_test.go).
func deadProxyInfo(t *testing.T) *ProxyInfo {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Release the port so nothing answers on it. A listener that accepts and
	// closes produces an EOF instead -- a different shape.
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return &ProxyInfo{
		URL:      "http://" + addr,
		Protocol: "https",
		IP:       host,
		Port:     port,
	}
}

// responsesProxyFixture wires a two-endpoint chain in front of a dead proxy
// pool, exactly as the chat proxy-fault tests do: the first provider carries
// the proxy budget so the router routes through the pool first, and the second
// is a healthy direct endpoint the request must resume on.
//
// The two endpoints are what makes the endpoint-cooldown assertion meaningful.
// With one endpoint, SelectEndpoint's "retry exhausted" path re-picks it, the
// pool is then empty (the only proxy is in cooldown), the attempt goes direct,
// and the endpoint legitimately fails on its own merits -- so asserting it
// stays cool would be asserting behaviour the code does not promise.
func responsesProxyFixture(t *testing.T, first http.HandlerFunc, second http.HandlerFunc) (*httptest.Server, *Router, *ProxyService, *ProxyInfo) {
	t.Helper()
	dead := deadProxyInfo(t)
	svc := NewProxyService("", time.Minute)
	svc.pool.set([]*ProxyInfo{dead})

	backend1 := httptest.NewServer(first)
	t.Cleanup(backend1.Close)
	backend2 := httptest.NewServer(second)
	t.Cleanup(backend2.Close)

	noRotation := 0
	cfg := &Config{
		// Rotation is disabled so the chain is walked in strict order and the
		// first pick is deterministically the proxied endpoint. The default
		// window is 8, which randomizes the pick across both entries and makes
		// this fixture flaky: many runs would start on the direct endpoint and
		// never touch the proxy the fixture exists to exercise.
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Providers: map[string]ProviderConfig{
			"proxied": {URL: backend1.URL, APIKeyEnv: "TEST_KEY", Proxy: 1},
			"direct":  {URL: backend2.URL, APIKeyEnv: "TEST_KEY"},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "proxied", Model: "m1"},
				{Provider: "direct", Model: "m2"},
			}},
		},
	}
	t.Setenv("TEST_KEY", "sk-test")

	proxy := NewProxy(cfg)
	proxy.SetProxyService(svc)
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	g := NewGatewayContext(router, proxy, cfg, "", testGatewayKey, false)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", g.HandleResponses)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, router, svc, dead
}

// chatReply encodes one chat-completion reply carrying text, so a backend can
// answer the Responses surface after the proxy fault.
func chatReply(id, text string) []byte {
	b, err := json.Marshal(ChatCompletionResponse{
		ID: id, Object: "chat.completion", Created: 1,
		Choices: []ChatCompletionChoice{{Message: ChatCompletionMessage{Role: "assistant", Content: text}}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// writeSSEChatReply streams one chat-completion chunk set carrying text, so the
// streaming Responses surface can be answered by a healthy backend.
func writeSSEChatReply(w http.ResponseWriter, id, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\","+
		"\"created\":1,\"model\":\"m2\",\"choices\":[{\"index\":0,"+
		"\"delta\":{\"role\":\"assistant\",\"content\":%q},"+
		"\"finish_reason\":null}]}\n\n", id, text)
	if flusher != nil {
		flusher.Flush()
	}
	fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\","+
		"\"created\":1,\"model\":\"m2\",\"choices\":[{\"index\":0,"+
		"\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", id)
	if flusher != nil {
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// TestResponsesCompletionProxyFaultCoolsProxyNotEndpoint pins the non-streaming
// Responses path.
//
// Before the fix it unconditionally cooled the endpoint on every error, so a
// dead proxy charged a healthy provider, and it had no retry loop at all. A
// proxy fault must land on the proxy -- which is then skipped -- and leave the
// endpoint behind it out of the penalty, because it never failed.
func TestResponsesCompletionProxyFaultCoolsProxyNotEndpoint(t *testing.T) {
	var firstHits int32
	second := chatReply("c2", "recovered")
	// firstHits must stay 0: the proxy dies before the request reaches the
	// origin, so a hit means the attempt left the proxy path.
	srv, router, svc, dead := responsesProxyFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&firstHits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.Write(chatReply("c1", "first"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(second)
		})

	resp := postResponses(t, srv, `{"model":"smart","input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (the request must survive the proxy fault): %s", resp.StatusCode, readAllString(t, resp))
	}
	var env responseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if env.Output[0].Content[0].Text != "recovered" {
		t.Errorf("text = %q, want recovered (the second model's answer)", env.Output[0].Content[0].Text)
	}

	if !svc.isDown(dead.URL) {
		t.Errorf("dead proxy %s was not cooled down after its CONNECT was refused", dead.URL)
	}
	if got := atomic.LoadInt32(&firstHits); got != 0 {
		t.Errorf("the proxied endpoint was hit %d times, want 0 -- the request left the proxy path", got)
	}
	assertProxyFaultEndpointsNotCooled(t, router)
}

// TestResponsesStreamProxyFaultCoolsProxyNotEndpoint pins the streaming Responses
// path, which had no ProxyError handling at all: it cooled the endpoint on a
// proxy fault and never retried with a different proxy. It also had no proxy
// retry loop, and streamResponsesAttempt dialed direct with the shared client,
// silently bypassing the pool -- which is why the first backend must stay cold.
func TestResponsesStreamProxyFaultCoolsProxyNotEndpoint(t *testing.T) {
	var firstHits int32
	srv, router, svc, dead := responsesProxyFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&firstHits, 1)
			writeSSEChatReply(w, "c1", "first")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writeSSEChatReply(w, "c2", "recovered")
		})

	req, err := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(`{"model":"smart","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (the request must survive the proxy fault): %s", resp.StatusCode, readAllString(t, resp))
	}
	body := readAllString(t, resp)

	// The fault lands before anything is written to the client, so the failed
	// attempt contributes no lifecycle of its own: exactly one sequence.
	if n := strings.Count(body, "event: "+evResponseCreated); n != 1 {
		t.Errorf("response.created count = %d, want 1 (the failed attempt must not leak a second lifecycle); got:\n%s", n, body)
	}
	if !strings.Contains(body, "recovered") {
		t.Errorf("expected the resumed stream's content; got:\n%s", body)
	}
	// "first" belongs to the endpoint behind the dead proxy. Seeing it means
	// the request left the proxy path and hit that origin directly.
	if strings.Contains(body, "first") {
		t.Errorf("stream contains the proxied endpoint's content, so the attempt bypassed the proxy; got:\n%s", body)
	}

	if !svc.isDown(dead.URL) {
		t.Errorf("dead proxy %s was not cooled down after its CONNECT was refused", dead.URL)
	}
	if got := atomic.LoadInt32(&firstHits); got != 0 {
		t.Errorf("the proxied endpoint was hit %d times, want 0 -- the request left the proxy path", got)
	}
	assertProxyFaultEndpointsNotCooled(t, router)
}

// assertProxyFaultEndpointsNotCooled checks that neither chain member took a
// cooldown. The endpoint behind the dead proxy never failed, and the endpoint
// that answered succeeded, so any cooldown on either means the penalty landed on
// the wrong party. Cooldowns are keyed by endpoint key, so naming either model
// means the fault was charged to a healthy model.
func assertProxyFaultEndpointsNotCooled(t *testing.T, router *Router) {
	t.Helper()
	for key, cd := range router.GetAllCooldowns() {
		if strings.Contains(key, "proxied") || strings.Contains(key, "m1") {
			t.Errorf("endpoint %s was cooled down for a proxy failure: %+v", key, cd)
		}
	}
}
