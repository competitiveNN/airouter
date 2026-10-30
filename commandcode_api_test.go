package main

// Tests for the standalone Command Code endpoint (commandcode_api.go).
//
// These drive the real HTTP surface against a fake Command Code upstream, so
// they cover the parts a unit test of the translator cannot: authentication,
// the model passthrough contract, key failover, and the non-streaming fold.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ccShimNDJSON is a minimal successful Command Code event stream: a start
// event, one text delta, and a finish event with usage. The translators require
// the finish event (truncation is an error) and at least one output token.
const ccShimNDJSON = `{"type":"start"}
{"type":"text-delta","text":"Hello"}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":1}}
`

// ccShimUpstream is a fake Command Code upstream. It answers the background
// fingerprint/lifecycle pre-requests with an empty object and every
// /alpha/generate call with whatever respond returns.
type ccShimUpstream struct {
	mu      sync.Mutex
	paths   []string
	gen     int
	respond func(w http.ResponseWriter, r *http.Request, gen int)
}

func (u *ccShimUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		if r.URL.Path != "/alpha/generate" {
			u.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
			return
		}
		u.gen++
		gen := u.gen
		respond := u.respond
		u.mu.Unlock()

		w.Header().Set("Content-Type", "application/x-ndjson")
		respond(w, r, gen)
	}
}

func (u *ccShimUpstream) generates() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gen
}

func (u *ccShimUpstream) sawGenerate() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, p := range u.paths {
		if p == "/alpha/generate" {
			return true
		}
	}
	return false
}

// ccShimGateway builds a gateway whose commandcode/commandcode2 providers both
// point at the given fake upstream servers.
func ccShimGateway(t *testing.T, ups map[string]*ccShimUpstream, chains map[string][]ModelEndpoint) (*CommandCodeShim, *httptest.Server) {
	t.Helper()
	providers := map[string]ProviderConfig{}
	names := make([]string, 0, len(ups))
	for name, u := range ups {
		srv := httptest.NewServer(u.handler())
		t.Cleanup(srv.Close)
		providers[name] = ProviderConfig{URL: srv.URL}
		names = append(names, name)
	}
	// Keep the declared order stable: commandcode before commandcode2.
	ordered := make([]string, 0, len(names))
	for _, name := range []string{"commandcode", "commandcode2"} {
		if _, ok := providers[name]; ok {
			ordered = append(ordered, name)
		}
	}
	for _, name := range names {
		if _, ok := providers[name]; !ok {
			continue
		}
		if strings.HasPrefix(name, "commandcode") && !containsString(ordered, name) {
			ordered = append(ordered, name)
		}
	}

	models := chains
	if models == nil {
		models = map[string][]ModelEndpoint{"smart": {{Provider: ordered[0], Model: "cc/one"}}}
	}
	cfg := &Config{Providers: providers, Models: map[string]ModelConfig{}}
	for logical, chain := range models {
		cfg.Models[logical] = ModelConfig{Chain: chain}
	}

	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	g := NewGatewayContext(router, NewProxy(cfg), cfg, "", testGatewayKey, false)
	proxy := g.proxy
	proxy.SetProtocolResolver(router.ProtocolFor)

	shim := NewCommandCodeShim(g, ordered, 0)
	srv := httptest.NewServer(shim.Handler())
	t.Cleanup(srv.Close)
	return shim, srv
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func ccShimPost(t *testing.T, srv *httptest.Server, path, body string, auth bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The endpoint is a second listener, not a bypass: it must authenticate with
// the same gateway key and refuse politely without it.
func TestCommandCodeShimRequiresAuth(t *testing.T) {
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{"commandcode": up}, nil)

	resp := ccShimPost(t, srv, "/v1/chat/completions",
		`{"model":"cc/one","messages":[{"role":"user","content":"hi"}]}`, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if up.sawGenerate() {
		t.Fatal("an unauthenticated request reached the upstream")
	}
}

// The model the client names is the model sent upstream. There is no logical
// model in this path, so an id the gateway has never heard of must still work.
func TestCommandCodeShimPassesModelThrough(t *testing.T) {
	var gotModel string
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		raw, _ := io.ReadAll(r.Body)
		// The model travels inside the envelope's params, not at the top level.
		var env struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &env); err == nil {
			gotModel = env.Params.Model
		}
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{"commandcode": up}, nil)

	body := `{"model":"vendor/unlisted-model-9","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := ccShimPost(t, srv, "/v1/chat/completions", body, true)
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
	}
	if gotModel != "vendor/unlisted-model-9" {
		t.Fatalf("upstream model = %q, want the client's model id unchanged", gotModel)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(string(out), `"content":"Hello"`) {
		t.Fatalf("streamed content missing:\n%s", out)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", out)
	}
}

// A rate-limited key must not reach the client: the next key serves the same
// session. The ordering is the shim's own session-affine rotation, so the test
// asks the shim which provider it will try first instead of guessing.
func TestCommandCodeShimFallsThroughRateLimitedKey(t *testing.T) {
	limited := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}}
	healthy := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	shim, srv := ccShimGateway(t, map[string]*ccShimUpstream{
		"commandcode":  limited,
		"commandcode2": healthy,
	}, nil)

	body := `{"model":"cc/one","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	// Session affinity picks the starting key, so pick a conversation whose
	// start is the rate-limited provider: otherwise the healthy key answers
	// first and the failover this test is about never happens.
	for i := 1; i <= 512; i++ {
		candidate := fmt.Sprintf(`{"model":"cc/one","stream":true,"messages":[{"role":"user","content":"hi %d"}]}`, i)
		if shim.endpoints("cc/one", bodySessionID([]byte(candidate)))[0].Provider == "commandcode" {
			body = candidate
			break
		}
	}
	order := shim.endpoints("cc/one", bodySessionID([]byte(body)))
	if len(order) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(order))
	}
	if order[0].Provider != "commandcode" {
		t.Fatalf("no conversation found that starts on the limited key: %v", order)
	}

	resp := ccShimPost(t, srv, "/v1/chat/completions", body, true)
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the rate limit was hidden from the client): %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), `"content":"Hello"`) {
		t.Fatalf("no content from the healthy key:\n%s", out)
	}
	// Both keys must have been used: the walk reaches the healthy one, and if
	// the limited key was first it was tried and penalized.
	if !limited.sawGenerate() || !healthy.sawGenerate() {
		t.Fatalf("both keys should have been contacted (limited=%d healthy=%d)",
			limited.generates(), healthy.generates())
	}
}

// A non-streaming client still gets one Chat Completions object: the upstream
// always answers with the NDJSON event stream, which is folded here.
func TestCommandCodeShimAggregatesNonStreamingResponse(t *testing.T) {
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{"commandcode": up}, nil)

	body := `{"model":"cc/one","messages":[{"role":"user","content":"hi"}]}`
	resp := ccShimPost(t, srv, "/v1/chat/completions", body, true)
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatalf("unmarshal completion: %v\n%s", err, out)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", completion.Object)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "Hello" {
		t.Fatalf("choices = %+v, want one message with content Hello", completion.Choices)
	}
}

// Command Code reports an exhausted balance as 402. The client must see the
// OpenAI-equivalent 429, not a payment-required status it cannot act on.
func TestCommandCodeShimMapsUpstreamStatus(t *testing.T) {
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient credits","code":"PAYMENT_REQUIRED"}}`))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{"commandcode": up}, nil)

	body := `{"model":"cc/one","messages":[{"role":"user","content":"hi"}]}`
	resp := ccShimPost(t, srv, "/v1/chat/completions", body, true)
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (402 is mapped to the rate-limit family): %s", resp.StatusCode, out)
	}
}

// The listing comes from the configured chains, so a client sees the Command
// Code models the gateway actually routes to - and nothing else.
func TestCommandCodeShimListsOnlyItsOwnModels(t *testing.T) {
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{
		"commandcode":  up,
		"commandcode2": up,
	}, map[string][]ModelEndpoint{
		"smart": {
			{Provider: "commandcode", Model: "cc/one"},
			{Provider: "commandcode2", Model: "cc/two"},
		},
		"fast": {
			{Provider: "commandcode2", Model: "cc/two"},
			{Provider: "someother", Model: "not-cc"},
		},
	})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range listed.Data {
		ids[m.ID] = true
	}
	if len(ids) != 2 || !ids["cc/one"] || !ids["cc/two"] {
		t.Fatalf("listed = %v, want exactly cc/one and cc/two", ids)
	}
}

// The shim has no Responses surface; a client pointed at the wrong path should
// get an explanation rather than a bare 404 from the mux.
func TestCommandCodeShimRefusesResponsesSurface(t *testing.T) {
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{"commandcode": up}, nil)

	resp := ccShimPost(t, srv, "/v1/responses", `{"model":"cc/one","input":"hi"}`, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "/v1/chat/completions") {
		t.Fatalf("error should name the supported surface: %s", out)
	}
}

// Without a configured provider there is nothing to serve, so main must not
// start a listener (and Enabled must say so).
func TestCommandCodeShimDisabledWithoutProviders(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{},
		Models:    map[string]ModelConfig{},
	}
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	g := NewGatewayContext(router, NewProxy(cfg), cfg, "", testGatewayKey, false)

	if shim := NewCommandCodeShim(g, []string{"commandcode", "commandcode2"}, 0); shim.Enabled() {
		t.Fatal("shim reports enabled with no providers configured")
	}
	if shim := NewCommandCodeShim(g, nil); shim.Enabled() {
		t.Fatal("shim reports enabled with an empty provider list")
	}
}

// An empty model is a client error, not something to forward: there is no
// logical model to resolve here, so a request without one cannot be routed.
func TestCommandCodeShimRejectsMissingModel(t *testing.T) {
	up := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	_, srv := ccShimGateway(t, map[string]*ccShimUpstream{"commandcode": up}, nil)

	resp := ccShimPost(t, srv, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}]}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "model") {
		t.Fatalf("error should name the missing field: %s", out)
	}
	if up.sawGenerate() {
		t.Fatal("a body without a model reached the upstream")
	}
}

// A session must stick to one key: the same body has to produce the same start
// index every time, or a conversation would hop between keys on every turn.
func TestCommandCodeShimSessionAffinityIsStable(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"a": {URL: "http://127.0.0.1:1"},
			"b": {URL: "http://127.0.0.1:1"},
			"c": {URL: "http://127.0.0.1:1"},
		},
		Models: map[string]ModelConfig{},
	}
	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	g := NewGatewayContext(router, NewProxy(cfg), cfg, "", testGatewayKey, false)
	s := NewCommandCodeShim(g, []string{"a", "b", "c"}, 0)
	id := "ctx:00112233445566778899aabbccddeeff"
	first := s.endpoints("m", id)
	for i := 0; i < 20; i++ {
		again := s.endpoints("m", id)
		for j := range first {
			if first[j].Provider != again[j].Provider {
				t.Fatalf("order changed between calls: %v vs %v", first, again)
			}
		}
	}

	// The rotation has to depend on the session: the start index of each of
	// these conversations must be the hashed one, and at least one of them must
	// land on a provider other than the first. A shim that always started at
	// provider zero would still be "stable" above, so this is the assertion
	// that the affinity is actually applied.
	rotated := false
	for i := 0; i < 64; i++ {
		sid := fmt.Sprintf("ctx:%032d", i)
		eps := s.endpoints("m", sid)
		want := s.providerNames[sessionIndex(sid, len(s.providerNames))]
		if eps[0].Provider != want {
			t.Fatalf("session %q starts at %q, want %q", sid, eps[0].Provider, want)
		}
		if want != s.providerNames[0] {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("no session rotated off the first provider: session affinity is not applied")
	}
	// Every provider appears exactly once: the rotation reorders, it does not
	// drop or duplicate candidates.
	seen := map[string]int{}
	for _, ep := range first {
		seen[ep.Provider]++
	}
	if len(seen) != 3 || seen["a"] != 1 || seen["b"] != 1 || seen["c"] != 1 {
		t.Fatalf("endpoints = %v, want each provider once", first)
	}
}

// TestCommandCodeShimStreamTerminatesOnMidStreamFailure pins the contract that
// makes the client's stream well-formed even when the upstream dies after
// partial output. The shim's stream() path used to return from that branch
// with nothing to terminate the response: the client received the partial
// chunks it had flushed and then a truncated stream — no [DONE], no EOF, no
// error — and its parser hung waiting for the next event.
//
// The fix is a deferred sendDone() that emits [DONE] on every exit path, and
// a doneSent flag so the success path (StreamToClient already wrote [DONE])
// does not emit a second sentinel.
func TestCommandCodeShimStreamTerminatesOnMidStreamFailure(t *testing.T) {
	// First provider writes a partial chunk then drops the connection.
	first := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"start"}
{"type":"text-delta","text":"Hello"}`))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Abrupt close: no finish event, no [DONE].
		h, ok := w.(http.Hijacker)
		if ok {
			c, _, _ := h.Hijack()
			c.Close()
		}
	}}
	// Second provider answers cleanly; the shim must not reach it once
	// bytes are on the wire (the client owns the stream), but the response
	// must still terminate.
	second := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = w.Write([]byte(ccShimNDJSON))
	}}
	shim, srv := ccShimGateway(t, map[string]*ccShimUpstream{
		"commandcode":  first,
		"commandcode2": second,
	}, map[string][]ModelEndpoint{
		"smart": {
			{Provider: "commandcode", Model: "cc/one"},
			{Provider: "commandcode2", Model: "cc/two"},
		},
	})
	_ = shim

	body := `{"model":"cc/one","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	// Session affinity picks the starting key, so find a conversation that
	// starts on the first provider: otherwise the healthy second key
	// answers first and the mid-stream failure this test is about never
	// happens.
	for i := 1; i <= 512; i++ {
		candidate := fmt.Sprintf(`{"model":"cc/one","stream":true,"messages":[{"role":"user","content":"hi %d"}]}`, i)
		if shim.endpoints("cc/one", bodySessionID([]byte(candidate)))[0].Provider == "commandcode" {
			body = candidate
			break
		}
	}
	resp := ccShimPost(t, srv, "/v1/chat/completions", body, true)
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), `"content":"Hello"`) {
		t.Fatalf("expected the partial content the client already received, got:\n%s", out)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Fatalf("client stream was not terminated with [DONE]:\n%s", out)
	}
	// The second provider must not have been contacted: once bytes are on
	// the wire the stream belongs to the client and a restart would
	// duplicate output.
	if second.sawGenerate() {
		t.Error("second provider was contacted after the first flushed partial output; the client's stream must not be restarted")
	}
}
