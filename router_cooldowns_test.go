package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// loadCooldownsTestConfig builds a minimal config with one provider and one
// logical model, enough for NewRouter to construct a router whose cooldown
// file we can point at a temp dir.
func loadCooldownsTestConfig(t *testing.T) *Config {
	t.Helper()
	raw := `
providers:
  test:
    url: https://example.invalid/v1
    api_key_env: TEST_KEY
models:
  smart:
    chain:
      - provider: test
        model: m1
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return &cfg
}

// TestLoadCooldownsSurvivesMalformedCircuit is a regression test for a real
// operational incident: a hand-edited cooldowns.json carried a circuits entry
// with "state":"open" (a string) where CircuitState is an int (router.go
// CircuitState type). The merged-envelope json.Unmarshal is all-or-nothing, so
// that single bad value aborted the parse and dropped EVERY cooldown on disk.
//
// The result was that a manual cooldown edit silently reset all backoff state,
// and the router re-hammered every failing endpoint from a cold start. A bad
// circuit must now cost only that circuit.
func TestLoadCooldownsSurvivesMalformedCircuit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cooldowns.json")

	expiry := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	// "state":"open" is a string where the decoder expects a number — the exact
	// shape that used to wipe the whole file.
	content := `{
  "cooldowns": {
    "test:good-1": {"expiry":"` + expiry + `","status_code":429,"error_count":3,"last_error":"rate limited"},
    "test:good-2": {"expiry":"` + expiry + `","status_code":500,"error_count":2,"last_error":"boom"}
  },
  "circuits": {
    "test:bad-circuit": {"state":"open","opened_at":"` + expiry + `","probes_sent":0}
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cooldowns: %v", err)
	}

	r := NewRouter(loadCooldownsTestConfig(t), path)
	defer r.Close()

	got := r.GetAllCooldowns()
	if len(got) != 2 {
		t.Fatalf("malformed circuit discarded cooldowns: got %d, want 2 (%v)", len(got), keysOf(got))
	}
	for _, k := range []string{"test:good-1", "test:good-2"} {
		if _, ok := got[k]; !ok {
			t.Errorf("cooldown %s lost to a malformed sibling circuit", k)
		}
	}
	// The malformed circuit itself must be dropped rather than half-applied.
	if _, ok := r.GetAllCircuits()["test:bad-circuit"]; ok {
		t.Error("malformed circuit should not have been restored")
	}
}

// TestLoadCooldownsValidFileUnchanged guards the fix: a well-formed file must
// still restore both cooldowns and circuits exactly as before.
func TestLoadCooldownsValidFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cooldowns.json")

	expiry := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	content := `{
  "cooldowns": {
    "test:good-1": {"expiry":"` + expiry + `","status_code":429,"error_count":3,"last_error":"rate limited"}
  },
  "circuits": {
    "test:good-1": {"state":1,"opened_at":"` + expiry + `","probes_sent":0}
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cooldowns: %v", err)
	}

	r := NewRouter(loadCooldownsTestConfig(t), path)
	defer r.Close()

	if got := len(r.GetAllCooldowns()); got != 1 {
		t.Errorf("cooldowns: got %d, want 1", got)
	}
	cb, ok := r.GetAllCircuits()["test:good-1"]
	if !ok {
		t.Fatal("open circuit not restored from valid file")
	}
	if cb.State != CircuitOpen {
		t.Errorf("circuit state: got %v, want CircuitOpen", cb.State)
	}
}

func keysOf(m map[string]CooldownEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- DELETE /admin/cooldowns -------------------------------------------------

// TestAdminCooldownsDeleteOne verifies the operator can clear a single
// endpoint's backoff state without restarting the daemon, which is the whole
// point of the endpoint: hand-editing cooldowns.json under a running router
// gets silently overwritten from its in-memory copy.
func TestAdminCooldownsDeleteOne(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{
		InitialRotationWindow:   &noRotation,
		CircuitBreakerThreshold: intPtr(2),
	}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	bad := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	good := &ModelEndpoint{Provider: "openai", Model: "gpt-4o"}
	for _, ep := range []*ModelEndpoint{bad, good} {
		router.ApplyCooldown(ep, 500, "server error")
		router.ApplyCooldown(ep, 500, "server error")
	}
	if len(router.GetAllCooldowns()) != 2 {
		t.Fatalf("setup: want 2 cooldowns, got %d", len(router.GetAllCooldowns()))
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns?model="+bad.Key(), nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Cleared int    `json:"cleared"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Cleared == 0 {
		t.Error("expected cleared > 0")
	}
	if resp.Model != bad.Key() {
		t.Errorf("model: got %q, want %q", resp.Model, bad.Key())
	}

	// The named endpoint is gone; the untouched one must survive.
	got := router.GetAllCooldowns()
	if _, ok := got[bad.Key()]; ok {
		t.Error("targeted cooldown was not cleared")
	}
	if _, ok := got[good.Key()]; !ok {
		t.Error("clearing one model also cleared an unrelated one")
	}
}

// TestAdminCooldownsDeleteAll covers the bulk path: no model= clears
// everything, which is the operation that most needs to be correct because it
// drops all backoff protection at once.
func TestAdminCooldownsDeleteAll(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{
		InitialRotationWindow:   &noRotation,
		CircuitBreakerThreshold: intPtr(2),
	}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	for _, m := range []string{"gpt-4", "gpt-4o", "o3"} {
		ep := &ModelEndpoint{Provider: "openai", Model: m}
		router.ApplyCooldown(ep, 500, "server error")
		router.ApplyCooldown(ep, 500, "server error")
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(router.GetAllCooldowns()); n != 0 {
		t.Errorf("cooldowns after clear-all: got %d, want 0", n)
	}
	if n := len(router.GetAllCircuits()); n != 0 {
		t.Errorf("circuits after clear-all: got %d, want 0", n)
	}
}

// TestAdminCooldownsDeleteRequiresAuth: the endpoint clears backoff state and
// therefore exposes which endpoints are failing, and it lets an attacker force
// traffic back at a rate-limited provider. It must be behind the same auth as
// the GET it shares a route with.
func TestAdminCooldownsDeleteRequiresAuth(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{InitialRotationWindow: &noRotation}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	router.ApplyCooldown(ep, 500, "server error")

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns", nil)
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 401 {
		t.Fatalf("unauthenticated DELETE: expected 401, got %d", rec.Code)
	}
	if n := len(router.GetAllCooldowns()); n != 1 {
		t.Errorf("unauthenticated DELETE still cleared state (%d left)", n)
	}
}

// TestAdminCooldownsDeleteUnknownModelIs404 keeps a typo from looking like a
// successful clear — the operator needs to know nothing was reset.
func TestAdminCooldownsDeleteUnknownModelIs404(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{InitialRotationWindow: &noRotation}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns?model=nope%3Anope", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 404 {
		t.Fatalf("expected 404 for unknown model, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- fallback wall-clock bound -----------------------------------------------

// TestFallbackWallClockBoundIsSane guards the per-request fallback budget.
//
// maxAttempts is chainLen*3+1, which for the shipped config is 82 (smart),
// 145 (work) and 115 (large). At the 5s per-attempt timeout that applies to a
// small request, the loop could in principle run for minutes. A 4-token `smart`
// request was measured taking 62.6s on 2026-09-29, walking 20+ nvidia
// endpoints at 5s apiece. The wall-clock bound caps that.
func TestFallbackWallClockBoundIsSane(t *testing.T) {
	if maxFallbackWallClock <= 0 {
		t.Fatal("maxFallbackWallClock must be positive")
	}
	// A client should never wait longer than this; keep it under a minute so
	// proxies/clients with their own idle timeouts don't cut us off first.
	if maxFallbackWallClock > time.Minute {
		t.Errorf("maxFallbackWallClock = %v, want <= 1m", maxFallbackWallClock)
	}
}

// TestFallbackBudgetScalesWithRequestSize: a small request gets a much shorter
// budget than a large one, because its per-attempt timeout is also short, so
// the walk is nearly all dead time. Measured 2026-09-29: a 4-token `smart`
// request took 62.6s before the bound and 22.9s with a flat 45s ceiling —
// still a user staring at a spinner.
func TestFallbackBudgetScalesWithRequestSize(t *testing.T) {
	small := fallbackBudget(requestTimeout(4))      // 5s  per attempt -> 12s
	medium := fallbackBudget(requestTimeout(25000)) // 7s  per attempt -> 20s
	large := fallbackBudget(requestTimeout(200000)) // 25s per attempt -> 45s (ceiling)

	if small != 12*time.Second {
		t.Errorf("small-request budget = %v, want 12s", small)
	}
	if small >= medium {
		t.Errorf("small-request budget %v should be shorter than medium %v", small, medium)
	}
	if medium >= large {
		t.Errorf("medium-request budget %v should be shorter than large %v", medium, large)
	}
	// The small-request budget exists specifically to cap a spinner at a
	// tolerable length; a flat 45s is what we are trying to avoid here.
	if small > 15*time.Second {
		t.Errorf("small-request budget %v is too long; it defeats the purpose", small)
	}
	if large > maxFallbackWallClock {
		t.Errorf("large-request budget %v exceeds the hard ceiling %v", large, maxFallbackWallClock)
	}
}

// TestFallbackBudgetAlwaysAllowsTwoAttempts: whenever two attempts actually
// fit inside the hard ceiling, the budget must cover both, so a single
// transient failure never ends a request.
//
// For a very large context a single attempt can itself approach the 45s
// ceiling; there the ceiling wins deliberately (see fallbackBudget) rather
// than letting the effective budget drift back to minutes.
func TestFallbackBudgetAlwaysAllowsTwoAttempts(t *testing.T) {
	for _, tokens := range []int{0, 4, 1000, 10_000, 100_000, 500_000, 2_000_000} {
		timeout := requestTimeout(tokens)
		budget := fallbackBudget(timeout)
		if budget <= 0 {
			t.Fatalf("tokens=%d: non-positive budget %v", tokens, budget)
		}
		if budget > maxFallbackWallClock {
			t.Errorf("tokens=%d: budget %v exceeds hard ceiling %v", tokens, budget, maxFallbackWallClock)
		}
		if 2*timeout <= maxFallbackWallClock && budget < 2*timeout {
			t.Errorf("tokens=%d: budget %v < 2 x per-attempt timeout %v, but both fit "+
				"inside the %v ceiling", tokens, budget, timeout, maxFallbackWallClock)
		}
	}
}

// TestStreamSSE_DoneIsAlwaysLastAndEmittedOnce guards the SSE termination
// contract. A client stops parsing at the first `data: [DONE]`, so a stream
// that starts with one loses every real chunk that follows.
//
// The failure mode this pins: a provider whose deltas carry nothing the
// client treats as content (kilocode/dots-studio sends role-only deltas with
// `content:""` and a `reasoning` field) never satisfies the release
// condition in sseEventIsRelease, so [DONE] is the event that first sets
// released=true. The old code then buffered the sentinel, and the post-loop
// flush emitted that buffer ahead of its own [DONE] — [DONE] first, real
// chunks after, then a duplicate. Observed live 2026-09-29 on `work` and
// `smart` against dots-studio, where upstream's own stream is well-formed
// ([DONE] last), confirming the fault was ours.
//
// Both tool-call modes are covered: with coalescing on, [DONE] is held back
// for synthesized tool calls; with it off (the daemon default), [DONE] used
// to flow straight through the buffer.
func TestStreamSSE_DoneIsAlwaysLastAndEmittedOnce(t *testing.T) {
	// A reasoning-only stream: every delta is role-only with empty content,
	// so nothing releases until [DONE].
	const reasoningOnly = `data: {"id":"gen-1","object":"chat.completion.chunk","model":"dots-studio/dots-3-note-preview:free","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"The user"}}]}

data: {"id":"gen-1","object":"chat.completion.chunk","model":"dots-studio/dots-3-note-preview:free","choices":[{"index":0,"delta":{"content":"","reasoning":" wants"}}]}

data: {"id":"gen-1","object":"chat.completion.chunk","model":"dots-studio/dots-3-note-preview:free","choices":[{"index":0,"delta":{"content":"","role":"assistant"}}]}

data: [DONE]

`

	for _, tc := range []struct {
		name      string
		toolCalls bool
	}{
		{"toolCallsDisabled", false}, // the daemon's shipped default
		{"toolCallsEnabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			flusher := &testFlusher{buf}
			p := &Proxy{}
			p.SetToolCalls(tc.toolCalls)

			if _, _, _, err := p.streamSSE(flusher, flusher, strings.NewReader(reasoningOnly)); err != nil {
				t.Fatalf("streamSSE returned error: %v", err)
			}

			out := buf.String()
			// The content chunks must be flushed BEFORE the sentinel. A
			// client stops parsing at the first [DONE], so a sentinel that
			// appears ahead of any chunk truncates the response.
			firstChunk := strings.Index(out, "data: {")
			firstDone := strings.Index(out, "data: [DONE]")
			if firstDone < firstChunk {
				t.Errorf("[DONE] at byte %d precedes the first chunk at byte %d in:\n%s",
					firstDone, firstChunk, out)
			}
			if n := strings.Count(out, "data: [DONE]"); n != 1 {
				t.Errorf("want exactly 1 [DONE] sentinel, got %d in:\n%s", n, out)
			}
			if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
				t.Errorf("[DONE] must be the final event; stream ended with:\n%s", out)
			}
			// The real chunks must survive: they are what the client is after.
			if !strings.Contains(out, `"gen-1"`) {
				t.Errorf("stream lost its content chunks; got:\n%s", out)
			}
			if strings.TrimSpace(out) == "" {
				t.Error("stream produced no output at all")
			}
		})
	}
}

// TestStreamSSE_EmitsDoneWhenUpstreamOmitsIt covers an upstream that closes the
// connection without sending [DONE]. Since the sentinel is now emitted
// unconditionally at the end of streamSSE, the client still gets a properly
// terminated stream instead of hanging until the connection drops.
func TestStreamSSE_EmitsDoneWhenUpstreamOmitsIt(t *testing.T) {
	const noSentinel = `data: {"id":"gen-2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}

`
	buf := &bytes.Buffer{}
	flusher := &testFlusher{buf}
	p := &Proxy{}
	p.SetToolCalls(false)

	if _, _, _, err := p.streamSSE(flusher, flusher, strings.NewReader(noSentinel)); err != nil {
		t.Fatalf("streamSSE returned error: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Errorf("want exactly 1 [DONE] sentinel, got %d in:\n%s", n, out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Errorf("[DONE] must be the final event; stream ended with:\n%s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("stream lost its content chunk; got:\n%s", out)
	}
}

// TestStreamSSE_IgnoresSSECommentLines covers upstreams that interleave `:`
// comment lines (keepalives) with data events — commandcode2's relay does
// this. A comment line is not a data event: the SSE spec says to ignore it.
//
// The old scanner appended every non-blank line to eventBuf, so a ": keepalive"
// became a pseudo-event dispatched on the next blank line. sseEventIsRelease
// cannot JSON-parse it and fails open, so a comment would release the stream
// and flush the buffer at an arbitrary point, and the comment itself was
// forwarded to the client. Observed live 2026-09-29 as `fast` streams where a
// keepalive mid-stream produced a [DONE] ahead of the remaining chunks.
func TestStreamSSE_IgnoresSSECommentLines(t *testing.T) {
	const withKeepalives = `: keepalive

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}

: keepalive

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"llo"}}]}

: keepalive

data: [DONE]

`
	for _, toolCalls := range []bool{false, true} {
		buf := &bytes.Buffer{}
		flusher := &testFlusher{buf}
		p := &Proxy{}
		p.SetToolCalls(toolCalls)

		acc, _, _, err := p.streamSSE(flusher, flusher, strings.NewReader(withKeepalives))
		if err != nil {
			t.Fatalf("toolCalls=%v: streamSSE returned error: %v", toolCalls, err)
		}
		out := buf.String()

		if strings.Contains(out, ": keepalive") {
			t.Errorf("toolCalls=%v: SSE comment line leaked to the client:\n%s", toolCalls, out)
		}
		if n := strings.Count(out, "data: [DONE]"); n != 1 {
			t.Errorf("toolCalls=%v: want exactly 1 [DONE], got %d in:\n%s", toolCalls, n, out)
		}
		if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
			t.Errorf("toolCalls=%v: [DONE] must be last; stream ended with:\n%s", toolCalls, out)
		}
		if acc != "hello" {
			t.Errorf("toolCalls=%v: accumulated content = %q, want %q", toolCalls, acc, "hello")
		}
		// The chunks must appear before the sentinel, in order.
		hi, lo := strings.Index(out, `"he"`), strings.Index(out, `"llo"`)
		done := strings.Index(out, "data: [DONE]")
		if hi < 0 || lo < 0 || !(hi < lo && lo < done) {
			t.Errorf("toolCalls=%v: chunks out of order (he=%d llo=%d done=%d):\n%s",
				toolCalls, hi, lo, done, out)
		}
	}
}

// assertSSEWellFormed checks the termination contract on a captured stream
// body: exactly one [DONE], it is the final data event, and it comes after
// every content chunk. Shared by the fallback tests below.
func assertSSEWellFormed(t *testing.T, out string) {
	t.Helper()
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Errorf("want exactly 1 [DONE] sentinel, got %d in:\n%s", n, out)
	}
	firstChunk := strings.Index(out, "data: {")
	firstDone := strings.Index(out, "data: [DONE]")
	if firstDone < firstChunk {
		t.Errorf("[DONE] at byte %d precedes the first chunk at byte %d — the client "+
			"would drop everything after it:\n%s", firstDone, firstChunk, out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Errorf("[DONE] must be the final event; stream ended with:\n%s", out)
	}
}

// TestHandleStream_MidStreamFallbackEmitsDoneOnce covers the fallback path at
// the gateway level, not just inside streamSSE.
//
// A failed attempt writes a terminal [DONE] from its own streamSSE post-loop
// before the caller decides to fall back. If the resumed attempt then writes
// its own [DONE] — or if handleStream's deferred sendDone() adds a third —
// the client sees more than one sentinel. A conforming client stops parsing at
// the first one, so the resumed content that follows is discarded and the
// response is silently truncated even though the gateway "succeeded".
//
// This goes through the real HandleChatCompletions -> handleStream path (the
// sibling test TestProviderProxyStreamingMidStreamResume drives the proxy loop
// by hand and so never exercises the deferred sendDone()).
func TestHandleStream_MidStreamFallbackEmitsDoneOnce(t *testing.T) {
	noRotation := 0

	// backend1 flushes one content chunk, then an SSE error. It never reaches
	// [DONE] itself — the error event terminates the attempt.
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"b1","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"error":{"message":"upstream died","type":"server_error"}}`+"\n\n")
		flusher.Flush()
	}))
	defer backend1.Close()

	// backend2 completes normally and sends the real [DONE].
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"b2","object":"chat.completion.chunk","created":2,"model":"m2","choices":[{"index":0,"delta":{"role":"assistant","content":"World"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"id":"b2","object":"chat.completion.chunk","created":2,"model":"m2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		flusher.Flush()
	}))
	defer backend2.Close()

	cfg := &Config{
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
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
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)
	gateway.SetTestCooldown(200 * time.Millisecond)

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	out := rec.Body.String()

	// The resume must actually have happened, or this proves nothing.
	if !strings.Contains(out, "Hello ") || !strings.Contains(out, "World") {
		t.Fatalf("expected content from both the failed and resumed attempts, got:\n%s", out)
	}
	assertSSEWellFormed(t, out)
}

// TestHandleStream_UpstreamClosesWithoutDoneAcrossFallback is the same
// invariant for the abrupt-death path: backend1 is killed mid-stream (no [DONE]
// at all), and the gateway resumes on backend2. Each streamSSE call emits its
// own sentinel unconditionally, so the question is whether handleStream's
// deferred sendDone() stacks a further one on top.
func TestHandleStream_UpstreamClosesWithoutDoneAcrossFallback(t *testing.T) {
	noRotation := 0

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"b1","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		// Die without [DONE] and without an error event.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"b2","object":"chat.completion.chunk","created":2,"model":"m2","choices":[{"index":0,"delta":{"role":"assistant","content":"World"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
	}))
	defer backend2.Close()

	cfg := &Config{
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Providers: map[string]ProviderConfig{
			"backend1": {URL: backend1.URL},
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
	gateway := NewGatewayContext(router, proxy, cfg, "", "", true)
	gateway.SetTestCooldown(200 * time.Millisecond)

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	gateway.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", errCode(rec))
	}
	out := rec.Body.String()
	if !strings.Contains(out, "World") {
		t.Fatalf("expected resumed content from backend2, got:\n%s", out)
	}
	assertSSEWellFormed(t, out)
}

func errCode(rec *httptest.ResponseRecorder) int { return rec.Code }

// TestSSEEventIsRelease_FailOpen pins the deliberate fail-open behaviour of the
// release predicate.
//
// sseEventIsRelease returns true for anything it cannot JSON-parse. That is
// intentional: if the predicate stalled on an unrecognised event shape, the
// stream would never be released and the client would hang until the
// connection dropped. Failing open trades a possible premature flush for
// never hanging.
//
// It is worth pinning because fail-open is exactly what made the `: keepalive`
// bug destructive: a keepalive comment is not JSON, so it released the stream
// and flushed the buffer at a moment dictated by a comment rather than by
// content. The real fix was to never route comments into the event path at
// all (see TestStreamSSE_IgnoresSSECommentLines), NOT to make this predicate
// fail-closed. A future change that "tightens" this to return false on parse
// errors would reintroduce silent truncation.
func TestSSEEventIsRelease_FailOpen(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"doneSentinel", "data: [DONE]\n", true},
		{"realContent", `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n", true},
		{"toolCallDelta", `data: {"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}` + "\n", true},
		{"roleOnlyDelta", `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n", false},
		{"reasoningOnlyDelta", `data: {"choices":[{"delta":{"reasoning":"thinking","content":""}}]}` + "\n", false},
		{"emptyChoices", `data: {"choices":[]}` + "\n", false},

		// Fail-open cases: unparseable payloads must still release.
		{"unparseable", "data: {not json at all\n", true},
		{"emptyPayload", "data: \n", true},
		{"bareCommentShape", ": keepalive\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sseEventIsRelease([]byte(tc.raw)); got != tc.want {
				t.Errorf("sseEventIsRelease(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
