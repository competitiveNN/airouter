package main

// Command Code API endpoint.
//
// Command Code (api.commandcode.ai) is not an OpenAI-compatible surface: it
// answers POST /alpha/generate with an NDJSON event stream and has its own
// envelope, fingerprint lifecycle and header set. That translation already
// lives in commandcode.go, but it is reachable only from the *inside* of the
// router: a logical model (smart/work/fast/large) has to contain a Command Code
// endpoint in its fallback chain, and the far end must speak Chat Completions.
//
// This file exposes the same protocol support as a standalone OpenAI-compatible
// endpoint on its own listener, which is what the separate commandcode-proxy
// process used to do (the `http://127.0.0.1:3050/v1` provider in
// config.example.yaml). Clients POST ordinary Chat Completions bodies and name
// ANY Command Code model id; the shim does not map the name through a logical
// model, does not reorder a chain and does not rewrite the request, because the
// caller here is already talking about the concrete model it wants.
//
// What it does share with the gateway proper is everything that makes failure
// invisible: the same request budget (requestTimeout), the same stream
// translation and mid-stream failover (Proxy.StreamToClient), the same cooldown
// store (so a key that just 429'd is skipped by both surfaces), and the same
// authentication.
//
// Failover is across the configured Command Code providers, in order: each one
// is a different API key against the same upstream, so a rate-limited first key
// falls through to the next without the client noticing. A session sticks to
// the provider it started on (the start index is derived from the session id)
// so a conversation does not bounce between keys until one of them fails.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// CommandCodeShim is the OpenAI-compatible frontend backed by the Command Code
// protocol.
type CommandCodeShim struct {
	gateway *GatewayContext
	// providerNames is the configured order of providers to try. Names are
	// resolved against the live config on every request, so a provider removed
	// by a hot reload stops being used immediately instead of only after a
	// restart.
	providerNames []string
}

// NewCommandCodeShim builds the shim. providerNames is a comma-separated list
// of provider names from the config (for example "commandcode,commandcode2"),
// in failover order.
func NewCommandCodeShim(g *GatewayContext, providerNames []string) *CommandCodeShim {
	names := make([]string, 0, len(providerNames))
	for _, name := range providerNames {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return &CommandCodeShim{gateway: g, providerNames: names}
}

// Enabled reports whether there is anything to serve. It is false when the
// operator configured no Command Code providers, in which case main starts no
// listener at all.
func (s *CommandCodeShim) Enabled() bool {
	if s == nil || len(s.providerNames) == 0 {
		return false
	}
	cfg := s.gateway.config.Load()
	for _, name := range s.providerNames {
		if _, ok := cfg.Providers[name]; ok {
			return true
		}
	}
	return false
}

// Handler is the shim's mux. Both the /v1-prefixed and bare paths are served
// because the reference proxy accepted both (some clients append /v1
// themselves, some take the base URL verbatim).
func (s *CommandCodeShim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.HandleChatCompletions)
	mux.HandleFunc("/chat/completions", s.HandleChatCompletions)
	mux.HandleFunc("/v1/responses", s.HandleUnsupportedResponses)
	mux.HandleFunc("/responses", s.HandleUnsupportedResponses)
	mux.HandleFunc("/v1/models", s.HandleModels)
	mux.HandleFunc("/models", s.HandleModels)
	mux.HandleFunc("/health", s.HandleHealth)
	return mux
}

// endpoints resolves the live provider list into this request's candidate
// endpoints. The requested model is used verbatim: the shim's contract is that
// the caller names the Command Code model it wants.
func (s *CommandCodeShim) endpoints(model, sessionID string) []ModelEndpoint {
	cfg := s.gateway.config.Load()
	eps := make([]ModelEndpoint, 0, len(s.providerNames))
	for _, name := range s.providerNames {
		if _, ok := cfg.Providers[name]; !ok {
			continue
		}
		eps = append(eps, ModelEndpoint{
			Provider: name,
			Model:    model,
			Protocol: string(protocolCommandCode),
		})
	}
	if len(eps) < 2 {
		return eps
	}
	// Sticky start per session, so a conversation keeps using the key it began
	// on. The rotation is only over the start position; the walk below still
	// falls through to the next key on failure.
	start := sessionIndex(sessionID, len(eps))
	if start == 0 {
		return eps
	}
	ordered := make([]ModelEndpoint, 0, len(eps))
	ordered = append(ordered, eps[start:]...)
	ordered = append(ordered, eps[:start]...)
	return ordered
}

// sessionIndex hashes a session id into [0, n). An empty session id and a
// single candidate both yield 0, which keeps the common case free of any
// reordering.
func sessionIndex(sessionID string, n int) int {
	if n <= 1 || sessionID == "" {
		return 0
	}
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(sessionID); i++ {
		h ^= uint32(sessionID[i])
		h *= prime32
	}
	return int(h % uint32(n))
}

// HandleChatCompletions serves POST /v1/chat/completions.
func (s *CommandCodeShim) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.gateway.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := readRequestBody(r)
	if err != nil {
		writeAPIError(w, 400, "Failed to read request body", "invalid_request_error", "bad_request")
		return
	}

	// Only the two routing knobs are decoded; the body is forwarded as it
	// arrived, exactly like the gateway's own chat path.
	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		return
	}
	model := strings.TrimSpace(head.Model)
	if model == "" {
		writeAPIError(w, 400, "Missing required field: model", "invalid_request_error", "model_not_found")
		return
	}

	sessionID := bodySessionID(body)
	timeout := requestTimeout(estimateTokensFromBody(body))
	eps := s.endpoints(model, sessionID)
	if len(eps) == 0 {
		writeAPIError(w, 503, "No Command Code provider is configured", "server_error", "provider_not_configured")
		return
	}

	if head.Stream {
		s.stream(w, r, body, model, eps, sessionID, timeout)
		return
	}
	s.complete(w, r, body, model, eps, sessionID, timeout)
}

// stream writes Chat Completions SSE. An attempt that fails before any byte
// reached the client is retried on the next provider (that is the whole point
// of having two keys on one upstream); once output has started the stream
// belongs to the client and a restart would duplicate text, so the attempt's
// outcome is final.
func (s *CommandCodeShim) stream(w http.ResponseWriter, r *http.Request, body []byte, model string, eps []ModelEndpoint, sessionID string, timeout time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "Streaming not supported", "server_error", "streaming_unsupported")
		return
	}

	var lastErr error
	for i := range eps {
		ep := eps[i]
		if !s.gateway.router.IsAvailable(&ep) {
			continue
		}
		attemptStart := time.Now()
		partial, _, _, err := s.gateway.proxy.StreamToClient(
			r.Context(), w, flusher, body, ep, timeout, sessionID,
			StreamAttemptOptions{MaxIdleTimeout: timeout},
		)
		if err == nil {
			s.gateway.router.RecordSuccess(&ep)
			s.gateway.recordAttempt(&ep, time.Since(attemptStart), true)
			return
		}
		s.gateway.recordAttempt(&ep, time.Since(attemptStart), false)
		lastErr = err
		if partial != "" {
			// Bytes are already on the wire; the client owns this stream.
			log.Printf("[commandcode] session=%s model=%s provider=%s: stream failed after %d bytes: %v",
				sessionID, model, ep.Provider, len(partial), err)
			return
		}
		log.Printf("[commandcode] session=%s model=%s provider=%s: attempt failed before output: %v",
			sessionID, model, ep.Provider, err)
		s.penalize(&ep, err, 0, sessionID)
	}

	status, msg, errType, code := http.StatusBadGateway, "All Command Code providers failed", "server_error", "upstream_unavailable"
	if lastErr != nil {
		msg = fmt.Sprintf("All Command Code providers failed: %v", lastErr)
	}
	writeAPIError(w, status, msg, errType, code)
}

// complete serves a non-streaming request. Command Code always answers with the
// NDJSON event stream -- the envelope pins stream:true even for a client that
// asked for one object -- so the events are folded back into a single Chat
// Completions object with the same aggregator the gateway uses.
func (s *CommandCodeShim) complete(w http.ResponseWriter, r *http.Request, body []byte, model string, eps []ModelEndpoint, sessionID string, timeout time.Duration) {
	var (
		lastStatus int
		lastBody   []byte
		lastErr    error
	)
	for i := range eps {
		ep := eps[i]
		if !s.gateway.router.IsAvailable(&ep) {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		resp, err := s.gateway.proxy.Forward(ctx, body, ep, sessionID)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("[commandcode] session=%s model=%s provider=%s: forward failed: %v",
				sessionID, model, ep.Provider, err)
			s.penalize(&ep, err, 0, sessionID)
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			s.penalize(&ep, readErr, 0, sessionID)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			status, payload := resp.StatusCode, respBody
			// Remap the Command Code statuses onto the OpenAI-equivalent the
			// client and the cooldown classifier expect (402 -> 429, 5xx ->
			// retryable, unlisted -> 502).
			if mapped := ccMapError(resp.StatusCode, respBody); mapped != nil {
				status, payload = mapped.StatusCode, mapped.Body
			}
			lastStatus, lastBody = status, payload
			s.penalize(&ep, nil, resp.StatusCode, sessionID)
			continue
		}

		out, cerr := ccAggregateStream(respBody, model, time.Now().Unix())
		if cerr != nil {
			// A truncated or empty event stream is a failed attempt, not a
			// completion: forwarding half a fold would silently corrupt the
			// answer, while falling through to the next key gets a real one.
			lastErr = cerr
			log.Printf("[commandcode] session=%s model=%s provider=%s: aggregation failed: %v",
				sessionID, model, ep.Provider, cerr)
			s.penalize(&ep, cerr, 0, sessionID)
			continue
		}

		s.gateway.router.RecordSuccess(&ep)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(out); err != nil {
			log.Printf("[commandcode] session=%s model=%s: write error (client likely disconnected): %v",
				sessionID, model, err)
		}
		return
	}

	if lastStatus != 0 && len(lastBody) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(lastStatus)
		_, _ = w.Write(lastBody)
		return
	}
	msg := "All Command Code providers failed"
	if lastErr != nil {
		msg = fmt.Sprintf("All Command Code providers failed: %v", lastErr)
	}
	writeAPIError(w, http.StatusBadGateway, msg, "server_error", "upstream_unavailable")
}

// penalize applies the gateway's cooldown policy to a failed attempt: a
// transport-level error is classified by ApplyCooldownFromErrorForSession, an
// upstream status by ApplyCooldownForSession. A proxy-owned failure is never
// charged to the endpoint, mirroring the chat path.
func (s *CommandCodeShim) penalize(ep *ModelEndpoint, err error, status int, sessionID string) {
	var pErr *ProxyError
	if err != nil && errors.As(err, &pErr) {
		return
	}
	if err != nil {
		s.gateway.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
	} else {
		s.gateway.router.ApplyCooldownForSession(ep, status, "", sessionID, 0)
	}
	s.gateway.recordCooldown()
}

// HandleModels lists the Command Code models the gateway knows about: the
// endpoints of the shim's providers that appear in the configured chains. It is
// not a catalogue -- Command Code models are not enumerated anywhere in this
// repo -- but it gives a client a list to choose from. A model outside it still
// works, because the requested id is passed through verbatim.
func (s *CommandCodeShim) HandleModels(w http.ResponseWriter, r *http.Request) {
	if !s.gateway.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	cfg := s.gateway.config.Load()
	served := make(map[string]bool, len(s.providerNames))
	for _, name := range s.providerNames {
		if _, ok := cfg.Providers[name]; ok {
			served[name] = true
		}
	}

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelEntry, 0, 16)
	seen := make(map[string]bool)
	// Map iteration order is random; sort the chain names so the listing is
	// stable between calls (clients cache it).
	chainNames := make([]string, 0, len(cfg.Models))
	for chainName := range cfg.Models {
		chainNames = append(chainNames, chainName)
	}
	sort.Strings(chainNames)
	for _, chainName := range chainNames {
		for _, ep := range cfg.Models[chainName].Chain {
			if !served[ep.Provider] || seen[ep.Model] {
				continue
			}
			seen[ep.Model] = true
			data = append(data, modelEntry{ID: ep.Model, Object: "model", OwnedBy: ep.Provider})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// HandleHealth is the shim's liveness probe. It reports the providers it would
// use and whether each is currently available, so an operator can tell a
// misconfigured provider list from a cooled-down one.
func (s *CommandCodeShim) HandleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.gateway.config.Load()
	type providerHealth struct {
		Provider   string `json:"provider"`
		Configured bool   `json:"configured"`
		Available  bool   `json:"available"`
	}
	out := make([]providerHealth, 0, len(s.providerNames))
	for _, name := range s.providerNames {
		_, ok := cfg.Providers[name]
		available := false
		if ok {
			available = s.gateway.router.IsAvailable(&ModelEndpoint{
				Provider: name,
				Protocol: string(protocolCommandCode),
			})
		}
		out = append(out, providerHealth{Provider: name, Configured: ok, Available: available})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "providers": out})
}

// HandleUnsupportedResponses answers the Responses API path explicitly rather
// than 404ing: Command Code has no Responses envelope, and a client pointed at
// the shim with the wrong surface should hear why.
func (s *CommandCodeShim) HandleUnsupportedResponses(w http.ResponseWriter, r *http.Request) {
	if !s.gateway.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	writeAPIError(w, 404, "The Command Code endpoint serves /v1/chat/completions only",
		"invalid_request_error", "not_found")
}
