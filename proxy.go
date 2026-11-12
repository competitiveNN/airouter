package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StreamAttemptOptions carries the per-request interruption controls for one
// streaming attempt. The zero value preserves the proxy's own defaults, so
// existing callers and tests do not need to supply anything.
type StreamAttemptOptions struct {
	// MaxIdleTimeout is the longest the client may see no output while this
	// attempt is active. It is clamped by the caller to the interruption
	// budget that is left.
	MaxIdleTimeout time.Duration
	// OnProgress is called whenever bytes reach the client. The gateway uses
	// it to start a fresh interruption window and to release the attempt's
	// connection/first-output deadline once output has started.
	OnProgress func()
	// Direct forces this attempt to skip the proxy pool even when the provider
	// is configured to use it. The fallback loop sets it to retry the same
	// endpoint without the middleman after a proxy failure.
	Direct bool
	// Reasoning, when non-nil, receives the reasoning content of this attempt.
	// The chat fallback replays it as `reasoning_content` on the assistant
	// message it appends: Command Code's thinking models reject a replayed
	// assistant message without it ("Missing required parameter:
	// 'messages[N].reasoning_content'"), so a mid-stream failover died on
	// arrival at the one provider that could have continued the answer.
	Reasoning *strings.Builder

	// AllowLongPrefill lets the attempt exceed the normal 30s interruption
	// ceiling. It exists for the Command Code shim: a very large prompt spends
	// more than 30s in prefill before the first event, and clamping it produced
	// a guaranteed 502. The caller must also pass a size-derived timeout; this
	// flag only removes the ceiling, it does not invent a budget.
	AllowLongPrefill bool

	// KeepaliveInterval, when > 0, writes an SSE comment to the client on this
	// cadence while the attempt is waiting for its first client-visible byte.
	// A long prefill is otherwise indistinguishable from a dead connection to
	// the client's low-speed timeout. Keepalives stop at the first real write.
	KeepaliveInterval time.Duration
}

// progressWriter observes successful writes to the client. It is deliberately
// a writer wrapper rather than a callback threaded through every SSE write
// site: the chat stream has several distinct write paths (buffered release,
// direct deltas, synthesized tool calls, and the final sentinel), and missing
// one would leave the client-visible watchdog running while output was in
// fact flowing.
type progressWriter struct {
	w        io.Writer
	progress func()
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 && w.progress != nil {
		w.progress()
	}
	return n, err
}

// keepaliveWriter writes SSE comments until the first real byte is written.
// The stop function is idempotent and blocks until the ticker goroutine has
// exited, so no keepalive can interleave with the first event or with an error
// body written after the attempt returns.
type keepaliveWriter struct {
	w    io.Writer
	stop func()
	once sync.Once
}

func (k *keepaliveWriter) Write(p []byte) (int, error) {
	k.once.Do(k.stop)
	return k.w.Write(p)
}

// startSSEKeepalive returns a writer that emits ": keepalive\n\n" every
// interval until its first Write, plus the stop function (safe to call more
// than once). A nil flusher or a non-positive interval disables it.
func startSSEKeepalive(w io.Writer, flusher http.Flusher, interval time.Duration) (*keepaliveWriter, func()) {
	if interval <= 0 || flusher == nil {
		return &keepaliveWriter{w: w, stop: func() {}}, func() {}
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(stopCh)
			<-done
		})
	}
	return &keepaliveWriter{w: w, stop: stop}, stop
}

// keepaliveResponseWriter is an http.ResponseWriter whose body writes go
// through a keepaliveWriter, so the pre-output keepalive stops on the first
// real byte (content, an SSE error, or a "reconnecting" comment) and cannot
// interleave with it.
type keepaliveResponseWriter struct {
	http.ResponseWriter
	keepalive *keepaliveWriter
}

func (k keepaliveResponseWriter) Write(p []byte) (int, error) { return k.keepalive.Write(p) }

// idleKeepaliveWriter serializes every client write and emits an SSE comment
// whenever the stream has been silent for a full interval. Unlike the one-shot
// pre-output keepalive, it keeps covering the stream for its whole lifetime,
// which also covers a long prefill, a cooldown wait, and a mid-stream stall.
//
// The interval must stay well below the client's per-read timeout (maki:
// 30s). Without this, silence is indistinguishable from a dead connection and
// surfaces as "connection error" even though the gateway is still working.
type idleKeepaliveWriter struct {
	w        io.Writer
	flusher  http.Flusher
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

// startSSEIdleKeepalive wraps w. stop is idempotent and waits for the ticker
// goroutine to exit, so no comment can be written after the handler returns.
func startSSEIdleKeepalive(w io.Writer, flusher http.Flusher, interval time.Duration) (*idleKeepaliveWriter, func()) {
	k := &idleKeepaliveWriter{w: w, flusher: flusher, interval: interval, last: time.Now()}
	if interval <= 0 || flusher == nil {
		return k, func() {}
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case now := <-t.C:
				k.mu.Lock()
				if now.Sub(k.last) >= interval {
					if _, err := io.WriteString(k.w, ": keepalive\n\n"); err != nil {
						k.mu.Unlock()
						return
					}
					k.flusher.Flush()
					k.last = now
				}
				k.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(stopCh)
			<-done
		})
	}
	return k, stop
}

// Write records the write before returning, so the ticker measures silence
// from the last real byte (content, an SSE error, or a reconnecting comment).
func (k *idleKeepaliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n, err := k.w.Write(p)
	if n > 0 {
		k.last = time.Now()
	}
	return n, err
}

type idleKeepaliveResponseWriter struct {
	http.ResponseWriter
	keepalive *idleKeepaliveWriter
}

func (k idleKeepaliveResponseWriter) Write(p []byte) (int, error) { return k.keepalive.Write(p) }

type Proxy struct {
	client *http.Client
	// baseTransport is the shared transport. An attempt that needs its own
	// header-timeout budget clones it. NOTE that http.Transport.Clone does
	// NOT share the connection pool with the original: the clone gets a fresh
	// pool and re-establishes its connections, so a clone costs pooling. That
	// is unavoidable here because ResponseHeaderTimeout has no per-request
	// equivalent -- bounding a request context would also bound the body,
	// which a long generation must not be.
	baseTransport *http.Transport

	// ccTransport/ccClient route Command Code upstream traffic through
	// the HTTP CONNECT proxy named by CC_UPSTREAM_PROXY
	// (commandcode-proxy issue #18). Both are nil -- and every accessor
	// below falls back to the direct client/transport -- when the env
	// var is unset or unparsable, so the default path is byte-for-byte
	// the direct one.
	ccTransport *http.Transport
	ccClient    *http.Client

	// longClient/longTransport are the direct path for a large-context attempt,
	// with a response-header budget large enough to cover prefill. The
	// per-request context/attempt timer still bounds every attempt, so a short
	// request cannot wait longer than its own budget merely because this
	// transport's header timeout is larger.
	longClient    *http.Client
	longTransport *http.Transport

	// protocolFor resolves an endpoint's effective protocol, consulting the
	// router's learned map. Injected after construction for the same reason
	// metrics is: the proxy is built before the router exists.
	protocolMu        sync.RWMutex
	protocolFn        func(*ModelEndpoint) protocol
	config            *atomic.Pointer[Config]
	toolCalls         atomic.Bool
	streamIdleTimeout time.Duration
	// metrics is the shared collector, set after construction (the gateway
	// owns the instance, and the proxy is built before the gateway exists).
	// Guarded by metricsMu because SetMetrics runs on the main goroutine
	// while streamSSE reads it from request goroutines.
	metricsMu sync.RWMutex
	metrics   *Metrics

	// upstreamSlots bounds concurrent upstream attempts. nil/disabled when
	// cap is 0 (unlimited). Acquired by acquireUpstream.
	upstreamSlots chan struct{}
	// maxUpstreamConcurrency is the configured cap (0 = unlimited).
	maxUpstreamConcurrency int

	// Proxy pool wiring. Guarded by proxyMu.
	proxyMu  sync.RWMutex
	proxySvc *ProxyService
}

// SetMetrics attaches the shared metrics collector to the proxy so stream-level
// signals (SSE comment routing) are recorded. Nil disables recording, which is
// the case for the many unit tests that build a bare &Proxy{}.
// SetProtocolResolver wires the router's learned-protocol lookup.
// SetProtocolResolver wires the router's learned-protocol lookup.
func (p *Proxy) SetProtocolResolver(fn func(*ModelEndpoint) protocol) {
	p.protocolMu.Lock()
	p.protocolFn = fn
	p.protocolMu.Unlock()
}

// effectiveProtocol is the protocol this attempt should use.
func (p *Proxy) effectiveProtocol(ep *ModelEndpoint) protocol {
	if p == nil {
		return protocolChatCompletions
	}
	p.protocolMu.RLock()
	fn := p.protocolFn
	p.protocolMu.RUnlock()
	if fn == nil {
		return ep.UpstreamProtocol()
	}
	return fn(ep)
}

// proxyTries resolves this endpoint's proxy budget: the maximum number of
// different proxies to try before the endpoint gives up. 0 means a direct
// connection (no proxy).
func (p *Proxy) proxyTries(ep *ModelEndpoint) int {
	// Command Code upstreams never route through the proxy pool: the
	// protocol has its own auth model and the proxy pool is not used.
	if p != nil && p.effectiveProtocol(ep) == protocolCommandCode {
		return 0
	}
	if ep.Proxy > 0 {
		return ep.Proxy
	}
	if p == nil || p.config == nil {
		return 0
	}
	cfg := p.config.Load()
	if cfg == nil {
		return 0
	}
	provider, ok := cfg.Providers[ep.Provider]
	if !ok || provider.Proxies.Retries <= 0 {
		return 0
	}
	return provider.Proxies.Retries
}

// maxProxiedBodyBytes is the largest request body an attempt may send through
// a pool proxy. Rotation exists to change the source IP for rate limits; a
// free proxy that cannot carry a prefill-sized upload costs the whole attempt
// budget and returns nothing. Measured 2026-11-03: a 331k-token `smart`
// request went through 165.154.162.73:8888, timed out after 25s of a 30s
// budget, and the request 503'd. The same body DIRECT to the same kilocode
// model answered in ~1s at 134KB and ~3s at 600KB.
//
// 64KB, not 256KB: the second live failure (14:19, 116k tokens) had a body
// just under the old limit and still lost 12.5s of a 30s budget to
// 165.154.162.73:8888. Token counts and body bytes diverge (dense code/tool
// JSON is ~2 bytes per token), so the cut has to be low enough to catch a
// prefill-sized upload in either unit.
const maxProxiedBodyBytes = 64 << 10

// proxyAllowedForBody reports whether an attempt carrying a body of n bytes
// may use the proxy pool. Large prefill uploads go direct.
func proxyAllowedForBody(n int) bool { return n <= maxProxiedBodyBytes }

// SetProxyService wires the proxy pool into the Proxy. Endpoints whose
// resolved proxy budget is greater than 0 route upstream traffic through a
// proxy from the pool, where that budget comes from the endpoint's own proxy
// value or, by default, its provider's. Nil disables proxying (direct
// connections only).
func (p *Proxy) SetProxyService(svc *ProxyService) {
	p.proxyMu.Lock()
	p.proxySvc = svc
	p.proxyMu.Unlock()
}

// MarkProxyDown puts a proxy into cooldown so it is skipped by pick() until
// the cooldown expires. Used when a proxied request fails at the transport
// layer so the proxy (not the endpoint) takes the penalty.
func (p *Proxy) MarkProxyDown(proxyURL string, d time.Duration) {
	p.proxyMu.RLock()
	svc := p.proxySvc
	p.proxyMu.RUnlock()
	if svc != nil {
		svc.MarkDown(proxyURL, d)
	}
}

// MarkProxyReady records that a proxy was just proven usable, so it counts
// against the ready-pool requirement. Called only on a real success through the
// proxy, which is the strongest evidence available.
func (p *Proxy) MarkProxyReady(proxyURL string) {
	p.proxyMu.RLock()
	svc := p.proxySvc
	p.proxyMu.RUnlock()
	if svc != nil {
		svc.MarkReady(proxyURL)
	}
}

// attemptClient returns the client and the proxy (nil = direct) for one attempt
// to ep. Every proxy surface uses this so the streaming and non-streaming paths
// cannot drift on how, or whether, an attempt is proxied: a nil usedProxy means
// the attempt goes direct and the caller must not attribute faults to a proxy,
// while a non-nil one is what they credit on success and charge on a
// transport-layer failure.
//
// timeout, when non-zero, bounds this attempt's connection phases — dial, TLS
// handshake, response headers — by the request's own size-adjusted budget.
// Without it the shared transport's global 15s/30s values decide, so a provider
// that accepted the connection and then went silent held one attempt for 15s
// even though the request's budget said 5s: exactly the long consecutive
// failure a per-attempt budget exists to prevent. The timeouts are applied to a
// CLONE rather than to the shared transport, because mutating the shared one
// would change every other in-flight request.
func (p *Proxy) attemptClient(ep *ModelEndpoint, timeout time.Duration, allowProxy bool) (*http.Client, *ProxyInfo) {
	client := p.client
	transport := p.baseTransport
	isCommandCode := p.effectiveProtocol(ep) == protocolCommandCode
	if isCommandCode {
		client = p.commandCodeClient()
		transport = p.commandCodeTransport()
	}
	var usedProxy *ProxyInfo
	if allowProxy && p.proxyTries(ep) > 0 {
		p.proxyMu.RLock()
		svc := p.proxySvc
		p.proxyMu.RUnlock()
		if svc != nil {
			if proxy := svc.pick(); proxy != nil {
				if tr := svc.TransportFor(transport, proxy); tr != nil {
					// TransportFor already built this transport for this one
					// proxy, so bounding its timeouts cannot leak into
					// another attempt.
					applyAttemptTimeouts(tr, timeout)
					client = &http.Client{Transport: tr}
					usedProxy = proxy
				} else {
					// No transport could be built for this proxy: the attempt
					// leaves the proxy path and no proxy is credited or blamed.
					log.Printf("[proxy] cannot build a transport for %s/%s proxy %s; trying direct",
						ep.Provider, ep.Model, redactProxyURL(proxy.URL))
				}
			} else {
				// The operator asked for a proxy and none can be used, so the
				// attempt goes direct. Deliberate (a request through a dead pool
				// still beats no request), never silent: upstream traffic
				// arriving from the gateway's own address is otherwise the only
				// symptom.
				log.Printf("[proxy] no usable proxy for %s/%s (pool=%d, ready=%d); trying direct",
					ep.Provider, ep.Model, svc.pool.len(), svc.ReadyCount())
			}
		}
	}

	// Enforce mandatory proxy routing: when the provider requires proxy routing
	// and no proxy was selected, fail the attempt (nil client) so the caller
	// advances the fallback chain instead of silently going direct. This mirrors
	// Forward, which does the same check inline. attemptClient backs the two
	// streaming surfaces (StreamToClient, streamResponsesAttempt), both of which
	// treat a nil client as "mandatory proxy unavailable". The check must run
	// after the selection block: a non-empty pool whose pick() returns nil, or a
	// transport that could not be built, otherwise leaves usedProxy nil and would
	// fall through to a direct connection, defeating the mandatory flag.
	if usedProxy == nil && p.config != nil {
		if cfg := p.config.Load(); cfg != nil {
			if provider := cfg.Providers[ep.Provider]; provider.Proxies.Mandatory {
				return nil, nil
			}
		}
	}

	// The direct path reuses the shared pooled transport. Per-attempt
	// deadlines are applied via the request context, not per-transport
	// timeouts (see handleCompletion / handleStream and StreamToClient), so
	// the shared pool and its idle connections stay intact instead of being
	// re-dialed on every hop and every fallback attempt.
	if usedProxy == nil && transport != nil {
		if timeout > maxRequestAttemptTimeout && p.longClient != nil {
			// The shared transport caps the response-header wait at
			// maxRequestAttemptTimeout. Providers prefill before sending the
			// headers, so that cap would kill a large-context attempt long
			// before its own (larger) budget. The long client's per-attempt
			// context is still bounded by the caller's timeout.
			client = p.longClient
		} else {
			client = &http.Client{Transport: transport}
		}
	}
	return client, usedProxy
}

// requiredProxyTries is how many proxies must be simultaneously ready: the sum
// of every enabled provider's proxy budget, plus any per-endpoint overrides.
//
// The sum, not the maximum, because different requests are routed to different
// providers, and each is allowed to spend its own budget of attempts on
// distinct proxies. With two providers configured `proxy: 3`, up to six
// distinct proxies can be needed at once, so six must be ready for that path to
// actually be usable rather than failing over to a direct connection.
func (p *Proxy) requiredProxyTries() int {
	if p == nil || p.config == nil {
		return 0
	}
	cfg := p.config.Load()
	if cfg == nil {
		return 0
	}
	total := 0
	for _, provider := range cfg.Providers {
		if provider.Proxies.Retries > 0 {
			total += provider.Proxies.Retries
		}
	}
	for _, model := range cfg.Models {
		for _, ep := range model.Chain {
			if ep.Proxy > 0 {
				total += ep.Proxy
			}
		}
	}
	return total
}

// UpdateRequiredReady pushes the current ready-pool requirement into the proxy
// service. Must be called at startup and on every config reload, or the
// requirement keeps the value it had when the process started.
func (p *Proxy) UpdateRequiredReady() {
	if p == nil {
		return
	}
	p.proxyMu.RLock()
	svc := p.proxySvc
	p.proxyMu.RUnlock()
	if svc == nil {
		return
	}
	svc.SetRequiredReady(p.requiredProxyTries())
}

// commandCodeClient returns the HTTP client Command Code upstreams
// are dialed through: the proxy-routed one when CC_UPSTREAM_PROXY is
// configured, the shared client otherwise.
func (p *Proxy) commandCodeClient() *http.Client {
	if p.ccClient != nil {
		return p.ccClient
	}
	return p.client
}

// commandCodeTransport returns the transport Command Code upstreams are
// dialed through, for the per-attempt clone in StreamToClient.
func (p *Proxy) commandCodeTransport() *http.Transport {
	if p.ccTransport != nil {
		return p.ccTransport
	}
	return p.baseTransport
}

// redactProxyURL renders a proxy URL without its credentials: the
// URL may carry user:pass and must never reach a log line whole.
func redactProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid proxy url)"
	}
	return u.Scheme + "://" + u.Host
}

func (p *Proxy) SetMetrics(m *Metrics) {
	p.metricsMu.Lock()
	p.metrics = m
	p.metricsMu.Unlock()
}

// recordSSEComments reports SSE comment lines that were routed out of the event
// path. Nil-safe.
func (p *Proxy) recordSSEComments(n int) {
	if n <= 0 {
		return
	}
	p.metricsMu.RLock()
	m := p.metrics
	p.metricsMu.RUnlock()
	if m != nil {
		m.SSECommentRoutedOut(n)
	}
}

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				MaxConnsPerHost:     100,
				IdleConnTimeout:     90 * time.Second,
				// Bound the connection-setup phases so a fallback request to a
				// flaky provider cannot hang forever inside http.Client.Do.
				// The response body (the actual generation) is guarded
				// separately by firstByteReader / idleTimeoutReader.
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: maxRequestAttemptTimeout,
			},
		},
		streamIdleTimeout: defaultStreamIdleTimeout,
	}
	p.baseTransport = p.client.Transport.(*http.Transport)

	// Large-context attempts prefill before the upstream writes headers, so the
	// shared transport's 25s ResponseHeaderTimeout is too short. Give them a
	// pooled transport with the large-context ceiling; the per-attempt context
	// still enforces the real budget.
	p.longTransport = p.baseTransport.Clone()
	p.longTransport.ResponseHeaderTimeout = maxLargeContextAttemptTimeout
	p.longClient = &http.Client{Transport: p.longTransport}

	// Upstream concurrency cap: bounds how many upstream attempts run at
	// once so a client burst cannot become unbounded dial fan-out. Sorted,
	// operator-tunable via preferences. A zero cap disables the limit.
	maxUp := defaultMaxUpstreamConcurrency
	if cfg != nil && cfg.Preferences != nil {
		maxUp = cfg.Preferences.MaxUpstreamConcurrencyValue()
	}
	p.maxUpstreamConcurrency = maxUp
	if maxUp > 0 {
		p.upstreamSlots = make(chan struct{}, maxUp)
	}

	// Command Code upstreams never use an HTTP CONNECT proxy: they always
	// go direct, ignoring CC_UPSTREAM_PROXY.
	p.config = &atomic.Pointer[Config]{}
	p.config.Store(cfg)
	return p
}

// geminiStripFields lists request fields that Gemini's OpenAI-compatible
// endpoint rejects (it surfaces them as 400 "Unknown name" proto errors).
// The router otherwise forwards the raw client body, so these are removed
// before sending upstream. They are stripped recursively at any nesting
// depth, because clients may place them inside nested objects/arrays. Applies
// to all providers whose name starts with "gemini" (gemini, gemini2, ...).
var geminiStripFields = []string{"thinking", "thinking_budget", "reasoning", "reasoning_effort"}

// sanitizeRequestBody removes provider-specific unsupported fields from the
// request JSON, recursing into nested objects and arrays. For Gemini it also
// guarantees the thought_signature Google requires on replayed tool calls.
// It returns the original body unchanged if nothing applies or on a parse
// failure (fail-open so we never drop a valid request).
func sanitizeRequestBody(body []byte, provider string) []byte {
	if !strings.HasPrefix(provider, "gemini") {
		return body
	}
	fieldSet := make(map[string]bool, len(geminiStripFields))
	for _, f := range geminiStripFields {
		fieldSet[f] = true
	}
	if out, changed := stripJSONFields(body, fieldSet); changed {
		log.Printf("[debug] provider=%s -> stripped unsupported fields %v", provider, geminiStripFields)
		body = out
	}
	return injectGeminiThoughtSignatures(body, provider)
}

// geminiSkipThoughtSignature is Google's documented dummy value for imported
// function-call history. Gemini 3.x requires a thought_signature on every
// replayed functionCall part and answers 400 INVALID_ARGUMENT otherwise:
//
//	Function call is missing a thought_signature in functionCall parts.
//
// A routed conversation can contain tool calls produced by a non-Google model
// (or by a client that does not echo extra_content), so there is no real
// signature to replay. Google documents these sentinels for exactly that case:
// "skip_thought_signature_validator" or "context_engineering_is_the_way_to_go".
const geminiSkipThoughtSignature = "skip_thought_signature_validator"

// injectGeminiThoughtSignatures fills in the sentinel on every assistant tool
// call that lacks one. Real signatures are never overwritten. The body is
// returned byte-for-byte when nothing needed to change.
//
// Measured 2026-11-07: a 300k-token tool-using session failed over to the
// Gemini keys, every one answered 400 for the same missing signature, and the
// exhausted walk surfaced a 503 to the client. The signature cannot be
// reconstructed from a tool call made by another provider, which is what the
// sentinel is for.
func injectGeminiThoughtSignatures(body []byte, provider string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	messages, ok := doc["messages"].([]any)
	if !ok {
		return body
	}
	changed := false
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			if hasGeminiThoughtSignature(call["extra_content"]) {
				continue
			}
			extra, _ := call["extra_content"].(map[string]any)
			if extra == nil {
				extra = map[string]any{}
				call["extra_content"] = extra
			}
			google, _ := extra["google"].(map[string]any)
			if google == nil {
				google = map[string]any{}
				extra["google"] = google
			}
			google["thought_signature"] = geminiSkipThoughtSignature
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	log.Printf("[debug] provider=%s -> injected a dummy thought_signature into replayed tool calls", provider)
	return out
}

// hasGeminiThoughtSignature reports whether an extra_content value already
// carries a non-empty signature under either namespace Google uses
// (generativelanguage: google, Vertex: vertex).
func hasGeminiThoughtSignature(extra any) bool {
	obj, ok := extra.(map[string]any)
	if !ok {
		return false
	}
	for _, ns := range []string{"google", "vertex"} {
		inner, ok := obj[ns].(map[string]any)
		if !ok {
			continue
		}
		if sig, ok := inner["thought_signature"].(string); ok && strings.TrimSpace(sig) != "" {
			return true
		}
	}
	return false
}

// stripJSONFields recursively walks a JSON document and removes any object
// keys present in fields. It returns the (possibly re-encoded) document and
// whether anything was removed.
func stripJSONFields(v json.RawMessage, fields map[string]bool) (json.RawMessage, bool) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return v, false
	}
	switch v[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v, &obj); err != nil {
			return v, false
		}
		changed := false
		for f := range fields {
			if _, ok := obj[f]; ok {
				delete(obj, f)
				changed = true
			}
		}
		for k, val := range obj {
			newVal, c := stripJSONFields(val, fields)
			if c {
				obj[k] = newVal
				changed = true
			}
		}
		if !changed {
			return v, false
		}
		out, err := json.Marshal(obj)
		if err != nil {
			return v, false
		}
		return out, true
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return v, false
		}
		changed := false
		for i, val := range arr {
			newVal, c := stripJSONFields(val, fields)
			if c {
				arr[i] = newVal
				changed = true
			}
		}
		if !changed {
			return v, false
		}
		out, err := json.Marshal(arr)
		if err != nil {
			return v, false
		}
		return out, true
	}
	return v, false
}

func (p *Proxy) SetStreamIdleTimeout(d time.Duration) {
	if d > 0 {
		// The idle window is a client-visible interruption bound, not an
		// operator preference. A larger value would let a stalled provider
		// hold a session silent for longer than the gateway promises.
		if d > maxSessionInterruption {
			d = maxSessionInterruption
		}
		p.streamIdleTimeout = d
	}
}

func (p *Proxy) StreamIdleTimeout() time.Duration {
	return p.streamIdleTimeout
}

func (p *Proxy) SetToolCalls(enabled bool) {
	p.toolCalls.Store(enabled)
}

func (p *Proxy) ToolCallsEnabled() bool {
	return p.toolCalls.Load()
}

func (p *Proxy) buildRequest(ctx context.Context, body []byte, endpoint ModelEndpoint, providerCfg *ProviderConfig, stream bool, sessionID string) (*http.Request, error) {
	backendBody, err := ReplaceModelName(body, endpoint.Model)
	if err != nil {
		return nil, fmt.Errorf("replace model name: %w", err)
	}
	backendBody = sanitizeRequestBody(backendBody, endpoint.Provider)

	// Upstream protocol. `chat` is the historical default and every provider
	// understands it; `responses` addresses a model that speaks the Responses
	// API instead. The translation happens here, at the edge, so the router,
	// the fallback loop and the cooldown classifier keep speaking the one
	// canonical shape regardless of what the provider speaks.
	proto := p.effectiveProtocol(&endpoint)
	path := proto.upstreamPath()
	if proto == protocolResponses {
		translated, terr := chatBodyToResponses(backendBody, endpoint.Model)
		if terr != nil {
			// Fail toward the shape that always works. A translation bug must
			// never be the reason a provider stops serving this model.
			log.Printf("[debug] provider=%s model=%s -> responses translation failed (%v); using chat/completions",
				endpoint.Provider, endpoint.Model, terr)
			path = protocolChatCompletions.upstreamPath()
		} else {
			backendBody = translated
		}
	}

	// Command Code: translate the Chat Completions request into the
	// /alpha/generate envelope and fire the per-key device-fingerprint
	// and lifecycle pre-requests. The envelope is built from the model-
	// substituted body so params.model names the endpoint's model, and
	// the session id is derived from the request content so one logical
	// conversation pins to one upstream thread (prompt-cache affinity).
	var ccSessionID string
	if proto == protocolCommandCode {
		apiKey := providerCfg.APIKey()
		envelope, sid, terr := ccBuildEnvelope(backendBody, endpoint.Model, apiKey, nil)
		if terr != nil {
			return nil, terr
		}
		backendBody = envelope
		ccSessionID = sid
		ccEnsureInitialized(ctx, p.commandCodeClient(), providerCfg.URL, apiKey)
	}

	// OpenCode free-tier gate. The gateway admits a request only when
	// the body carries `stream:true` and a `tools` array holding both
	// `bash` and `read` (measured live 2026-10-11; every other
	// combination answers 403 FreeTierError). The official CLI satisfies
	// this without trying -- it always streams and carries its full
	// coding toolset -- but a bare chat-completion client does not, so
	// the body is rewritten into the gate's shape here, before the
	// protocol translation, on the Chat Completions path only: the
	// Responses translation carries `stream` and `tools` through its own
	// envelope, and the Responses SSE stream it produces is not folded
	// back here.
	if proto == protocolChatCompletions && isOpencodeProvider(endpoint.Provider) {
		backendBody = opencodeGateBody(backendBody)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", providerCfg.URL+path, bytes.NewReader(backendBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if providerCfg.APIKey() != "" {
		req.Header.Set("Authorization", "Bearer "+providerCfg.APIKey())
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	// OpenCode gateways (opencode.ai/zen, /zen/go) require client-attribution
	// headers to grant free-tier access and to pin a session to a single
	// upstream for cache affinity. Without them the gateway treats the request
	// as anonymous and rejects free models with
	// `403 FreeTierError: OpenCode's free tier can only be used from within
	// OpenCode` (oh-my-pi#12306). The session/request IDs are synthesized
	// per request so the upstream cannot fingerprint one client across
	// sessions, matching the official opencode CLI. Explicit provider headers
	// in the config always win (set after the synthesized ones below).
	//
	// The session id must be the canonical `ses_<12hex><14base62>`; the gateway
	// measures a 403 for any other shape. Headers alone are not
	// sufficient, though: the gateway also requires stream=true and a
	// `tools` array holding both `bash` and `read`. The official CLI
	// always sends both, and the body gate above (opencodeGateBody)
	// rewrites a bare chat completion into the same shape, so the
	// contract holds for agent and non-agent clients alike — see
	// scripts/fake-opencode-upstream.py, which enforces the same
	// contract.
	if isOpencodeProvider(endpoint.Provider) {
		for k, v := range opencodeRequestHeaders() {
			req.Header.Set(k, v)
		}
		// The free tier asks for a session id that is stable "for each
		// conversation", so it is anchored on the conversation rather than on
		// the gateway session id, which is a hash of the whole messages array
		// and therefore drifts on every turn. `body` is the client's original
		// body on purpose: the logical model is part of the anchor, and using
		// backendBody would key the anchor on the endpoint's substituted model
		// name and split one conversation across a fallback chain.
		// Both spellings carry the same id. Setting only the canonical
		// x-opencode-session would leave X-Session-ID holding the per-request
		// random id from opencodeRequestHeaders, so the two would disagree and
		// the routing backend would read the unstable one. The session-id and
		// affinity spellings are set from the same value for the same reason.
		setSession := func(id string) {
			ses := opencodeSessionFor(id)
			req.Header.Set("x-opencode-session", ses)
			req.Header.Set("x-opencode-session-id", ses)
			req.Header.Set("x-session-affinity", ses)
			req.Header.Set("X-Session-ID", ses)
		}
		if conv := opencodeConversationID(body); conv != "" {
			setSession(conv)
		} else if sessionID != "" {
			// No anchor available (unparseable body). Fall back to the gateway
			// session id, mapped onto the canonical ses_ form rather than
			// forwarded: "ctx:<hex>" is a shape the gateway rejects with 403.
			setSession(sessionID)
		}
	}
	// Command Code client headers. Applied before the provider
	// overrides below so an explicit header in the airouter
	// config still wins, exactly as for the OpenCode attribution
	// headers above.
	if proto == protocolCommandCode {
		ccSetRequestHeaders(req, providerCfg.APIKey(), ccSessionID)
	}
	// Provider-level header overrides from config take precedence over the
	// synthesized OpenCode attribution headers.
	for k, v := range providerCfg.Headers {
		req.Header.Set(k, v)
	}

	return req, nil
}

// ProxyError is returned when a request routed through a proxy fails at the
// transport/dial layer (the proxy itself is unreachable or misbehaving), as
// distinct from the endpoint returning an HTTP error. The endpoint is not at
// fault, so it must not be cooled down; the proxy is.
type ProxyError struct {
	Proxy *ProxyInfo
	Err   error

	// MidStream marks a failure that happened after content had already been
	// written to the client. The caller must NOT retry the attempt: the client
	// already holds partial output, so re-running the same endpoint would
	// duplicate it. The proxy is still marked down and the request still falls
	// back to the next model in the chain, resuming from the replayed context.
	MidStream bool
}

func (e *ProxyError) Error() string {
	return fmt.Sprintf("proxy %s: %v", redactProxyURL(e.Proxy.URL), e.Err)
}

func (e *ProxyError) Unwrap() error {
	return e.Err
}

// proxyFault reports whether err should be charged to the proxy that carried the
// request rather than to the endpoint behind it. Callers pass it only when the
// attempt actually went through a proxy.
//
// When a proxy is in the path the tunnel, the certificate and the connection all
// belong to the proxy, so a transport-layer failure is the proxy's doing. Measured
// live on 2026-10-18 against public free proxies, these are the failure modes that
// actually occur, and every one of them was previously classified as an ENDPOINT
// fault -- which cooled the endpoint and left the broken proxy in rotation:
//
//   - CONNECT refused: net.OpError{Op:"proxyconnect"} wrapping the dial error.
//     Go puts "proxyconnect" on the INNER net.OpError, not on url.Error (whose Op
//     is the HTTP method, "Post"/"Get"), so matching on url.Error.Op never fires.
//   - A MITM proxy answering inside the tunnel: an x509 hostname or authority
//     failure for the ORIGIN's host, which the origin itself would not produce.
//   - A proxy too slow to finish the tunnel: context deadline / i/o timeout.
//   - A proxy that drops the connection once the tunnel stood: EOF / reset.
//
// An endpoint's HTTP status never reaches here -- client.Do returns those as a
// response, not an error -- so this cannot steal an endpoint's 429 or 5xx. The one
// thing this CAN over-attribute is a genuine origin outage behind a working
// tunnel: it burns a few proxy attempts and marks those proxies down briefly
// before the chain moves on. Retrying a proxy is cheap and a wrong proxy penalty
// lasts 30s, so the bias is deliberately towards blaming the proxy.
func proxyFault(err error) bool {
	if err == nil {
		return false
	}
	// The client hung up. Not the proxy's fault, and not worth a cooldown on
	// anything: the request is already gone.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Any network-layer fault: the tunnel never stood, or stood and died. Covers
	// the proxyconnect shape above plus the SOCKS5 dialer's dial errors, and --
	// because resolution happens at the proxy -- DNS failures too.
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	// The proxy answered where the origin should have.
	if tlsIntercepted(err) {
		return true
	}
	// Timeouts of any flavour: the http client's header timeout, our per-attempt
	// context, or a raw net deadline. context.DeadlineExceeded implements
	// net.Error with Timeout() == true, so one check covers both spellings.
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// proxyAttribution reports whether a transport-layer failure on an attempt that
// ACTUALLY went through a proxy should be charged to that proxy.
//
// proxyFault answers "what KIND of failure is this". This answers "and does this
// attempt belong to the proxy". Both are needed, because the failure that
// matters most here has no writable shape of its own: Go turns a non-200 CONNECT
// response into a bare errors.New(reasonPhrase) (net/http/transport.go:1937),
// so there is no net.OpError, no TLS error, no net.Error timeout and no EOF for
// proxyFault to match on, and the reason phrase is whatever the proxy chose to
// send. Measured live on 2026-10-21, with kilocode and opencode both configured
// proxy:3:
//
//	cooldown kilocode:minimax/minimax-m3.1-flash:free status=0 errors=23
//	  Post "https://api.kilo.ai/api/gateway/v1/chat/completions": Not Allowed
//	cooldown opencode:deepseek-v4.1-flash-free status=0 errors=14
//	  Post "https://opencode.ai/zen/v1/chat/completions": Bad Gateway
//
// Those are free proxies refusing the tunnel, not the providers. status_code 0
// is the tell: a real provider 403/502 arrives as a RESPONSE with a status and
// a body (see the ApplyCooldownForSession path in api.go), so it could never be
// recorded as a statusless Post error string. Because proxyFault read them as
// ordinary endpoint failures, Forward returned a ProviderError, api.go cooled
// down the endpoint, error_count climbed to 13-23 and the circuit breaker
// opened -- while the broken proxy stayed in rotation and the other two tries
// of the proxy:3 budget were never spent.
//
// The bias is deliberate and documented on proxyFault: with a proxy in the path
// the tunnel, the certificate and the connection all belong to it, so a
// transport failure is the proxy's doing unless we know better. The one thing
// this must not claim is the client's own disconnect, which arrives string-
// matched as the same network error but is no proxy's fault.
func proxyAttribution(err error, usedProxy bool) bool {
	if err == nil || !usedProxy {
		return false
	}
	// The client hung up: the request is already gone, and blaming a proxy for
	// it would put a working proxy into cooldown.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// A failure proxyFault already recognises: the tunnel never stood, stood and
	// died, or the proxy answered where the origin should have.
	if proxyFault(err) {
		return true
	}
	// Everything else client.Do can fail with on an attempt the proxy carried --
	// the bare CONNECT rejection above, net/http's own header timeout, an idle
	// stall -- either travelled the proxy's tunnel or never left it.
	return !isClientDisconnect(err)
}

// tlsIntercepted reports whether err means something other than the origin
// finished the TLS handshake: a certificate that does not verify for the host we
// asked for, an unknown authority, or bytes that are not a TLS record at all.
// Through a direct connection these would be endpoint faults; through a proxy
// that built a CONNECT tunnel they are the proxy answering instead.
func tlsIntercepted(err error) bool {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return true
	}
	var hostname *x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var authority *x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return true
	}
	var record *tls.RecordHeaderError
	return errors.As(err, &record)
}

func (p *Proxy) Forward(ctx context.Context, body []byte, endpoint ModelEndpoint, sessionID string) (*http.Response, context.CancelFunc, error) {
	release, err := p.acquireUpstream(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	defer release()
	providerCfg, ok := p.config.Load().Providers[endpoint.Provider]
	if !ok {
		return nil, func() {}, fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}
	cancel := func() {}
	if providerCfg.Timeout > 0 {
		timeoutCtx, c := context.WithTimeout(ctx, providerCfg.Timeout)
		ctx = timeoutCtx
		cancel = c
	}

	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, false, sessionID)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}

	// Select the client: Command Code upstreams use the command-code client,
	// proxied endpoints use a transport routed through the proxy pool, and
	// everything else uses the shared direct client.
	client := p.client
	if p.effectiveProtocol(&endpoint) == protocolCommandCode {
		client = p.commandCodeClient()
	}
	// Large-context attempts prefill before the upstream writes headers, so the
	// shared transport's 25s ResponseHeaderTimeout is too short. If this request
	// has a larger deadline, use the long transport; the deadline still bounds
	// the attempt.
	if deadline, ok := ctx.Deadline(); ok && p.longClient != nil {
		if remaining := time.Until(deadline); remaining > maxRequestAttemptTimeout {
			client = p.longClient
		}
	}

	// The proxy selected for this attempt, if any. Used to wrap dial failures
	// as ProxyError so the router can cool down the proxy instead of the
	// endpoint and retry with a different proxy.
	var usedProxy *ProxyInfo
	allowProxy := proxyAllowedForBody(len(body))
	if !allowProxy && p.proxyTries(&endpoint) > 0 {
		log.Printf("[proxy] body %d bytes exceeds %d; sending %s/%s direct",
			len(body), maxProxiedBodyBytes, endpoint.Provider, endpoint.Model)
	}
	if allowProxy && p.proxyTries(&endpoint) > 0 {
		p.proxyMu.RLock()
		svc := p.proxySvc
		p.proxyMu.RUnlock()
		if svc != nil {
			proxy := svc.pick()
			if proxy != nil {
				base := p.baseTransport
				if p.effectiveProtocol(&endpoint) == protocolCommandCode {
					base = p.commandCodeTransport()
				}
				// A nil transport means no proxy could be built for this one:
				// the attempt then leaves the proxy path, and usedProxy stays
				// nil so the proxy is neither credited with a success nor
				// charged with a failure it never had.
				if tr := svc.TransportFor(base, proxy); tr != nil {
					client = &http.Client{Transport: tr}
					usedProxy = proxy
				} else {
					log.Printf("[proxy] cannot build a transport for %s/%s proxy %s; trying direct",
						endpoint.Provider, endpoint.Model, redactProxyURL(proxy.URL))
				}
			} else {
				// The operator asked for a proxy and none can be used, so this
				// attempt leaves the proxy path entirely. That is deliberate
				// (a request through a dead pool still beats no request), but it
				// must not be silent: without this line the only symptom would be
				// upstream traffic arriving from the gateway's own address.
				log.Printf("[proxy] no usable proxy for %s/%s (pool=%d, ready=%d); trying direct",
					endpoint.Provider, endpoint.Model, svc.pool.len(), svc.ReadyCount())
			}
		}
	}

	// Enforce mandatory proxy routing: if the provider requires proxy
	// routing and no proxy was selected, fail the attempt instead of
	// falling back to a direct connection.
	if usedProxy == nil && providerCfg.Proxies.Mandatory {
		cancel()
		return nil, func() {}, &ProxyError{
			Proxy: nil,
			Err:   fmt.Errorf("mandatory proxy routing enabled but no proxy available for %s/%s", endpoint.Provider, endpoint.Model),
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		// Nothing has reached the client yet, so a proxy fault here is safe to
		// retry on the same endpoint with a different proxy. See proxyAttribution
		// for why every transport-layer failure on a proxied attempt belongs to
		// the proxy -- including the CONNECT status-text rejection proxyFault
		// cannot see (which used to land on the endpoint's cooldown instead).
		if proxyAttribution(err, usedProxy != nil) {
			cancel()
			return nil, func() {}, &ProxyError{Proxy: usedProxy, Err: err}
		}
		cancel()
		return nil, func() {}, &ProviderError{Err: err}
	}
	// The proxy carried this request as far as a response header. That is the
	// strongest evidence of usability we ever get for a proxy, so it renews the
	// readiness mark.
	if usedProxy != nil {
		p.MarkProxyReady(usedProxy.URL)
	}
	return resp, cancel, nil
}

func (p *Proxy) Warmup(ctx context.Context) {
	cfg := p.config.Load()
	const warmupPrompt = `Answer with ` + "`ready.`" + ` and nothing else.`

	reqBody := map[string]interface{}{
		"messages": []map[string]string{
			{"role": "user", "content": warmupPrompt},
		},
		"max_tokens": 10,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		log.Printf("[warmup] failed to marshal request: %v", err)
		return
	}

	for _, modelName := range LogicalModels {
		chain, ok := cfg.GetChain(modelName)
		if !ok || len(chain) == 0 {
			log.Printf("[warmup] no chain for model %s, skipping", modelName)
			continue
		}

		// Pick endpoints at depth 0, 0.33, and 0.66 of the fallback chain.
		// Use math.Round so depth=0.66 on a 3-element chain gives index 2, not 1.
		depths := []float64{0, 0.33, 0.66}
		type slot struct {
			depth float64
			ep    ModelEndpoint
		}
		var slots []slot
		for _, depth := range depths {
			idx := int(math.Round(float64(len(chain)-1) * depth))
			if idx >= len(chain) {
				idx = len(chain) - 1
			}
			slots = append(slots, slot{depth: depth, ep: chain[idx]})
		}

		// Fire 3 rounds of warmups at each of those depths, sequentially per
		// depth but the three depth-slots run in parallel. This stresses every
		// model in the chain enough that a dead/slow provider shows up as a
		// cooldown before the first user request.
		var wg sync.WaitGroup
		for _, s := range slots {
			wg.Add(1)
			go func(s slot) {
				defer wg.Done()
				for round := 1; round <= 3; round++ {
					if ctx.Err() != nil {
						return
					}
					warmupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					resp, fwdCancel, err := p.Forward(warmupCtx, body, s.ep, "")
					if err != nil {
						fwdCancel()
						cancel()
						log.Printf("[warmup] %s/%s depth=%.2f round=%d: %v", s.ep.Provider, s.ep.Model, s.depth, round, err)
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					fwdCancel()
					cancel()
					if resp.StatusCode != 200 {
						log.Printf("[warmup] %s/%s depth=%.2f round=%d: status %d", s.ep.Provider, s.ep.Model, s.depth, round, resp.StatusCode)
						continue
					}
					log.Printf("[warmup] %s/%s depth=%.2f round=%d: ok", s.ep.Provider, s.ep.Model, s.depth, round)
				}
			}(s)
		}
		wg.Wait()
	}
}

// applyAttemptTimeouts bounds every phase of ONE upstream attempt by the
// request's own size-adjusted timeout.
//
// The size-based timeout used to cover only the response-header wait. The
// connection phases before it -- the TCP dial and the TLS handshake -- were
// governed by the transport's global 15s, so a provider that accepted the
// connection and then went silent could hold a single attempt for 15s even
// though the request's budget said 5s. That is the "long consecutive failure"
// this closes: an attempt now costs at most the timeout the request budget
// allows, whatever phase it dies in.
//
// The dialer is wrapped rather than replaced: a SOCKS5 proxy transport carries
// its own DialContext, and overwriting it would silently turn a proxied
// request into a direct one. The wrapper only adds a deadline to the context
// the underlying dialer already receives.
//
// The body phase is deliberately NOT bounded here. A long generation must not
// be cut off; firstByteReader guards time-to-first-token and idleTimeoutReader
// guards a stalled stream.
func applyAttemptTimeouts(tr *http.Transport, timeout time.Duration) {
	if tr == nil || timeout <= 0 {
		return
	}
	tr.ResponseHeaderTimeout = timeout
	tr.TLSHandshakeTimeout = timeout
	base := tr.DialContext
	if base == nil {
		base = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return base(dialCtx, network, addr)
	}
}

func (p *Proxy) StreamToClient(ctx context.Context, w io.Writer, flusher http.Flusher, body []byte, endpoint ModelEndpoint, timeout time.Duration, sessionID string, options ...StreamAttemptOptions) (string, []streamToolCall, int, error) {
	release, err := p.acquireUpstream(ctx)
	if err != nil {
		return "", nil, 0, err
	}
	defer release()
	providerCfg, ok := p.config.Load().Providers[endpoint.Provider]
	if !ok {
		return "", nil, 0, fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	opts := StreamAttemptOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	// The provider's configured timeout is a CEILING on how long one call to it
	// may take, never a floor. Raising the caller's timeout to it let one
	// stalled attempt outlast the request's fallback budget, so the walk gave
	// up with "budget exhausted after 1 attempts" instead of trying the next
	// model. Measured 2026-11-02: a 9.9k-token `work` request had a 6s attempt
	// budget, the nvidia2 provider's configured `timeout: 20s` raised the
	// attempt to 20s, the upstream stayed silent, and the client got a 503 with
	// a healthy model still untried in the chain.
	if providerCfg.Timeout > 0 && (timeout <= 0 || providerCfg.Timeout < timeout) {
		timeout = providerCfg.Timeout
	}
	// Deliberately no `max_interruption` floor here. Callers that need one
	// apply it BEFORE the fallback walk clamps the attempt to the budget that
	// is left (api.go's perAttemptBase, commandcode_api.go's own floor). Doing
	// it again here undid the walk's first-attempt reserve: a Command Code
	// attempt was raised back to 30s while the entire interruption budget was
	// 30s, so it timed out and the client got "budget exhausted after 1
	// attempt" with every other endpoint untried (measured 2026-11-06, every
	// ~2 minutes). Same failure class as the 2026-11-02 provider-timeout floor.
	ceiling := maxSessionInterruption
	if opts.AllowLongPrefill {
		// A very large prompt cannot produce its first event inside the
		// normal interruption window. The caller opts in and supplies a
		// timeout derived from the prompt size.
		ceiling = maxLargeContextAttemptTimeout
	}
	if timeout > ceiling {
		timeout = ceiling
	}
	maxIdle := p.streamIdleTimeout
	if opts.MaxIdleTimeout > 0 {
		maxIdle = minDuration(maxIdle, opts.MaxIdleTimeout)
	}
	// The idle watchdog also starts before the first byte, so it must cover
	// prefill too when the attempt is allowed to outlast the normal ceiling.
	if opts.AllowLongPrefill && timeout > maxIdle {
		maxIdle = timeout
	}
	if maxIdle > ceiling {
		maxIdle = ceiling
	}

	// One deadline covers the WHOLE pre-output attempt: DNS, dial, TLS,
	// response headers, and the wait for the first client-visible event.
	// Per-transport timeouts alone are not enough because they are sequential
	// (a 15s dial plus a 15s TLS handshake plus a 30s header wait can consume
	// 60s), and a request context with a fixed deadline would also kill a
	// legitimate long generation after the first byte.
	//
	// WithCancelCause lets us stop the timer when output actually reaches the
	// client without cancelling the request context. The body then remains
	// governed by the client-visible idle watchdog instead of the initial
	// attempt deadline.
	attemptCtx, attemptCancel := context.WithCancelCause(ctx)
	attemptTimeoutCause := errors.New("upstream attempt timeout")
	var attemptTimer *time.Timer
	var releaseOnce sync.Once
	releaseAttempt := func() {
		releaseOnce.Do(func() {
			if attemptTimer != nil {
				attemptTimer.Stop()
			}
		})
	}
	defer func() {
		releaseAttempt()
		attemptCancel(nil)
	}()
	if timeout > 0 {
		attemptTimer = time.AfterFunc(timeout, func() {
			attemptCancel(attemptTimeoutCause)
		})
	}

	// Use the attempt context for the request so the timer can interrupt the
	// connection/header phase. It is cancelled on timeout, but NOT when the
	// first output arrives: only the timer is stopped then, so a long body
	// phase stays alive.
	req, err := p.buildRequest(attemptCtx, body, endpoint, &providerCfg, true, sessionID)
	if err != nil {
		return "", nil, 0, err
	}

	// A long prefill emits no bytes the client can use. Without a keepalive the
	// client's own low-speed timer kills the connection before the upstream
	// answers, so the attempt gets no chance to use its extended budget.
	//
	// This starts before client.Do because Command Code may do its prefill
	// before sending the response headers. The first comment is delayed by a
	// full interval, so an upstream that refuses immediately (429/401) still
	// returns its real status and the caller can fail over; only a genuinely
	// slow attempt commits the 200 + SSE response.
	if opts.KeepaliveInterval > 0 {
		keepalive, stop := startSSEKeepalive(w, flusher, opts.KeepaliveInterval)
		defer stop()
		w = keepalive
	}

	// The header wait is bounded by the SAME size-based budget as
	// time-to-first-token, not by the transport's global 30s.
	//
	// This was a live failure, not a theoretical one. The streaming path passed
	// the unbounded parent context, so `timeout` only started counting once the
	// headers had already arrived -- and for a large prompt the provider does
	// its prefill BEFORE sending them. A ~44k-token request against NVIDIA
	// therefore sat for the full 30s ResponseHeaderTimeout on every endpoint,
	// and two of those exhausted the 45s fallback budget:
	//
	//	request -> nvidia/z-ai/glm-5.3 (timeout=10s, ~44473 tokens)
	//	... 30s later ...
	//	stream fallback budget exhausted after 3 attempts in 1m0.718s (budget 45s)
	//
	// The client saw "all models unavailable" when the real problem was that
	// each attempt was allowed to run three times longer than its own budget.
	//
	// A cloned transport carries a per-attempt ResponseHeaderTimeout. The
	// clone's connection pool is new, so the attempt re-dials rather than
	// reusing base's idle connections: ResponseHeaderTimeout is a transport
	// setting with no per-request equivalent, and a request context would
	// have to also bound the body, which a long generation must not be. The
	// body phase is deliberately left unbounded: this deadline governs
	// headers only.
	//
	// Select the transport and client for this attempt; see attemptClient.
	// A prefill-sized body goes direct: the pool exists to rotate source IPs
	// for rate limits, not to carry megabytes through a free proxy. A call
	// that asks for Direct skips it too (the proxy-failure retry).
	allowProxy := proxyAllowedForBody(len(body)) && !opts.Direct
	if !allowProxy && p.proxyTries(&endpoint) > 0 {
		log.Printf("[proxy] body %d bytes exceeds %d; sending %s/%s direct",
			len(body), maxProxiedBodyBytes, endpoint.Provider, endpoint.Model)
	}
	client, usedProxy := p.attemptClient(&endpoint, timeout, allowProxy)
	if client == nil {
		// Mandatory proxy routing is enabled but no proxy is available.
		// Fail this attempt so the fallback chain can advance.
		return "", nil, 0, &ProxyError{
			Proxy: nil,
			Err:   fmt.Errorf("mandatory proxy routing enabled but no proxy available for %s/%s", endpoint.Provider, endpoint.Model),
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(context.Cause(attemptCtx), attemptTimeoutCause) {
			err = fmt.Errorf("upstream attempt timed out after %v: %w", timeout, context.DeadlineExceeded)
		}
		// Nothing has reached the client yet, so a proxy fault here is safe to
		// retry on the same endpoint with a different proxy. See proxyAttribution
		// for why every transport-layer failure on a proxied attempt belongs to
		// the proxy -- including the CONNECT status-text rejection proxyFault
		// cannot see (which used to land on the endpoint's cooldown instead).
		if proxyAttribution(err, usedProxy != nil) {
			return "", nil, 0, &ProxyError{Proxy: usedProxy, Err: err}
		}
		return "", nil, 0, &ProviderError{Err: err}
	}
	// The proxy carried this request as far as a response header, so it counts
	// as proven usable and renews its readiness mark.
	if usedProxy != nil {
		p.MarkProxyReady(usedProxy.URL)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes))
		// Keep the upstream's own words in the log. A "reason=unknown" with a
		// body truncated to its first two bytes is what made the Google 400s
		// undiagnosable: the message that names the offending field is in there.
		log.Printf("[debug] upstream error provider=%s model=%s status=%d body=%s",
			endpoint.Provider, endpoint.Model, resp.StatusCode, truncateErr(string(bodyBytes), 600))
		// Command Code error statuses go through the same CC_STATUS_MAP
		// the non-streaming path applies (api.go), so the cooldown
		// classifier sees the OpenAI-equivalent status on streaming
		// attempts too. Without it a raw 402 bypasses the transient 429
		// escalation curve and walks into a soft ban after three
		// consecutive failures.
		if p.effectiveProtocol(&endpoint) == protocolCommandCode {
			if mapped := ccMapError(resp.StatusCode, bodyBytes); mapped != nil {
				if ra := ParseRetryAfter(resp.Header.Get("Retry-After")); ra > mapped.RetryAfter {
					mapped.RetryAfter = ra
				}
				return "", nil, 0, mapped
			}
		}
		return "", nil, 0, &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes, RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	// A 200 without an SSE content type means the provider ignored stream:true
	// and returned a plain JSON body. Forwarding that as SSE would garble the
	// client's stream (abrupt / malformed termination), so treat it as a failure
	// and fall back to the next model in the chain. Command Code is exempt: its
	// /alpha/generate always answers with an NDJSON event stream whose Content-
	// Type is not text/event-stream, and the translator below turns it into SSE.
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") && p.effectiveProtocol(&endpoint) != protocolCommandCode {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes))
		resp.Body.Close()
		return "", nil, 0, &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes, RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	// Guard only the first byte with the size-based timeout. Once the stream
	// starts producing output we stream the rest under the parent context so a
	// long generation is never cut off mid-stream. The idleTimeoutReader on top
	// catches upstream stalls that neither emit an SSE error nor close the
	// connection: if no chunk arrives within the configured stream idle timeout
	// we treat it as a failure and fall back to the next model instead of
	// hanging the client indefinitely.
	firstCtx, firstCancel := context.WithTimeout(attemptCtx, timeout)
	defer firstCancel()

	// When the endpoint was addressed with the Responses API, translate the
	// upstream stream back into Chat Completions SSE before anything else sees
	// it. Everything downstream -- the buffering-until-first-content rule, the
	// tool-call accumulation, the [DONE] framing -- is protocol-agnostic and
	// works unchanged on the translated bytes. Command Code's NDJSON event
	// stream is translated the same way.
	var src io.Reader = resp.Body
	switch p.effectiveProtocol(&endpoint) {
	case protocolResponses:
		src = newResponsesSSEToChatReader(resp.Body)
	case protocolCommandCode:
		src = newCCStreamToChatReader(resp.Body, ccBodyModel(body, endpoint.Model), time.Now().Unix())
	}

	guarded := &firstByteReader{r: src, firstCtx: firstCtx}
	idle := newIdleTimeoutReader(guarded, maxIdle)
	defer idle.Close()

	// Every write that reaches the client is progress: release the initial
	// attempt deadline, reset the client-visible idle watchdog, and tell the
	// gateway to start a fresh interruption window for a possible failover.
	progress := func() {
		idle.MarkProgress()
		releaseAttempt()
		if opts.OnProgress != nil {
			opts.OnProgress()
		}
	}
	streamOut := io.Writer(w)
	streamOut = &progressWriter{w: streamOut, progress: progress}

	partial, toolCalls, completionTokens, sErr := p.streamSSE(streamOut, flusher, idle, endpoint.Model, opts.Reasoning)
	if sErr != nil && errors.Is(context.Cause(attemptCtx), attemptTimeoutCause) {
		sErr = fmt.Errorf("upstream attempt timed out after %v: %w", timeout, context.DeadlineExceeded)
	}
	if sErr != nil && usedProxy != nil && proxyFault(sErr) {
		// The stream died at the transport layer with a proxy in the path, so the
		// proxy takes the penalty. MidStream is set because content may already
		// have been flushed: the caller must move to the NEXT model rather than
		// re-run this endpoint, which would repeat what the client already saw.
		return partial, toolCalls, completionTokens, &ProxyError{Proxy: usedProxy, Err: sErr, MidStream: true}
	}
	return partial, toolCalls, completionTokens, sErr
}

// firstByteReader applies firstCtx (the size-based timeout) to only the first
// Read call. After the first byte arrives, subsequent reads use the underlying
// reader directly so the rest of a long stream is not bounded by the timeout.
type firstByteReader struct {
	r        io.Reader
	firstCtx context.Context
	once     sync.Once
	released bool
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	if f.released {
		return f.r.Read(p)
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := f.r.Read(p)
		ch <- result{n, err}
	}()
	select {
	case <-f.firstCtx.Done():
		// The goroutine spawned above may still be blocked in f.r.Read. Close the
		// underlying reader so that Read returns and the goroutine can exit,
		// avoiding a leak. The deferred idle.Close()/resp.Body.Close() will close
		// it again (idempotent for a net/http body).
		//
		// SAFETY: f.r is always a *http.Response.Body (set in StreamToClient),
		// which implements io.Closer. If a future change wraps f.r in a
		// non-closer reader, this type assertion will fail and the goroutine
		// will leak until the connection is closed by some other means. Always
		// ensure f.r implements io.Closer.
		if c, ok := f.r.(io.Closer); ok {
			c.Close()
		}
		return 0, f.firstCtx.Err()
	case res := <-ch:
		// Only release the first-byte timeout once we actually received data.
		// An io.Reader may return (0, nil) which doesn't count as "first byte".
		if res.n > 0 || res.err != nil {
			f.once.Do(func() { f.released = true })
		}
		return res.n, res.err
	}
}

// Close closes the underlying reader so an idle watchdog (idleTimeoutReader) can
// unblock a Read that is blocked inside the firstByteReader goroutine.
func (f *firstByteReader) Close() error {
	if c, ok := f.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// streamIdleTimeout bounds the time the CLIENT may see no output while an
// upstream stream is active. It is reset only when output actually reaches the
// client, not merely when the upstream sends bytes: role-only preambles,
// keepalives, and tool-call deltas that are still buffered are not progress
// from the client's point of view.
//
// If an upstream stalls (rate limit, dropped connection that never returns EOF,
// etc.) without emitting an SSE error event, we detect it here and fall back to
// the next model instead of hanging the client indefinitely. The value is
// configurable through Proxy.SetStreamIdleTimeout, but is capped at the
// maximum session interruption so no setting can break the 30s contract.
const defaultStreamIdleTimeout = maxSessionInterruption

// idleTimeoutReader wraps an io.Reader and returns an error if no data is read
// for longer than timeout. It is used to detect mid-stream stalls that produce
// neither an SSE error event nor a connection close. On idle expiry it closes
// the underlying reader (when it implements io.Closer) to unblock any in-flight
// Read so the error can propagate and trigger a fallback.
//
// SAFETY: A dedicated watchdog goroutine waits on the timer channel. Only a
// client-visible progress event sends a reset signal; the watchdog drains the
// timer channel and resets it. This avoids the classic timer.Reset-after-fire
// race: with time.AfterFunc there is no channel to drain, so Reset on a fired
// timer is undefined. Using time.NewTimer + an explicit drain in the watchdog
// goroutine makes the reset safe. The goroutine exits when the timer fires
// (timeout) or when Close() is called (stream done).
type idleTimeoutReader struct {
	r       io.Reader
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	closed  bool
	resetCh chan struct{}
	// idle records that the watchdog fired. Without it the Read that follows
	// returns whatever closing the body produced -- an io.EOF, a bare
	// "read on closed response body", or a reset -- and the caller cannot tell a
	// deliberate idle timeout from a stream that simply ended. See Read.
	idle atomic.Bool
}

// errStreamIdle is returned by idleTimeoutReader.Read once the watchdog has
// fired. It is a net.Error timeout rather than a bare error so that proxyFault
// can charge the proxy in the path (and, on a direct connection, so the
// cooldown classifier sees a retryable timeout rather than an EOF that looks
// like a normally ended stream).
type streamIdleError struct{}

func (streamIdleError) Error() string {
	return "stream idle timeout: no data within the configured window"
}
func (streamIdleError) Timeout() bool   { return true }
func (streamIdleError) Temporary() bool { return true }

var errStreamIdle error = streamIdleError{}

func newIdleTimeoutReader(r io.Reader, timeout time.Duration) *idleTimeoutReader {
	if timeout <= 0 {
		timeout = defaultStreamIdleTimeout
	}
	i := &idleTimeoutReader{
		r:       r,
		timeout: timeout,
		resetCh: make(chan struct{}, 1),
	}
	// Start the client-visible watchdog when the attempt starts, not on the
	// first raw upstream read. A provider can send role-only preambles or
	// keepalives for minutes without sending anything the client can use;
	// those bytes must not reset this timer.
	i.timer = time.NewTimer(i.timeout)
	go i.watchdog()
	return i
}

// MarkProgress records that output reached the client and starts a fresh idle
// window. Upstream bytes alone do not call this: only the write path knows
// whether an event was visible to the client or was still being buffered.
func (i *idleTimeoutReader) MarkProgress() {
	i.mu.Lock()
	closed := i.closed
	i.mu.Unlock()
	if closed {
		return
	}
	// Non-blocking send: if a reset is already pending, the watchdog will
	// reset once and the latest progress is covered by that reset.
	select {
	case i.resetCh <- struct{}{}:
	default:
	}
}

func (i *idleTimeoutReader) Read(p []byte) (int, error) {
	i.mu.Lock()
	if i.closed {
		timedOut := i.idle.Load()
		i.mu.Unlock()
		if timedOut {
			return 0, errStreamIdle
		}
		return 0, io.EOF
	}
	i.mu.Unlock()

	n, err := i.r.Read(p)

	i.mu.Lock()
	if err != nil {
		// Read failed: stop the timer so the watchdog can exit promptly.
		if i.timer != nil {
			i.timer.Stop()
		}
	}
	timedOut := i.idle.Load()
	i.mu.Unlock()
	if err != nil && timedOut {
		// The watchdog closed the body to end a stalled stream, and the error we
		// got back is that close, not the stream itself. Report the timeout so the
		// proxy in the path is charged and the chain moves on.
		return 0, errStreamIdle
	}
	return n, err
}

// watchdog blocks on the timer channel. On timeout it closes the reader
// (triggering a fallback). On reset signal it drains the timer channel and
// resets for another cycle. Exits on timeout or when Close() sets i.closed.
func (i *idleTimeoutReader) watchdog() {
	for {
		select {
		case <-i.timer.C:
			i.onIdle()
			return
		case <-i.resetCh:
			i.mu.Lock()
			if i.closed {
				i.mu.Unlock()
				return
			}
			// Drain the timer channel before resetting. If the timer fired
			// between our select cases, the channel has a value we must consume
			// before Reset, otherwise the next cycle could fire immediately.
			select {
			case <-i.timer.C:
			default:
			}
			i.timer.Reset(i.timeout)
			i.mu.Unlock()
		}
	}
}

func (i *idleTimeoutReader) onIdle() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	i.idle.Store(true)
	if c, ok := i.r.(io.Closer); ok {
		c.Close()
	}
}

// Close stops the idle timer and closes the underlying reader. It is called
// when the stream completes normally so the watchdog can't fire afterwards.
// It also signals the watchdog goroutine to exit.
func (i *idleTimeoutReader) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	if i.timer != nil {
		i.timer.Stop()
	}
	i.mu.Unlock()

	// Wake the watchdog so it sees i.closed == true and exits.
	select {
	case i.resetCh <- struct{}{}:
	default:
	}

	if c, ok := i.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (p *Proxy) streamSSE(w io.Writer, flusher http.Flusher, body io.Reader, upstreamModel string, reasoning *strings.Builder) (string, []streamToolCall, int, error) {
	var eventBuf bytes.Buffer
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	// Buffer events until the first real content token, first tool call, or
	// [DONE] arrives. Upstream errors (SSE error events, or non-200 status
	// handled by the caller) almost always occur before any content, so
	// discarding the buffer on error means the client receives nothing and we
	// can fall back to the next model with no duplicated output.
	var buffered bytes.Buffer
	var acc strings.Builder
	tcs := []streamToolCall{}
	released := false
	completionTokens := 0
	// sseComments counts the comment lines dropped by the scan loop below, so
	// the router can expose how much upstream chatter is being filtered out of
	// the event path. Reported to metrics after the loop, on every return path
	// that touches the scanner.
	sseComments := 0
	// Deferred rather than reported at each return: the drop can happen on a
	// clean finish, a mid-stream error that triggers fallback, or a flush
	// failure, and an error path is exactly when the upstream chatter is
	// worth knowing about.
	defer func() { p.recordSSEComments(sseComments) }()

	// When toolCalls is disabled, forward all events verbatim without any
	// coalescing. This preserves compatibility with clients that handle raw
	// tool-call deltas themselves.
	toolCalls := p.toolCalls.Load()

	// A model patch rewrites one upstream model's output (e.g. tool
	// calls a model traps inside its thinking block). It is nil for
	// every model without a registered patch, in which case the
	// stream is untouched.
	patch := modelPatchFor(upstreamModel)
	var patchState PatchStreamState
	if patch != nil {
		patchState = patch.BeginStream()
	}

	// Capture the model name from the stream so synthesized tool call events
	// can mirror it back to the client.
	streamModel := ""

	// Per-index tool call accumulator. Providers stream tool calls as multiple
	// deltas (name, then arguments across chunks). We suppress tool-only delta
	// events from the client and only emit the complete tool call once at
	// [DONE] time. Half-formed or truncated tool calls produce artifacts like
	// `maki_unknown_tool` on the client side; coalescing them atomically
	// avoids that entirely.
	emittedTC := map[int]bool{}
	pendingToolCalls := map[int]*pendingTC{}

	flushBuffered := func() error {
		if buffered.Len() == 0 {
			return nil
		}
		accumulateDelta(&acc, &tcs, buffered.Bytes(), reasoning)
		if _, err := w.Write(buffered.Bytes()); err != nil {
			return err
		}
		buffered.Reset()
		flusher.Flush()
		return nil
	}

	// synthesizeToolCallEvent builds a single SSE event that represents a
	// complete tool call, ready to be flushed to the client. It mirrors the
	// shape of the upstream deltas so client-side parsers see the same
	// schema, just with the full payload already present. The choice carries
	// finish_reason: "tool_calls" so the client parser recognizes this as a
	// terminal tool-call response (without it the client keeps waiting for
	// more deltas and eventually drops the stream).
	synthesizeToolCallEvent := func(index int, tc *pendingTC, model string) []byte {
		delta := map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []map[string]any{{
				"index": index,
				"id":    tc.id,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.name,
					"arguments": tc.args,
				},
			}},
		}
		choice := map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": "tool_calls",
		}
		payload, err := json.Marshal(map[string]any{
			"id":      "airouter-tc-" + strconv.Itoa(index),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{choice},
		})
		if err != nil {
			// Should never happen with a literal map; fall back to the
			// upstream raw bytes the caller already had buffered.
			return nil
		}
		return []byte("data: " + string(payload) + "\n\n")
	}

	// processEvent handles one fully-formed SSE event (raw bytes, ending in a
	// blank line). It extracts errors, decides whether the stream is now
	// "released" (safe to flush), and either flushes or buffers the event.
	processEvent := func(raw []byte) error {
		// A model patch rewrites the event before anything else
		// sees it: tags are stripped from the thinking fields and
		// any tool call the event completes is registered like an
		// upstream tool call, so it is synthesized at [DONE] time
		// (see the post-loop code below).
		if patch != nil {
			payload := sseDataPayload(raw)
			cleaned, extracted := patch.TransformStreamChunk(patchState, payload)
			for _, c := range extracted {
				idx := takePendingToolCallIndex(pendingToolCalls)
				pendingToolCalls[idx] = &pendingTC{
					id:           c.ID,
					name:         c.Name,
					args:         c.Arguments,
					argsReceived: true,
					complete:     true,
				}
				// Also accumulate for the fallback replay: a
				// mid-stream failure must replay an already-
				// extracted call via appendAssistantMessage.
				tcs = append(tcs, streamToolCall{
					Index:     idx,
					ID:        c.ID,
					Type:      "function",
					Name:      c.Name,
					Arguments: c.Arguments,
				})
			}
			if !bytes.Equal(cleaned, payload) {
				raw = rewriteSSEEventData(raw, cleaned)
			}
		}
		if err := ExtractSSEError(raw); err != nil {
			// Early upstream error: discard buffered output so far and signal the
			// caller to fall back.
			return err
		}
		// The [DONE] sentinel is intercepted here and NEVER forwarded. The
		// post-loop code emits the single sentinel that terminates the client
		// stream, so any sentinel that reached the write path would be a
		// duplicate — and a client stops parsing at the FIRST [DONE], so a
		// duplicate (or an early one) truncates the response.
		//
		// Two distinct bugs came from forwarding it, both observed live
		// 2026-09-29 and both fixed here:
		//
		//  1. Early, when the stream had not yet been released. A provider
		//     whose deltas carry nothing the client counts as content
		//     (kilocode/dots-studio: role-only deltas, `content:""`, plus a
		//     `reasoning` field) never satisfies sseEventIsRelease, so [DONE]
		//     was the event that first set released=true. It then got buffered
		//     and the post-loop flush emitted that buffer ahead of its own
		//     [DONE] — a stream STARTING with [DONE], with every real chunk
		//     after it, silently dropped by the client.
		//
		//  2. Duplicated, when the stream was already released. With
		//     toolCalls disabled (the daemon's shipped default) the write
		//     path forwards every event verbatim, so upstream's [DONE] went
		//     out AND the post-loop one followed: two sentinels.
		//
		// Flush the pending buffer first so ordering is preserved, then stop.
		if bytes.Equal(sseDataPayload(raw), []byte("[DONE]")) {
			return flushBuffered()
		}
		// An event that satisfies sseEventIsRelease ends the hold-and-discard
		// window: everything after it is real output. Flush what we held
		// BEFORE forwarding this event.
		//
		// Without this ordering the releasing event itself jumped ahead of the
		// buffered prefix. sseEventIsRelease only becomes true for an event
		// carrying content or a tool call, so the pre-release prefix is exactly
		// the role-only / reasoning-only opener — and the client received its
		// `role: "assistant"` delta *after* the first content token. That
		// inverts SSE delta ordering and breaks clients that treat a late role
		// marker as the start of a new message, so the output looked like two
		// responses.
		if !released && sseEventIsRelease(raw) {
			released = true
			if err := flushBuffered(); err != nil {
				return err
			}
		}
		// Capture completion token usage from SSE usage events emitted before
		// [DONE] so we can report TPS for streaming requests.
		if ct := extractStreamUsage(raw); ct > 0 {
			completionTokens = ct
		}
		// Capture the model name from the first chunk that carries it, so
		// synthesized tool call events can mirror it back to the client.
		if streamModel == "" {
			var mv struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(sseDataPayload(raw), &mv); err == nil && mv.Model != "" {
				streamModel = mv.Model
			}
		}
		if released {
			if toolCalls {
				// Fold any tool call deltas from this event into our per-index
				// accumulators. Tool-only delta events are suppressed from the
				// client; any event that also carries content (or usage) is
				// forwarded verbatim. We emit synthesized complete tool call
				// events only at [DONE] time, when the final accumulated
				// arguments are known. This guarantees the client only ever
				// receives complete, well-formed tool call events (no partial
				// deltas, no duplicates).
				//
				// [DONE] never reaches here: processEvent returns early for it
				// so the post-loop code can emit synthesized tool calls first
				// and then the single terminating sentinel. Clients finalize
				// parsing at [DONE], so a tool call after it would be dropped.
				mergeToolCallDeltas(raw, pendingToolCalls)
				if !sseEventIsToolDeltaOnly(raw) {
					accumulateDelta(&acc, &tcs, raw, reasoning)
					// If this content-bearing event also carries tool call
					// deltas, strip them before forwarding. The client would
					// otherwise see a partial delta (e.g. empty arguments)
					// followed by the synthesized complete event — which
					// produces duplicate/confusing tool call output. The
					// accumulated deltas will be synthesized at [DONE] time.
					forwarded := stripToolCallDeltas(raw)
					// forwarded already ends with '\n' (from eventBuf.WriteByte('\n')),
					// so add just one more '\n' to form the SSE blank-line delimiter.
					if _, err := w.Write(forwarded); err != nil {
						return err
					}
					if _, err := w.Write([]byte("\n")); err != nil {
						return err
					}
					flusher.Flush()
				} else {
					accumulateDelta(&acc, &tcs, raw, reasoning)
				}
			} else {
				// Tool calls disabled: forward all events verbatim to the client.
				// "Verbatim" still normalizes the one field strict clients
				// require and some upstreams omit (tool_calls[].index).
				forwarded := normalizeToolCallIndices(raw)
				accumulateDelta(&acc, &tcs, forwarded, reasoning)
				if _, err := w.Write(forwarded); err != nil {
					return err
				}
				if _, err := w.Write([]byte("\n")); err != nil {
					return err
				}
				flusher.Flush()
			}
		} else {
			buffered.Write(raw)
			buffered.Write([]byte("\n"))
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()

		// SSE comment lines (":" prefixed) carry no payload — they are keepalives
		// or upstream hints. Per the SSE spec they are ignored entirely.
		//
		// They must not be appended to eventBuf: an event is dispatched on the
		// next blank line, so a buffered ": keepalive" would be dispatched as
		// if it were a complete event. sseEventIsRelease cannot parse it (it is
		// not JSON), so it releases and the accumulated buffer — including any
		// real chunks — gets flushed at a moment dictated by a comment. It also
		// inflates the buffer with bytes the client has no use for. Observed live
		// 2026-09-29: commandcode2 relays emit ": keepalive" between chunks.
		if len(bytes.TrimSpace(line)) > 0 && bytes.HasPrefix(bytes.TrimSpace(line), []byte(":")) {
			sseComments++
			continue
		}

		if len(line) == 0 {
			if eventBuf.Len() > 0 {
				raw := eventBuf.Bytes()
				if err := processEvent(raw); err != nil {
					return acc.String(), tcs, completionTokens, err
				}
				eventBuf.Reset()
			}
			continue
		}

		eventBuf.Write(line)
		eventBuf.WriteByte('\n')
	}

	if err := scanner.Err(); err != nil {
		// If we never released, this is an upstream failure before any content
		// reached the client: discard the buffer and fall back.
		return acc.String(), tcs, completionTokens, err
	}

	// Process any remaining event in the buffer. The upstream may close the
	// connection without a trailing blank line, leaving the last SSE event in
	// eventBuf. Without this, the final event (which could be the only content
	// or a usage payload) would be silently dropped.
	if eventBuf.Len() > 0 {
		raw := eventBuf.Bytes()
		if err := processEvent(raw); err != nil {
			return acc.String(), tcs, completionTokens, err
		}
		eventBuf.Reset()
	}

	// Flush any trailing buffered events (content that arrived before the
	// release point and was held back). [DONE] is never in the buffer when
	// toolCalls is enabled because processEvent intercepts it; for non-tool
	// mode it passes through normally.
	if err := flushBuffered(); err != nil {
		return acc.String(), tcs, completionTokens, err
	}

	// Close the model patch's stream: text it still holds back (a `<`
	// that never resolved, or a block whose closer never arrived) is
	// emitted as one final event so no thinking text is lost, and any
	// call completed by the flush is registered like the ones below.
	if patch != nil {
		final, extracted := patch.FlushStream(patchState)
		for _, c := range extracted {
			idx := takePendingToolCallIndex(pendingToolCalls)
			pendingToolCalls[idx] = &pendingTC{
				id:           c.ID,
				name:         c.Name,
				args:         c.Arguments,
				argsReceived: true,
				complete:     true,
			}
			tcs = append(tcs, streamToolCall{
				Index:     idx,
				ID:        c.ID,
				Type:      "function",
				Name:      c.Name,
				Arguments: c.Arguments,
			})
		}
		if final != nil {
			if _, err := w.Write([]byte("data: " + string(final) + "\n\n")); err != nil {
				return acc.String(), tcs, completionTokens, err
			}
			flusher.Flush()
		}
	}

	// Emit synthesized tool calls BEFORE [DONE] so the client parser still
	// processes them as part of the stream. Arriving after [DONE] they would
	// be silently dropped. Iterate in deterministic index order so the
	// client receives tool calls in the same order the upstream intended.
	//
	// This also runs with toolCalls disabled when a model patch
	// extracted calls: the patch's calls are complete by construction,
	// and the client must receive them even when raw upstream tool
	// deltas are forwarded verbatim.
	if toolCalls || len(pendingToolCalls) > 0 {
		var indices []int
		for idx := range pendingToolCalls {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			ptc := pendingToolCalls[idx]
			if emittedTC[idx] {
				continue
			}
			// Skip incomplete tool calls: a name with no arguments (or no name
			// at all) produces an invalid tool call event. If the upstream was
			// simply slow, the next Read would have given us more data; if it
			// genuinely omitted them, emitting empty args would break the
			// client parser.
			if ptc.name == "" || !ptc.complete {
				continue
			}
			synth := synthesizeToolCallEvent(idx, ptc, streamModel)
			if synth != nil {
				if _, err := w.Write(synth); err != nil {
					return acc.String(), tcs, completionTokens, err
				}
				emittedTC[idx] = true
				flusher.Flush()
			}
		}
	}

	// Emit the [DONE] sentinel exactly once, always last. Clients finalize
	// parsing here, after any synthesized tool calls.
	//
	// This is unconditional (it no longer checks sawDone): [DONE] is no
	// longer forwarded by the streaming path at all, so this is the single
	// point that terminates the response. Emitting it unconditionally also
	// covers an upstream that closed the stream without sending [DONE].
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return acc.String(), tcs, completionTokens, err
	}
	flusher.Flush()
	return acc.String(), tcs, completionTokens, nil
}

// rawReasoningText extracts a reasoning delta that is a JSON string, and
// ignores the object shapes some routers send (they carry no text we can
// replay, and a typed string field would fail the whole event's decode,
// dropping the content and tool calls that share it).
func rawReasoningText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// normalizeToolCallIndices makes every tool-call delta carry an explicit
// `index`, which the OpenAI schema requires and strict clients reject without.
//
// Gemini's OpenAI-compatible surface emits a COMPLETE tool call in a single
// chunk with no index, and the gateway forwards chunks verbatim when
// synthesized tool calls are disabled (the daemon default). Measured
// 2026-11-03 in maki's log: "failed to parse SSE chunk: missing field `index`"
// for every Gemini tool call; the client drops the chunk, so the turn ends
// with no tool call and the stream just stops.
//
// The position inside the chunk is the only correlation available when the
// upstream omits the field, and a complete single call (the Gemini shape) is
// index 0. Chunks that already carry an index are returned untouched, so this
// costs one Contains + one Unmarshal only on the offending shape.
func normalizeToolCallIndices(raw []byte) []byte {
	payload := sseDataPayload(raw)
	if !bytes.Contains(payload, []byte(`"tool_calls"`)) {
		return raw
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return raw
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return raw
	}
	changed := false
	for _, c := range choices {
		ch, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := ch["delta"].(map[string]any)
		if !ok {
			continue
		}
		tcs, ok := delta["tool_calls"].([]any)
		if !ok {
			continue
		}
		for i, tc := range tcs {
			m, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			if _, has := m["index"]; !has {
				m["index"] = i
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return rewriteSSEEventData(raw, out)
}

// sseEventIsRelease reports whether an SSE event is safe to flush to the
// client: either it is the [DONE] sentinel or it carries the first non-empty
// content delta. Before this point we keep events buffered so an error can be
// swallowed without the client noticing.
func sseEventIsRelease(raw []byte) bool {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}
	var obj struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index int `json:"index"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		// Unparseable: release to avoid stalling (fail open).
		return true
	}
	if len(obj.Choices) > 0 {
		if obj.Choices[0].Delta.Content != "" {
			return true
		}
		if len(obj.Choices[0].Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// sseDataPayload extracts the JSON payload from the first "data:" line of an
// SSE event buffer.
func sseDataPayload(raw []byte) []byte {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(line[len("data:"):])
		}
	}
	return bytes.TrimSpace(raw)
}

// extractStreamUsage returns the completion token count from an SSE event that
// carries an OpenAI-style usage payload. Some providers emit this after [DONE]
// so we can measure TPS for streaming requests.
func extractStreamUsage(raw []byte) int {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0
	}
	var out struct {
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return 0
	}
	if out.Usage != nil {
		return out.Usage.CompletionTokens
	}
	return 0
}

// streamToolCall accumulates a streamed tool_call across multiple SSE deltas so
// it can be replayed as part of an assistant message when resuming after a
// mid-stream failure. Arguments are concatenated across deltas for the same
// index (matching how providers stream them).
type streamToolCall struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

// pendingTC is the in-flight accumulator used by streamSSE to coalesce tool
// call deltas before forwarding them to the client. A pending tool call is
// only emitted to the client once complete=true, which means we have the
// name and have received at least one arguments chunk (even if it's empty
// string — some functions legitimately take no arguments).
type pendingTC struct {
	id, name, args string
	argsReceived   bool
	complete       bool
}

// accumulateDelta extracts content and tool_call deltas from already-flushed SSE
// events and appends them to acc / tcs, so a mid-stream fallback can resume on
// the next model by replaying what the client already received as an assistant
// message. It safely ignores [DONE] sentinels and unparseable events.
func accumulateDelta(acc *strings.Builder, tcs *[]streamToolCall, raw []byte, reasoning *strings.Builder) {
	for _, ev := range bytes.Split(raw, []byte("\n\n")) {
		if len(bytes.TrimSpace(ev)) == 0 {
			continue
		}
		payload := sseDataPayload(ev)
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content          string          `json:"content"`
					ReasoningContent json.RawMessage `json:"reasoning_content"`
					Reasoning        json.RawMessage `json:"reasoning"`
					ReasoningText    json.RawMessage `json:"reasoning_text"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		for _, ch := range obj.Choices {
			if ch.Delta.Content != "" {
				acc.WriteString(ch.Delta.Content)
			}
			if reasoning != nil {
				reasoning.WriteString(rawReasoningText(ch.Delta.ReasoningContent))
				reasoning.WriteString(rawReasoningText(ch.Delta.Reasoning))
				reasoning.WriteString(rawReasoningText(ch.Delta.ReasoningText))
			}
			for _, tc := range ch.Delta.ToolCalls {
				var target *streamToolCall
				for i := range *tcs {
					if (*tcs)[i].Index == tc.Index {
						target = &(*tcs)[i]
						break
					}
				}
				if target == nil {
					*tcs = append(*tcs, streamToolCall{Index: tc.Index})
					target = &(*tcs)[len(*tcs)-1]
				}
				if tc.ID != "" {
					target.ID = tc.ID
				}
				if tc.Type != "" {
					target.Type = tc.Type
				}
				if tc.Function.Name != "" {
					target.Name = tc.Function.Name
				}
				target.Arguments += tc.Function.Arguments
			}
		}
	}
}

func extractSSEErrorFromBuffer(buf *bytes.Buffer) error {
	if buf.Len() == 0 {
		return nil
	}
	return ExtractSSEError(buf.Bytes())
}

// mergeToolCallDeltas parses tool call deltas from an SSE event and merges
// them into the per-index pendingToolCalls map. Returns the number of
// individual tool-call deltas processed (one per tool call entry in the
// event, not per event) and a slice of indices that were newly marked
// complete (i.e. they were not complete before this call). The caller
// decides when to forward accumulated events to the client.
func mergeToolCallDeltas(raw []byte, pending map[int]*pendingTC) (int, []int) {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0, nil
	}
	var obj struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return 0, nil
	}
	count := 0
	var newlyCompleted []int
	for _, tc := range obj.Choices {
		for _, d := range tc.Delta.ToolCalls {
			count++
			ptc, ok := pending[d.Index]
			if !ok {
				ptc = &pendingTC{}
				pending[d.Index] = ptc
			}
			wasComplete := ptc.complete
			if d.ID != "" {
				ptc.id = d.ID
			}
			if d.Function.Name != "" {
				ptc.name = d.Function.Name
			}
			// Any delta with a "function" field counts as an arguments chunk,
			// even if Arguments is "". This lets no-arg tool calls complete.
			if d.Function.Name != "" || d.Function.Arguments != "" {
				ptc.argsReceived = true
			}
			ptc.args += d.Function.Arguments
			// A tool call is complete once we have the name and have received
			// at least one arguments chunk (which may be empty).
			if ptc.name != "" && ptc.argsReceived {
				ptc.complete = true
			}
			if ptc.complete && !wasComplete {
				newlyCompleted = append(newlyCompleted, d.Index)
			}
		}
	}
	return count, newlyCompleted
}

// sseEventIsToolDeltaOnly returns true when the SSE event carries only a
// tool_call delta and no content, usage, or other fields. These events are
// buffered in-process until the tool call is complete and should not be
// forwarded to the client as-is (the caller in processEvent does that).
func sseEventIsToolDeltaOnly(raw []byte) bool {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	var obj struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index int `json:"index"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct{} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}
	if len(obj.Choices) == 0 {
		return false
	}
	delta := obj.Choices[0].Delta
	if delta.Content != "" {
		return false
	}
	if len(delta.ToolCalls) > 0 {
		return true
	}
	return false
}

// stripToolCallDeltas removes tool_call deltas from an SSE event's JSON
// payload. This is used when a content-bearing event also carries tool call
// deltas: we forward the content to the client but suppress the partial tool
// call deltas, which will be synthesized as a complete event at [DONE] time.
// Returns the original raw bytes unchanged if there are no tool call deltas
// or if parsing fails (fail-open so we never drop a valid event).
func stripToolCallDeltas(raw []byte) []byte {
	payload := sseDataPayload(raw)
	var obj struct {
		Choices []struct {
			Delta *struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta,omitempty"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return raw
	}
	hasToolCalls := false
	for _, ch := range obj.Choices {
		if ch.Delta != nil && len(ch.Delta.ToolCalls) > 0 {
			hasToolCalls = true
			break
		}
	}
	if !hasToolCalls {
		return raw
	}
	// Remove tool_calls from each choice's delta.
	var out map[string]json.RawMessage
	if err := json.Unmarshal(payload, &out); err != nil {
		return raw
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(out["choices"], &choices); err != nil {
		return raw
	}
	for i, ch := range choices {
		var chObj map[string]json.RawMessage
		if err := json.Unmarshal(ch, &chObj); err != nil {
			continue
		}
		if delta, ok := chObj["delta"]; ok {
			var deltaObj map[string]json.RawMessage
			if err := json.Unmarshal(delta, &deltaObj); err != nil {
				continue
			}
			delete(deltaObj, "tool_calls")
			newDelta, err := json.Marshal(deltaObj)
			if err != nil {
				continue
			}
			chObj["delta"] = newDelta
		}
		newCh, err := json.Marshal(chObj)
		if err != nil {
			continue
		}
		choices[i] = newCh
	}
	newChoices, err := json.Marshal(choices)
	if err != nil {
		return raw
	}
	out["choices"] = newChoices
	result, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	// Reconstruct the SSE event with the modified payload.
	// raw may contain multiple lines (e.g. "data: ..." plus event/id lines).
	var lines [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		lines = append(lines, line)
	}
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			// Replace the data line with the stripped payload.
			prefix := line[:len(line)-len(trimmed)]
			lines[i] = append(prefix, "data: "+string(result)...)
			break
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// ErrUpstreamSaturated is returned when the upstream concurrency cap is
// exhausted and an attempt cannot acquire a slot in time. Handlers map it
// to 503 + Retry-After.
var ErrUpstreamSaturated = errors.New("upstream saturated")

// acquireUpstream acquires one upstream attempt slot. release must be
// called (typically via defer) when the attempt completes. Returns an
// error if the cap is full and the slot is not available promptly.
func (p *Proxy) acquireUpstream(ctx context.Context) (func(), error) {
	if p == nil || p.upstreamSlots == nil {
		return func() {}, nil
	}
	select {
	case p.upstreamSlots <- struct{}{}:
		return func() { <-p.upstreamSlots }, nil
	default:
	}
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case p.upstreamSlots <- struct{}{}:
		return func() { <-p.upstreamSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrUpstreamSaturated
	}
}
