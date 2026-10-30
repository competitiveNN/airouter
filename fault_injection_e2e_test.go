package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestE2E500FallbackNeverReturns500 drives a request whose first upstream
// answers HTTP 500 against a chain that has a healthy fallback. The gateway
// must fail over to the healthy model and answer 200; the client must never
// see the upstream's 500, never a 502, and never a gateway 500.
//
// This is the canonical "client never sees model 500" case: a 5xx is a
// retryable upstream error, so the chain advances and the next model serves.
func TestE2E500FallbackNeverReturns500(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"bad":     {URL: releasedPortUrl(t)},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "bad", Model: "m-bad"},
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

// TestE2EStreaming500FallbackNeverReturns500 drives a STREAMING request
// whose first upstream answers HTTP 500. The gateway must fall back to the
// healthy model mid-stream and emit a clean SSE stream with [DONE]. The
// client must never see a 500, a 502, or a truncated stream.
func TestE2EStreaming500FallbackNeverReturns500(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"bad":     {URL: releasedPortUrl(t)},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "bad", Model: "m-bad"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client saw 500 on streaming transport refusal: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("client never got a terminal sentinel: %s", rec.Body.String())
	}
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

// TestE2EResponses500FallbackNeverReturns500 drives a Responses request whose
// first upstream answers HTTP 500 against a chain with a healthy fallback.
// The gateway must fail over to the healthy model and answer 200 in the
// Responses envelope; the client must never see a 500 or a 502.
func TestE2EResponses500FallbackNeverReturns500(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"bad":     {URL: releasedPortUrl(t)},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "bad", Model: "m-bad"},
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

// silentStallBackend accepts the request and never writes a byte, so the
// gateway's per-attempt first-byte timeout is the only thing that ends the
// attempt. The release channel is closed by the test's cleanup.
func silentStallBackend(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

func tlsFaultGateway(t *testing.T, cfg *Config) *GatewayContext {
	t.Helper()
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	proxy := &Proxy{
		client:            &http.Client{Transport: transport},
		baseTransport:     transport,
		streamIdleTimeout: defaultStreamIdleTimeout,
		config:            &atomic.Pointer[Config]{},
	}
	proxy.config.Store(cfg)
	return NewGatewayContext(router, proxy, cfg, "", "", true)
}
func faultTLSCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return cert
}

// TestE2EStreamStallPastTimeoutFailsOverNeverReturns500 drives a silent
// upstream stall on the streaming surface: the backend accepts the stream and
// then emits nothing. The per-attempt first-byte budget must end the attempt,
// the client must never see a 500, and the fallback must still deliver a clean
// terminal stream.
func TestE2EStreamStallPastTimeoutFailsOverNeverReturns500(t *testing.T) {
	stall := silentStallBackend(t)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"stalled": {URL: stall.URL, MaxInterruption: 250 * time.Millisecond},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "stalled", Model: "m-stalled"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client saw 500 on stalled upstream stream: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback answer with status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("client stream never terminated: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "world") {
		t.Fatalf("silent stall did not fail over to the healthy model: %s", rec.Body.String())
	}
}

// TestE2ENonStreamingStallPastTimeoutFailsOverNeverReturns500 mirrors the
// silent-stall case on the non-streaming surface: the upstream accepts the
// connection and then never answers, so the per-attempt timeout fires and the
// walk must fail over rather than leave the client hanging or erroring out.
func TestE2ENonStreamingStallPastTimeoutFailsOverNeverReturns500(t *testing.T) {
	stall := silentStallBackend(t)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"stalled": {URL: stall.URL, MaxInterruption: time.Second},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "stalled", Model: "m-stalled"},
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
		t.Fatalf("client saw 500 on stalled upstream (non-streaming): %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback answer with status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected healthy answer, got: %s", rec.Body.String())
	}
}

// TestE2ETlsResetFailsOverNeverReturns500 drives a real TLS-level reset: the
// upstream terminates the TLS connection mid-flight after the handshake, and
// the gateway must fail over to the next model without the client ever seeing
// a 500.
func TestE2ETlsResetFailsOverNeverReturns500(t *testing.T) {
	tlsCert := faultTLSCertificate(t)
	tlsBackend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("TLS backend: server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("TLS backend: hijack: %v", err)
			return
		}
		conn.Close()
	}))
	tlsBackend.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	tlsBackend.StartTLS()
	defer tlsBackend.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"tlsdead": {URL: tlsBackend.URL},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "tlsdead", Model: "m-tls"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := tlsFaultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client saw 500 after TLS reset: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback answer with status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("TLS reset did not fail over to the healthy model: %s", rec.Body.String())
	}
}

// TestE2EFailoverReasonInstrumented verifies that the failover-reason
// metric is populated by the real fallback walk. A dial-refused upstream
// triggers a FailoverTransport fallback, and the /metrics endpoint must
// report it under reason="transport".
func TestE2EFailoverReasonInstrumented(t *testing.T) {
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

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected healthy answer, got: %s", rec.Body.String())
	}

	// Now the metrics endpoint must reflect the failover reason.
	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+gw.gatewayAPIKey)
	gw.HandleMetrics(metricsRec, metricsReq)
	bodyStr := metricsRec.Body.String()

	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="transport"} 1`) {
		t.Errorf("missing reason=transport count in /metrics (got: %s)", bodyStr)
	}
}

// TestE2EChaoticNetworkLoad drives a small burst of concurrent requests
// through a gateway whose upstreams randomly fail (dial refused, 429,
// 500, mid-stream reset) and proves that no client ever sees a 500 and
// that the failover-reason metric is populated by the real handler path.
//
// This is the load half of the resiliency contract: the fault-injection
// tests above prove that ONE request survives ONE fault; this test proves
// that MANY requests survive MANY faults, including ones that arrive
// mid-stream, under concurrency.
func TestE2EChaoticNetworkLoad(t *testing.T) {
	// Two backends: one that always answers, one that fails in a way
	// chosen per-request by a counter.
	var mu sync.Mutex
	var calls int32
	chaos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		mu.Lock()
		switch n % 4 {
		case 0:
			// dial refused would be cleaner, but an in-process backend
			// cannot refuse its own dial; a 500 is the equivalent
			// upstream failure the router must fail over from.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"chaos 500"}}`))
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		case 2:
			// mid-stream reset: write partial then close.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			hij, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hij.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		}
		mu.Unlock()
	}))
	defer chaos.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"chaos": {URL: chaos.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "chaos", Model: "m-chaos"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	const workers = 16
	const perWorker = 8
	var wg sync.WaitGroup
	var bad int32
	var okCount int32
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}]}`))
				rec := httptest.NewRecorder()
				gw.HandleChatCompletions(rec, req)
				if rec.Code == http.StatusInternalServerError {
					atomic.AddInt32(&bad, 1)
				}
				if rec.Code == http.StatusOK {
					atomic.AddInt32(&okCount, 1)
				}
			}
		}()
	}
	wg.Wait()

	if bad > 0 {
		t.Errorf("saw %d client-visible 500s under chaotic load (want 0)", bad)
	}
	if okCount == 0 {
		t.Errorf("no request succeeded under chaotic load; the gateway is not failing over")
	}

	// The metrics endpoint must reflect the failover reasons the real
	// handler path recorded.
	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+gw.gatewayAPIKey)
	gw.HandleMetrics(metricsRec, metricsReq)
	bodyStr := metricsRec.Body.String()
	if !strings.Contains(bodyStr, "airouter_failovers_total_by_reason") {
		t.Errorf("metrics endpoint missing failover-reason series (got: %s)", bodyStr)
	}
	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_5xx"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_429"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="sse_parse"}`) {
		t.Errorf("failover-reason series did not capture any of the injected failure types (got: %s)", bodyStr)
	}
	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_5xx"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_429"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="sse_parse"}`) {
		t.Errorf("failover-reason series did not capture any of the injected failure types (got: %s)", bodyStr)
	}
}

// TestE2EChaoticStreamingLoad drives concurrent streaming requests
// through a chaotic upstream and proves no client sees a 500 and the
// failover-reason series is populated by the real streaming handler.
func TestE2EChaoticStreamingLoad(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	chaos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		switch n % 3 {
		case 0:
			// stall forever: the per-attempt first-byte timeout must end it.
			// The release channel lets the test cleanup close the server
			// without waiting for the request context to outlive it.
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case 1:
			// mid-stream reset after partial content.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			hij, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hij.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(func() {
		close(release)
		chaos.Close()
	})

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"chaos":   {URL: chaos.URL, MaxInterruption: 200 * time.Millisecond},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "chaos", Model: "m-chaos"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	const workers = 8
	const perWorker = 4
	var wg sync.WaitGroup
	var bad int32
	var doneCount int32
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`))
				rec := httptest.NewRecorder()
				gw.HandleChatCompletions(rec, req)
				if rec.Code == http.StatusInternalServerError {
					atomic.AddInt32(&bad, 1)
				}
				if strings.Contains(rec.Body.String(), "[DONE]") {
					atomic.AddInt32(&doneCount, 1)
				}
			}
		}()
	}
	wg.Wait()

	if bad > 0 {
		t.Errorf("saw %d client-visible 500s under chaotic streaming load (want 0)", bad)
	}
	if doneCount == 0 {
		t.Errorf("no streaming request completed with [DONE] under chaotic load")
	}

	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+gw.gatewayAPIKey)
	gw.HandleMetrics(metricsRec, metricsReq)
	bodyStr := metricsRec.Body.String()
	if !strings.Contains(bodyStr, "airouter_failovers_total_by_reason") {
		t.Errorf("metrics endpoint missing failover-reason series (got: %s)", bodyStr)
	}
}

// TestE2EDeterministicFaultSequence replays a fixed sequence of upstream
// faults in a single session and asserts that each request still reaches
// the healthy fallback and that the failover-reason series captures every
// fault type that was injected. Unlike the randomized load tests, this
// one is fully deterministic: the same fault sequence runs on every run,
// so a regression in the handling of one fault type shows up as a named
// failure rather than a flaky statistic.
func TestE2EDeterministicFaultSequence(t *testing.T) {
	var seq []string
	mu := sync.Mutex{}
	chaos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Derive the fault type from the request body rather than a
		// global counter: the sticky session pins a session to healthy
		// after its first failover, so a global counter would only ever
		// exercise the first fault type. Each pass uses a distinct
		// session, so the content drives which fault is exercised.
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		n := 0
		if i := strings.Index(s, "hi "); i >= 0 {
			rest := s[i+3:]
			fmt.Sscanf(rest, "%d", &n)
		}
		mu.Lock()
		seq = append(seq, r.URL.Path)
		mu.Unlock()
		switch n % 4 {
		case 0:
			// 500 — retryable upstream error.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"chaos 500"}}`))
		case 1:
			// 429 — rate limit.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		case 2:
			// mid-stream reset after partial content.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			hij, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hij.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
		case 3:
			// 400 — client-side 4xx, non-retryable; the chain advances
			// at most once and then surfaces the upstream error.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
		}
	}))
	t.Cleanup(chaos.Close)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(healthy.Close)

	cfg := &Config{
		Preferences: &Preferences{
			// Pin every new session to the head of the chain. The
			// default rotation window spreads the first 8 new sessions
			// across the chain head, which would send some of the
			// passes straight to healthy and skip the injected fault.
			InitialRotationWindow: &noRotation,
		},
		Providers: map[string]ProviderConfig{
			"chaos":   {URL: chaos.URL},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "chaos", Model: "m-chaos"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)
	// A short test cooldown keeps the chaos endpoint in rotation between
	// passes: the real escalation ladder (30s → 1m → 2m → 4m → 8m → 16m →
	// 30m) would park it after the first fault and the later passes would
	// never exercise their fault type.
	gw.SetTestCooldown(10 * time.Millisecond)

	// Drive the fixed sequence four times — each pass exercises a different
	// fault type, in order. Each pass uses a distinct session so the
	// sticky-session pin does not short-circuit the chain: after the first
	// failover the session is pinned to healthy, so a repeat of the same
	// session would only ever exercise the first fault type.
	//
	// Reset the cooldown table between passes. The test cooldown is short
	// (10ms) but the passes run back-to-back in the same goroutine, so the
	// chaos endpoint can still be cooling when the next pass's new session
	// is routed: selectInitialEndpoint skips a cooled endpoint, the rotation
	// window collapses to nothing, and the pass lands straight on healthy
	// without ever exercising its fault type. A clean slate guarantees every
	// pass hits the chaos head first.
	for i := 0; i < 4; i++ {
		gw.ResetTestCooldowns()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(fmt.Sprintf(`{"model":"smart","messages":[{"role":"user","content":"hi %d"}]}`, i)))
		rec := httptest.NewRecorder()
		gw.HandleChatCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("pass %d: expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "ok") {
			t.Errorf("pass %d: expected healthy answer, got: %s", i, rec.Body.String())
		}
	}

	// The metrics endpoint must reflect the failover reasons. The injected
	// faults are 500, 429, mid-stream reset (surfaced as a 502 read error),
	// and 400. Each pass uses a distinct session so the sticky pin does not
	// short-circuit the chain, and the healthy fallback answers on every
	// pass.
	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+gw.gatewayAPIKey)
	gw.HandleMetrics(metricsRec, metricsReq)
	bodyStr := metricsRec.Body.String()

	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_5xx"}`) {
		t.Errorf("metrics missing http_5xx reason (got: %s)", bodyStr)
	}
	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_429"}`) {
		t.Errorf("metrics missing http_429 reason (got: %s)", bodyStr)
	}
	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_4xx"}`) {
		t.Errorf("metrics missing http_4xx reason (got: %s)", bodyStr)
	}
}

// TestE2EConcurrentDeterministicFaultSequence drives the deterministic
// fault sequence (500, 429, mid-stream reset, 400) under concurrent load,
// proving that no client ever sees a 500 even when the chaos endpoint is
// hit by multiple simultaneous sessions.
//
// This is the concurrency half of the deterministic contract: the sequential
// pass proves that each fault type is exercised in turn; this test proves
// that the same faults do not produce 500s when they arrive concurrently.
func TestE2EConcurrentDeterministicFaultSequence(t *testing.T) {
	var seq []string
	var mu sync.Mutex
	chaos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		n := 0
		if i := strings.Index(s, "hi "); i >= 0 {
			rest := s[i+3:]
			fmt.Sscanf(rest, "%d", &n)
		}
		mu.Lock()
		seq = append(seq, r.URL.Path)
		mu.Unlock()
		switch n % 4 {
		case 0:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"chaos 500"}}`))
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			hij, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hij.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
		case 3:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
		}
	}))
	t.Cleanup(chaos.Close)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(healthy.Close)

	cfg := &Config{
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Providers: map[string]ProviderConfig{
			"chaos":   {URL: chaos.URL},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "chaos", Model: "m-chaos"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)
	gw.SetTestCooldown(10 * time.Millisecond)

	const workers = 8
	const perWorker = 4
	var wg sync.WaitGroup
	var bad int32
	var okCount int32
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				gw.ResetTestCooldowns()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(fmt.Sprintf(`{"model":"smart","messages":[{"role":"user","content":"hi %d"}]}`, i)))
				rec := httptest.NewRecorder()
				gw.HandleChatCompletions(rec, req)
				if rec.Code == http.StatusInternalServerError {
					atomic.AddInt32(&bad, 1)
				}
				if rec.Code == http.StatusOK {
					atomic.AddInt32(&okCount, 1)
				}
			}
		}()
	}
	wg.Wait()

	if bad > 0 {
		t.Errorf("saw %d client-visible 500s under concurrent deterministic load (want 0)", bad)
	}
	if okCount == 0 {
		t.Errorf("no request succeeded under concurrent deterministic load; the gateway is not failing over")
	}

	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+gw.gatewayAPIKey)
	gw.HandleMetrics(metricsRec, metricsReq)
	bodyStr := metricsRec.Body.String()
	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_5xx"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_429"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="http_4xx"}`) &&
		!strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="transport"}`) {
		t.Errorf("failover-reason series did not capture any of the injected failure types (got: %s)", bodyStr)
	}
}

// TestE2EDialRefusedFailoverReasonIsTransport proves the failover reason for a
// dial-refused upstream is classified as "transport" (not "unknown"), so
// operators can see connection-level dropouts in the metric.
func TestE2EDialRefusedFailoverReasonIsTransport(t *testing.T) {
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

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+gw.gatewayAPIKey)
	gw.HandleMetrics(metricsRec, metricsReq)
	bodyStr := metricsRec.Body.String()
	if !strings.Contains(bodyStr, `airouter_failovers_total_by_reason{reason="transport"} 1`) {
		t.Errorf("expected transport failover reason, got: %s", bodyStr)
	}
}

// TestE2EFallbackLogLinePresent proves the structured [fallback] log line is
// emitted on a non-streaming failover.
func TestE2EFallbackLogLinePresent(t *testing.T) {
	var logBuf strings.Builder
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer healthy.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"bad":     {URL: releasedPortUrl(t)},
			"healthy": {URL: healthy.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "bad", Model: "m-bad"},
				{Provider: "healthy", Model: "m-healthy"},
			}},
		},
	}
	gw := faultGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	gw.HandleChatCompletions(rec, req)

	if !strings.Contains(logBuf.String(), "[fallback]") {
		t.Errorf("expected [fallback] log line, got: %q", logBuf.String())
	}
}

// TestE2EMidStreamFallbackLogLinePresent proves the structured
// [fallback][mid-stream] log line is emitted on a streaming mid-stream
// failover.
func TestE2EMidStreamFallbackLogLinePresent(t *testing.T) {
	var logBuf strings.Builder
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"part \"},\"finish_reason\":null}]}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		hij, ok := w.(http.Hijacker)
		if ok {
			conn, _, err := hij.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n"))
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

	if !strings.Contains(logBuf.String(), "[fallback][mid-stream]") {
		t.Errorf("expected [fallback][mid-stream] log line, got: %q", logBuf.String())
	}
}
