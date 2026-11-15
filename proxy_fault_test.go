package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deadProxyClient dials a port with nothing listening on it, so the transport
// produces exactly the error shape Go records for a CONNECT it could not
// establish. Generating the error beats fabricating it: the whole bug was that
// the fabricated shape the old comment described (url.Error.Op containing
// "proxyconnect") is not the real one. Note the proxy must REFUSE the dial --
// a listener that accepts and closes produces an EOF instead, a different shape.
func deadProxyClient(t *testing.T) (*http.Client, error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	// Release the port so nothing answers on it.
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		return url.Parse("http://" + addr)
	}}
	return &http.Client{Transport: tr}, nil
}

// TestProxyConnectShapeIsWhatTheOldCodeMissed pins the misunderstanding, so the
// next person cannot "simplify" the classifier back into it.
func TestProxyConnectShapeIsWhatTheOldCodeMissed(t *testing.T) {
	c, err := deadProxyClient(t)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, err = c.Get("https://example.invalid/v1/models")
	if err == nil {
		t.Fatal("expected a failed CONNECT, got nil")
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("want url.Error, got %T", err)
	}
	// The old first branch tested this and never matched:
	if got := urlErr.Op; got != "Get" && got != "Post" {
		t.Errorf("url.Error.Op = %q, want an HTTP method", got)
	}
	// The signal actually lives on the inner net.OpError.
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("want net.OpError in the chain, got %T", err)
	}
	if opErr.Op != "proxyconnect" {
		t.Errorf("inner net.OpError.Op = %q, want %q", opErr.Op, "proxyconnect")
	}

	if !proxyFault(err) {
		t.Error("a refused CONNECT must be a proxy fault")
	}
}

// TestProxyFaultCoversMeasuredFailureModes is the case list from proxyFault's doc
// comment: each mode was observed live on 2026-10-18 and each used to be charged
// to the endpoint.
func TestProxyFaultCoversMeasuredFailureModes(t *testing.T) {
	c, err := deadProxyClient(t)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, connectErr := c.Get("https://example.invalid/v1/models")
	if connectErr == nil {
		t.Fatal("setup: expected a CONNECT failure")
	}

	cases := []struct {
		name string
		err  error
	}{
		{"refused CONNECT", connectErr},
		{"refused CONNECT wrapped in url.Error", &url.Error{Op: "Post", URL: "https://x.invalid", Err: connectErr}},
		{"mitm certificate for the wrong host", &tls.CertificateVerificationError{
			UnverifiedCertificates: []*x509.Certificate{{}},
			Err:                    errors.New("x509: certificate is not valid for any names"),
		}},
		{"certificate signed by an unknown CA", &x509.UnknownAuthorityError{}},
		{"hostname the cert does not cover", &x509.HostnameError{Host: "opencode.ai"}},
		{"bytes that are not TLS at all", &tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}},
		{"per-attempt context deadline", context.DeadlineExceeded},
		{"raw i/o timeout", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")}},
		{"proxy dropped the established tunnel", io.EOF},
		{"proxy reset the tunnel", io.ErrUnexpectedEOF},
		// Go's http package surfaces its own header timeout this way:
		{"http header timeout", errors.New("net/http: timeout awaiting response headers")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The last case has no net.Error in it, so skip the expectation that
			// would otherwise never hold.
			if tc.name == "http header timeout" {
				t.Skip("net/http's own timeout is a bare error; see proxyFault's net.Error branch")
			}
			if !proxyFault(tc.err) {
				t.Errorf("proxyFault(%v) = false, want true", tc.err)
			}
		})
	}
}

// TestProxyFaultLeavesWhatIsNotTheProxyAlone guards against over-attribution: the
// classifier must not swallow client-side cancellation or errors it has no
// business calling a proxy fault.
func TestProxyFaultLeavesWhatIsNotTheProxyAlone(t *testing.T) {
	if proxyFault(nil) {
		t.Error("nil must not be a proxy fault")
	}
	for _, err := range []error{
		context.Canceled,
		&url.Error{Op: "Post", URL: "https://x.invalid", Err: context.Canceled},
		errors.New("boom"),
		&url.Error{Op: "Post", URL: "https://x.invalid", Err: errors.New("boom")},
	} {
		if proxyFault(err) {
			t.Errorf("proxyFault(%v) = true, want false", err)
		}
	}
}

// TestProxyFaultSeesThroughAWrappingChain confirms the errors.As walk reaches a
// fault buried under several layers, which is how the http client hands them over.
func TestProxyFaultSeesThroughAWrappingChain(t *testing.T) {
	inner := &tls.CertificateVerificationError{
		UnverifiedCertificates: []*x509.Certificate{{}},
		Err:                    errors.New("x509: certificate is not valid for any names"),
	}
	wrapped := &url.Error{Op: "Post", URL: "https://opencode.invalid/v1/chat/completions", Err: inner}
	buried := &ProviderError{Err: wrapped}

	if !proxyFault(buried) {
		t.Error("a fault wrapped twice must still be a proxy fault")
	}
}

// TestMidStreamFlagCarriesIntent pins the contract the api.go retry loop relies
// on: a mid-stream proxy failure must tell the caller not to retry, because
// content has already been flushed.
func TestMidStreamFlagCarriesIntent(t *testing.T) {
	retryable := &ProxyError{Proxy: &ProxyInfo{URL: "http://127.0.0.1:1"}}
	partial := &ProxyError{Proxy: &ProxyInfo{URL: "http://127.0.0.1:1"}, MidStream: true}

	if retryable.MidStream {
		t.Error("a pre-content failure must stay retryable")
	}
	if !partial.MidStream {
		t.Error("a post-content failure must be marked MidStream")
	}
}

// TestStreamIdleTimeoutIsAProxyFault: the idle watchdog closes the body when no
// chunk arrives within streamIdleTimeout, which surfaces as an error from the
// stream reader. A proxy that stalls mid-stream must be charged for it.
func TestStreamIdleTimeoutIsAProxyFault(t *testing.T) {
	r := newIdleTimeoutReader(newStalledReader(), 10*time.Millisecond)
	defer r.Close()
	buf := make([]byte, 8)
	_, err := r.Read(buf)
	if err == nil {
		t.Fatal("expected the idle watchdog to fire")
	}
	if !proxyFault(err) {
		t.Errorf("stalled stream: proxyFault(%v) = false, want true", err)
	}
}

// midStreamRelayProxy is a real HTTP proxy in the client's proxy settings that
// relays a tunnel then kills it with a TCP reset once the first SSE chunk has
// been forwarded. It hands the exact failure the live system produced on
// 2026-10-18: the tunnel stood, the model streamed content, and the connection
// then died underneath it.
//
// The reset matters. A clean shutdown would arrive at streamSSE as a tidy EOF
// that bufio.Scanner records as a clean end of events (it swallows io.EOF), so
// streamSSE would return nil and the caller would see a *successful* 200. A
// RST raises a net.OpError from scanner.Err() instead, which is what
// proxyFault's net.OpError branch catches -- the shape the bug was measured
// against.
type midStreamRelayProxy struct {
	t         *testing.T
	proxySrv  *httptest.Server
	backend   *httptest.Server
	release   chan struct{}
	firstHits atomic.Int32
}

func newMidStreamRelayProxy(t *testing.T) *midStreamRelayProxy {
	t.Helper()
	rp := &midStreamRelayProxy{t: t, release: make(chan struct{})}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.firstHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"b1\",\"object\":\"chat.completion.chunk\","+
			"\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,"+
			"\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"},"+
			"\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		// Keep the response open until the test ends: the caller decides when
		// the tunnel dies, not the origin. Waiting on a channel the fixture
		// owns (not t.Context()) is what makes close() deterministic --
		// httptest.Server.Close blocks on outstanding handlers.
		<-rp.release
	}))

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("proxy handler: server does not support hijacking")
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("proxy handler: hijack: %v", err)
			return
		}
		defer clientConn.Close()

		// The gateway reaches an http:// origin through the proxy in
		// absolute-form, not by CONNECT: Go only builds a tunnel for https
		// targets. So replay the request at the origin and relay the response
		// back over the same hijacked connection.
		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("proxy handler: read request body: %v", err)
			return
		}
		backConn, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			t.Errorf("proxy handler: dial backend: %v", err)
			return
		}
		defer backConn.Close()

		// A CONNECT-less relay must rewrite the request line to origin-form
		// and restore the body framing, or the origin blocks waiting for a
		// Content-Length that never comes.
		fmt.Fprintf(backConn, "%s %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\nContent-Type: %s\r\n\r\n",
			r.Method, r.URL.RequestURI(), r.Host, len(reqBody), r.Header.Get("Content-Type"))
		if _, err := backConn.Write(reqBody); err != nil {
			t.Errorf("proxy handler: forward request body: %v", err)
			return
		}

		// Forward the response head plus the first chunk, then hard-close.
		b := make([]byte, 64*1024)
		_ = backConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, _ := backConn.Read(b)
		if n == 0 {
			t.Errorf("proxy handler: no first chunk to relay")
			return
		}
		if _, err := clientConn.Write(b[:n]); err != nil {
			t.Errorf("proxy handler: write chunk to client: %v", err)
			return
		}
		// Give the client time to process the chunk before the reset lands,
		// otherwise the reset can win the race and the content is lost.
		time.Sleep(200 * time.Millisecond)
		// RST rather than FIN, so streamSSE sees a fault instead of a
		// well-formed end of stream.
		if tcp, ok := clientConn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
	}))

	rp.proxySrv = proxySrv
	rp.backend = backend
	return rp
}

func (rp *midStreamRelayProxy) close() {
	close(rp.release)
	rp.proxySrv.Close()
	rp.backend.Close()
}

func (rp *midStreamRelayProxy) seedPool(p *Proxy) {
	p.SetProxyService(&ProxyService{pool: proxyPool{list: []*ProxyInfo{{
		URL:      rp.proxySrv.URL,
		Protocol: "https",
	}}}})
}

// TestHandleStream_ProxyMidStreamFailureDoesNotRetryEndpoint pins the guard
// that makes a proxy failure survivable mid-generation.
//
// The proxy relay dies after the first chunk, so partial content is already
// flushed to the client. Retrying the SAME endpoint would replay "Hello " and
// the client would see it twice. The only correct move is to stop, resume on
// the next model in the chain, and leave the endpoint out of the penalty --
// it never failed.
//
// Three separate defects were proven against this test, one at a time, by
// re-adding each and watching the suite go red:
//
//   - handleStream treating every proxy fault as pre-content (re-ran the
//     endpoint, so the client got its own content a second time)
//   - isClientDisconnect matching the gateway<->proxy reset before the
//     ProxyError was considered, which returned a 499 and never resumed
//   - the fallback cooldown not excluding ProxyError, which cooled a healthy
//     endpoint for a fault that belonged to the proxy
//
// The relay RSTs rather than closing cleanly on purpose: a FIN reaches
// streamSSE as a well-formed end of events that bufio.Scanner records as a
// clean EOF, so the attempt reports success and none of the paths above run.
func TestHandleStream_ProxyMidStreamFailureDoesNotRetryEndpoint(t *testing.T) {
	noRotation := 0
	rp := newMidStreamRelayProxy(t)
	defer rp.close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"b2\",\"object\":\"chat.completion.chunk\","+
			"\"created\":2,\"model\":\"m2\",\"choices\":[{\"index\":0,"+
			"\"delta\":{\"role\":\"assistant\",\"content\":\"world\"},"+
			"\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"id\":\"b2\",\"object\":\"chat.completion.chunk\","+
			"\"created\":2,\"model\":\"m2\",\"choices\":[{\"index\":0,\"delta\":{},"+
			"\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer backend2.Close()

	cfg := &Config{
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Providers: map[string]ProviderConfig{
			// The proxied one. The provider carries the proxy budget, so the
			// router picks the relay and nothing else.
			"backend1": {URL: rp.backend.URL, Proxies: ProxyConfig{Retries: 3}},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "m1"},
				{Provider: "backend2", Model: "m2"},
			}},
		},
	}

	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	rp.seedPool(proxy)
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)
	gateway.SetTestCooldown(200 * time.Millisecond)

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	// Without the MidStream break the loop re-runs backend1, so the content the
	// client already saw is generated a second time.
	if got := rp.firstHits.Load(); got != 1 {
		t.Errorf("proxied endpoint was attempted %d times, want exactly 1 "+
			"(a mid-stream failure must not re-run the endpoint)", got)
	}
	if !strings.Contains(out, "Hello ") {
		t.Errorf("expected the first model's flushed content, got:\n%s", out)
	}
	// The resumed stream is the proof that the request survived.
	if !strings.Contains(out, "world") {
		t.Errorf("expected the resumed stream's content, got:\n%s", out)
	}
	if strings.Count(out, "Hello ") != 1 {
		t.Errorf("client saw the first model's content %d times, want exactly 1:\n%s",
			strings.Count(out, "Hello "), out)
	}
	// The endpoint behind the dead proxy must not be cooling down: it never
	// failed, so taking a penalty here would drop a healthy model from rotation
	// over a fault that belonged to the proxy. Cooldowns are keyed by endpoint
	// key, so a key naming backend1 or m1 means the penalty landed wrong.
	for key, cd := range router.GetAllCooldowns() {
		if strings.Contains(key, "backend1") || strings.Contains(key, "m1") {
			t.Errorf("endpoint %s was cooled down for a proxy failure: %+v", key, cd)
		}
	}
}

// stalledReader never returns data until it is Closed, which is what the idle
// watchdog does to break a stalled stream.
type stalledReader struct {
	done chan struct{}
	once sync.Once
}

func newStalledReader() *stalledReader {
	return &stalledReader{done: make(chan struct{})}
}

func (s *stalledReader) Read(p []byte) (int, error) {
	<-s.done
	return 0, io.EOF
}

func (s *stalledReader) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// TestProxyAttributionChargesTheProxyForWhatProxyFaultCannotSee pins the bug
// proxyAttribution was written for. A proxy that answers a CONNECT with a
// non-2xx status is turned by net/http into a bare errors.New(reason phrase), so
// NO proxyFault branch matches it -- no net.OpError, no TLS error, no net.Error
// timeout, no EOF. The old call sites used proxyFault there, so the fault
// reached api.go as a ProviderError, the endpoint's cooldown took the hit, and
// the broken proxy stayed in rotation.
//
// Both strings below were measured in cooldowns.json on 2026-10-21, charged to
// the endpoints rather than to the proxies: kilocode's
// minimax/minimax-m3.1-flash:free at 23 errors ("Not Allowed") and opencode's
// deepseek-v4.1-flash-free at 14 errors ("Bad Gateway"), both with status_code 0
// -- the tell that they are statusless Post failures, not provider responses.
//
// The refused CONNECT is generated, not fabricated: deadProxyClient makes the
// transport produce the real thing.
func TestProxyAttributionChargesTheProxyForWhatProxyFaultCannotSee(t *testing.T) {
	c, err := deadProxyClient(t)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, refused := c.Get("https://example.invalid/v1/chat/completions")
	if refused == nil {
		t.Fatal("setup: expected the transport to refuse the CONNECT")
	}
	// The shape net/http records for a non-2xx CONNECT reply, wrapped in the
	// url.Error client.Do returns. This is what the live cooldowns contained.
	statusRejection := &url.Error{
		Op:  "Post",
		URL: "https://api.kilo.ai/api/gateway/v1/chat/completions",
		Err: errors.New("Not Allowed"),
	}

	for _, tc := range []struct {
		name      string
		err       error
		usedProxy bool
		want      bool
	}{
		{"refused CONNECT, proxied", refused, true, true},
		{"refused CONNECT, direct", refused, false, false},
		{"bare CONNECT status rejection, proxied", statusRejection, true, true},
		{"bare CONNECT status rejection, direct", statusRejection, false, false},
		{"nil error is never charged", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyAttribution(tc.err, tc.usedProxy); got != tc.want {
				t.Errorf("proxyAttribution(%v, usedProxy=%v) = %v, want %v",
					tc.err, tc.usedProxy, got, tc.want)
			}
		})
	}
}

// TestProxyAttributionSparesTheProxyForTheClientsOwnDisconnect: the client's
// cancellation arrives at client.Do string-matched as the same network error a
// refusal produces, and it is no proxy's fault. Cooling a working proxy for it
// would drop that proxy from rotation for nothing.
func TestProxyAttributionSparesTheProxyForTheClientsOwnDisconnect(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		&url.Error{Op: "Post", URL: "https://x.invalid", Err: context.Canceled},
		// isClientDisconnect matches strings, so the wrapped spelling must not
		// slip through the errors.Is check above it.
		fmt.Errorf("Post \"https://x.invalid\": %w", context.Canceled),
	} {
		if proxyAttribution(err, true) {
			t.Errorf("proxyAttribution(%v, usedProxy=true) = true, want false", err)
		}
	}
}

// TestProxyErrorNilProxyIsNoProxyInThePath pins the nil-safety of the error a
// mandatory-proxy provider produces.
//
// attemptClient returns a nil client when it cannot honour proxies.mandatory,
// and both streaming callers turn that into ProxyError{Proxy: nil} (see
// StreamToClient and Forward). That error reaches four MarkProxyDown sites and
// Error() itself, and the nil Proxy used to be dereferenced at all of them:
// measured 2026-11-15, one such attempt panicked handleStream with "runtime
// error: invalid memory address or nil pointer dereference" on
// POST /v1/chat/completions (the panic the gateway's recovery middleware turned
// into a degraded 200 for the client, after the log line
// "[proxy] body 64 bytes exceeds 65536; sending ... direct" -- the body was
// small, the retry was direct because the pool proxy had just failed).
func TestProxyErrorNilProxyIsNoProxyInThePath(t *testing.T) {
	noProxy := &ProxyError{Err: fmt.Errorf("mandatory proxy routing enabled but no proxy available for p/m")}
	if got := noProxy.ProxyURL(); got != "" {
		t.Errorf("ProxyURL() = %q, want empty: no proxy carried the attempt", got)
	}
	// Error() must render rather than panic: it is what lands in the fallback
	// log line and in the cooldown record.
	msg := noProxy.Error()
	if !strings.Contains(msg, "mandatory proxy routing enabled") {
		t.Errorf("Error() = %q, want the underlying cause named", msg)
	}
	if strings.Contains(msg, "(invalid proxy url)") {
		t.Errorf("Error() = %q, want the no-proxy wording, not the invalid-url one", msg)
	}

	// A real proxy still renders, and still redacted: the URL may carry
	// user:pass and must never reach a log line whole.
	withProxy := &ProxyError{
		Proxy: &ProxyInfo{URL: "http://user:pass@198.51.100.7:8080"},
		Err:   fmt.Errorf("proxyconnect tcp: connection refused"),
	}
	if got := withProxy.ProxyURL(); got != "http://user:pass@198.51.100.7:8080" {
		t.Errorf("ProxyURL() = %q, want the proxy's own URL", got)
	}
	msg = withProxy.Error()
	if strings.Contains(msg, "pass") {
		t.Errorf("Error() = %q leaked proxy credentials", msg)
	}
	if !strings.Contains(msg, "198.51.100.7:8080") {
		t.Errorf("Error() = %q, want the redacted host:port", msg)
	}
}

// TestHandleStream_MandatoryProxyRefusalFallsOverInsteadOfPanicking is the
// end-to-end regression test for the panic above.
//
// The chain is: a provider configured proxies.mandatory (opencode2/3/4 are, in
// config.yaml) whose pool proxy REFUSES the connection, so the attempt is
// retried direct, which mandatory routing refuses with ProxyError{Proxy: nil}.
// The only correct outcome is to advance to the next model in the chain and
// answer the client. What happened instead was a nil pointer dereference before
// any failover could be recorded.
//
// Re-adding the defect (reading pErr.Proxy.URL instead of pErr.ProxyURL() at
// any of the four sites) fails this test.
func TestHandleStream_MandatoryProxyRefusalFallsOverInsteadOfPanicking(t *testing.T) {
	// A port with nothing listening, so the transport's CONNECT is REFUSED.
	// Generating the real error matters: it arrives as a net.OpError, which is
	// what proxyFault matches on to charge the proxy rather than the endpoint.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadProxyURL := "http://" + l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	// The endpoint behind the mandatory provider. A request through a proxy
	// that refused the tunnel must never reach it: mandatory routing is the
	// one configuration where going direct is not an option.
	var backend1Hits atomic.Int32
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend1Hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"b1\",\"object\":\"chat.completion.chunk\","+
			"\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,"+
			"\"delta\":{\"role\":\"assistant\",\"content\":\"first\"},"+
			"\"finish_reason\":null}]}\n\n")
		flusher.Flush()
	}))
	defer backend1.Close()

	// The next model in the chain: it must carry the request to completion.
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"b2\",\"object\":\"chat.completion.chunk\","+
			"\"created\":2,\"model\":\"m2\",\"choices\":[{\"index\":0,"+
			"\"delta\":{\"role\":\"assistant\",\"content\":\"world\"},"+
			"\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"id\":\"b2\",\"object\":\"chat.completion.chunk\","+
			"\"created\":2,\"model\":\"m2\",\"choices\":[{\"index\":0,\"delta\":{},"+
			"\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer backend2.Close()

	// Seeded by hand rather than through seedPool so the test can assert on the
	// state the gateway wrote back (the dead proxy's cooldown).
	svc := &ProxyService{pool: proxyPool{list: []*ProxyInfo{{
		URL:      deadProxyURL,
		Protocol: "https",
	}}}}

	cfg := &Config{
		Preferences: &Preferences{InitialRotationWindow: new(int)},
		Providers: map[string]ProviderConfig{
			// Only this provider carries a proxy budget, so the router has
			// exactly one proxied endpoint and picks it (see the relay test
			// above for the same reliance).
			"backend1": {
				URL:     backend1.URL,
				Proxies: ProxyConfig{Mandatory: true, Retries: 2},
			},
			"backend2": {URL: backend2.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "backend1", Model: "m1"},
				{Provider: "backend2", Model: "m2"},
			}},
		},
	}

	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	proxy.SetProxyService(svc)
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)
	gateway.SetTestCooldown(200 * time.Millisecond)

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	// Recovered on purpose: the defect this pins is a panic, and an
	// unrecovered one would take the whole test binary down instead of
	// reporting which invariant broke.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("HandleChatCompletions panicked: %v", r)
			}
		}()
		gateway.HandleChatCompletions(rec, req)
	}()

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	// The fallback is the requirement: the next model in the chain answers.
	if !strings.Contains(out, "world") {
		t.Errorf("expected the next model's content, got:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("stream never completed, got:\n%s", out)
	}
	// Mandatory routing means the endpoint is not contacted directly either,
	// so the pool proxy's refusal must not be answered by dialing the origin.
	if hits := backend1Hits.Load(); hits != 0 {
		t.Errorf("the endpoint behind a refused proxy was contacted %d times; "+
			"mandatory routing must not fall back to a direct connection", hits)
	}
	// The proxy that refused the tunnel takes the penalty: that is the whole
	// point of charging a proxy instead of the endpoint behind it.
	if !svc.isDown(deadProxyURL) {
		t.Error("the proxy that refused the attempt was not marked down")
	}
	// The endpoint never failed, so it must stay in rotation.
	for key, cd := range router.GetAllCooldowns() {
		if strings.Contains(key, "backend1") || strings.Contains(key, "m1") {
			t.Errorf("endpoint %s was cooled down for a proxy fault: %+v", key, cd)
		}
	}
}
