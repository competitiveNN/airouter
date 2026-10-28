package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// releasedPortUrl returns the URL of a port with nothing listening on it, so
// upstream dials against it produce a refused-connection error rather than a
// tidy HTTP response.
func releasedPortUrl(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + addr
}

func faultGateway(t *testing.T, cfg *Config) *GatewayContext {
	t.Helper()
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	proxy := NewProxy(cfg)
	return NewGatewayContext(router, proxy, cfg, "", "", true)
}

// TestE2EDialRefusedNeverReturns500 -- the first upstream refuses the dial but
// the chain has a healthy fallback; the client gets the healthy answer, never a
// 500 or a panicked mid-run 500.
func TestE2EDialRefusedNeverReturns500(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"dead":    {URL: releasedPortUrl(t)},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "dead", Model: "m-dead"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client saw 500 on transport refusal: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected healthy answer, got: %s", rec.Body.String())
	}
}

// TestE2DAllDeadNeverReturns500 -- every upstream is unreachable; the gateway's
// own surfaced terminals (503/502/429-era) are acceptable, but never a raw 500 or
// panic-derived 500.
func TestE2DAllDeadNeverReturns500(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"d1": {URL: releasedPortUrl(t)},
			"d2": {URL: releasedPortUrl(t)},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "d1", Model: "m1"},
				{Provider: "d2", Model: "m2"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client saw 500 when all upstreams dead: %s", rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable &&
		rec.Code != http.StatusBadGateway &&
		rec.Code != http.StatusTooManyRequests &&
		rec.Code != 499 {
		t.Errorf("unexpected terminal status %d: %s", rec.Code, rec.Body.String())
	}
}

// TestE2EStreamingResetReplays -- first upstream writes partial content then
// drops the connection mid-stream; the gateway resends the partial via the next
// upstream and finishes with [DONE].
func TestE2EStreamingResetReplays(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part \"},\"finish_reason\":null}]}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		conn, ok := w.(http.Hijacker)
		if ok {
			c, _, _ := conn.Hijack()
			c.Close()
		}
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}))
	defer second.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"first":  {URL: first.URL},
			"second": {URL: second.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "first", Model: "m-first"},
				{Provider: "second", Model: "m-second"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client saw 500 mid-stream: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("client never got a terminal sentinel: %s", rec.Body.String())
	}
}

// TestE2EResponsesSurfaceNeverReturns500 mirrors the transport-failure case on
// the OpenAI Responses API surface, which the gateway translates into chat.
func TestE2EResponsesSurfaceNeverReturns500(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"dead":    {URL: releasedPortUrl(t)},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "dead", Model: "m-dead"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"smart","input":"hi"}`))
	rec := httptest.NewRecorder()
	gw.HandleResponses(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("Responses surface saw 500: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
}


// TestE2EResponsesSurfaceStreamingNeverReturns500 mirrors the mid-stream reset
// on the Responses surface. The responses stream path buffers before committing,
// so a failure before any byte leaves the handler must simply fall through to
// the next model and emit a clean stream, never a 500.
func TestE2EResponsesSurfaceStreamingNeverReturns500(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part\"},\"finish_reason\":null}]}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		conn, ok := w.(http.Hijacker)
		if ok {
			c, _, _ := conn.Hijack()
			c.Close()
		}
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}]}`))
	}))
	defer second.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"first":  {URL: first.URL},
			"second": {URL: second.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "first", Model: "m-first"},
				{Provider: "second", Model: "m-second"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"smart","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	gw.HandleResponses(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("Responses streaming surface saw 500: %s", rec.Body.String())
	}
}
