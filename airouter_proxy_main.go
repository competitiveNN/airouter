//go:build airouter_proxy

package main

// Standalone provider proxy.
//
// This binary exposes every provider defined in config.yaml as a flat,
// prefixed model namespace on one OpenAI-compatible endpoint. Each model id
// is "<provider prefix>/<upstream model>", e.g.
//
//	opencode1/moonshotai/kimi-k3.1-pro
//	opencode2/anthropic/claude-sonnet-4
//	nvidia1/deepseek-ai/deepseek-v3
//
// It deliberately performs no grouping into the four logical profiles and no
// cross-provider fallback: a request names one provider and one model, and is
// forwarded to that provider. Everything else (API-key injection, provider
// headers, per-provider timeouts, and especially mandatory proxy routing) is
// shared with the gateway through Proxy, so this surface observes the same
// config and the same proxy pool as the main daemon.
//
// Build with: go build -tags airouter_proxy -o airouter-proxy .

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// providerServer owns the request handlers for the standalone surface.
type providerServer struct {
	gateway *GatewayContext
}

// splitTrailingNumber splits a provider name into its base and a positive
// trailing number ("nvidia3" -> "nvidia", 3). Names without a trailing number
// (or whose suffix is not a positive integer) keep their full name and report
// 0, which callers treat as "bare" (unnumbered) member of its family.
func splitTrailingNumber(name string) (string, int) {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	if i == len(name) || i == 0 {
		return name, 0
	}
	n, err := strconv.Atoi(name[i:])
	if err != nil || n <= 0 {
		return name, 0
	}
	return name[:i], n
}

// providerPrefixes maps each configured provider name to the prefix used in
// model ids.
//
// Providers that are the only member of their family keep their name
// (ollama, kilocode). When a family has numbered members, the bare provider
// takes the lowest free number, so the config's
//
//	opencode, opencode2, opencode3, opencode4
//
// becomes
//
//	opencode1, opencode2, opencode3, opencode4
//
// exactly as the prefixed-model contract promises. The result is deterministic:
// names are sorted before assignment, so a map iteration order cannot shuffle
// the numbering between startups.
func providerPrefixes(cfg *Config) map[string]string {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	type family struct {
		bare     []string
		numbered map[int]string
	}
	families := make(map[string]*family)
	order := make([]string, 0, len(names))
	for _, name := range names {
		base, num := splitTrailingNumber(name)
		f, ok := families[base]
		if !ok {
			f = &family{numbered: map[int]string{}}
			families[base] = f
			order = append(order, base)
		}
		if num > 0 {
			f.numbered[num] = name
		} else {
			f.bare = append(f.bare, name)
		}
	}

	out := make(map[string]string, len(names))
	for _, base := range order {
		f := families[base]
		if len(f.numbered) == 0 {
			// A singleton family: its own name is unambiguous.
			for _, name := range f.bare {
				out[name] = name
			}
			continue
		}
		used := make(map[int]bool, len(f.numbered))
		for num := range f.numbered {
			used[num] = true
		}
		next := 1
		for _, name := range f.bare {
			for used[next] {
				next++
			}
			out[name] = base + strconv.Itoa(next)
			used[next] = true
		}
		for _, name := range f.numbered {
			out[name] = name
		}
	}
	return out
}

// modelIDsForProvider returns the unique model ids configured for provider,
// sorted so /v1/models is stable across restarts. The endpoints are collected
// from every chain because the config does not carry a separate per-provider
// catalog.
func modelIDsForProvider(cfg *Config, provider string) []ModelEndpoint {
	seen := make(map[string]ModelEndpoint)
	for _, mc := range cfg.Models {
		for _, ep := range mc.Chain {
			if ep.Provider != provider {
				continue
			}
			if _, ok := seen[ep.Key()]; ok {
				continue
			}
			seen[ep.Key()] = ep
		}
	}
	eps := make([]ModelEndpoint, 0, len(seen))
	for _, ep := range seen {
		eps = append(eps, ep)
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].Model < eps[j].Model })
	return eps
}

// lookupEndpoint finds the configured endpoint for provider/model so the
// request inherits its protocol (responses vs chat), vision flag and context
// window. A model that is not in any chain is still proxied, using the
// provider's default chat protocol.
func lookupEndpoint(cfg *Config, provider, model string) (ModelEndpoint, bool) {
	for _, mc := range cfg.Models {
		for _, ep := range mc.Chain {
			if ep.Provider == provider && ep.Model == model {
				return ep, true
			}
		}
	}
	return ModelEndpoint{}, false
}

// resolveModel splits a client model id ("opencode1/deepseek/v3") into the
// provider's config name and the upstream model id. The first path segment is
// the prefix exactly once; the remainder (which may itself contain slashes) is
// the upstream model.
func (s *providerServer) resolveModel(cfg *Config, id string) (provider, upstream string, ok bool) {
	if id == "" {
		return "", "", false
	}
	prefixes := providerPrefixes(cfg)
	prefix, upstream, found := strings.Cut(id, "/")
	if !found || prefix == "" || upstream == "" {
		return "", "", false
	}
	for name, p := range prefixes {
		if p == prefix {
			return name, upstream, true
		}
	}
	return "", "", false
}

func (s *providerServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleModels advertises the prefixed model namespace. Context windows are
// the endpoint's own advertised window (there is no chain to take a minimum
// over), so the value is exact for the single backend that will serve it.
func (s *providerServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.gateway.checkAuth(r) {
		writeAPIError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	cfg := s.gateway.config.Load()
	if cfg == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "No configuration loaded", "server_error", "no_config")
		return
	}
	prefixes := providerPrefixes(cfg)
	names := make([]string, 0, len(prefixes))
	for name := range prefixes {
		names = append(names, name)
	}
	sort.Strings(names)

	now := time.Now().Unix()
	models := make([]Model, 0, len(names)*4)
	for _, name := range names {
		prefix := prefixes[name]
		for _, ep := range modelIDsForProvider(cfg, name) {
			m := Model{
				ID:      prefix + "/" + ep.Model,
				Object:  "model",
				Created: now,
				OwnedBy: name,
			}
			if ctx := ep.ContextWindow(); ctx > 0 {
				m.MaxContextTokens = &ctx
			}
			models = append(models, m)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ModelListResponse{Object: "list", Data: models})
}

// handleChatCompletions forwards one chat completion to the provider named by
// the model prefix. Streaming bodies are relayed as SSE; non-streaming bodies
// are buffered by Proxy.Forward.
func (s *providerServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.gateway.checkAuth(r) {
		writeAPIError(w, http.StatusUnauthorized, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLargeRequestBytes)
	body, err := readRequestBody(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error", "bad_request")
		return
	}

	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		writeAPIError(w, http.StatusBadRequest, "Invalid JSON", "invalid_request_error", "bad_json")
		return
	}
	if head.Model == "" {
		writeAPIError(w, http.StatusBadRequest, "model parameter required", "invalid_request_error", "missing_model")
		return
	}

	cfg := s.gateway.config.Load()
	if cfg == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "No configuration loaded", "server_error", "no_config")
		return
	}
	provider, upstreamModel, ok := s.resolveModel(cfg, head.Model)
	if !ok {
		writeAPIError(w, http.StatusBadRequest,
			fmt.Sprintf("Unknown model: %s. Model ids are prefixed with a provider (see /v1/models).", head.Model),
			"invalid_request_error", "model_not_found")
		return
	}

	endpoint, found := lookupEndpoint(cfg, provider, upstreamModel)
	if !found {
		endpoint = ModelEndpoint{Provider: provider, Model: upstreamModel}
	}
	sessionID := bodySessionID(body)

	if head.Stream {
		s.streamChat(w, r, body, endpoint, sessionID)
		return
	}
	s.forwardChat(w, r, body, endpoint, sessionID)
}

// retryableProxyStatus reports whether an upstream HTTP status should cause a
// retry on the next API key of a multi-key provider. Rate limits, auth
// failures and server errors are key-specific or transient; other 4xx are
// request-level and won't improve by switching keys.
func retryableProxyStatus(code int) bool {
	if code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500
}

func providerAPIKeyCount(cfg *Config, provider string) int {
	if cfg != nil {
		if pc, ok := cfg.Providers[provider]; ok {
			if n := pc.NumAPIKeys(); n > 0 {
				return n
			}
		}
	}
	return 1
}

func (s *providerServer) forwardChat(w http.ResponseWriter, r *http.Request, body []byte, endpoint ModelEndpoint, sessionID string) {
	// Proxy.Forward applies the provider's configured timeout itself; the
	// outer deadline only prevents an unbounded hang when the provider has no
	// timeout of its own.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	cfg := s.gateway.config.Load()
	numKeys := providerAPIKeyCount(cfg, endpoint.Provider)

	// Rotate the starting key by session so load spreads across keys
	// (round-robin) instead of always hammering key 0 first.
	start := 0
	if sessionID != "" && numKeys > 1 {
		h := 0
		for _, c := range sessionID {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
		start = h % numKeys
	}

	var lastErr error
	var lastStatus int
	var lastHeaders http.Header
	var lastBody []byte

	for i := 0; i < numKeys; i++ {
		idx := (start + i) % numKeys
		ep := endpoint
		ep.APIKeyIndex = idx

		resp, release, err := s.gateway.proxy.Forward(ctx, body, ep, sessionID)
		if err != nil {
			lastErr = err
			var pErr *ProxyError
			var provErr *ProviderError
			proxyTag := "direct"
			if errors.As(err, &pErr) {
				if url := pErr.ProxyURL(); url != "" {
					proxyTag = "proxied via " + url
					s.gateway.proxy.MarkProxyDown(url, proxyCooldown)
				}
			} else if errors.As(err, &provErr) {
				if url := provErr.ProxyURL(); url != "" {
					proxyTag = "proxied via " + url
				}
			}
			log.Printf("[airouter-proxy] %s/%s (key %d) forward failed (%s): %v", endpoint.Provider, endpoint.Model, idx, proxyTag, err)
			continue
		}

		// Read the body up front so we can either forward it (success or a
		// non-retryable client error) or retry on the next key.
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBodyBytes+1))
		resp.Body.Close()
		release()

		if readErr != nil {
			lastErr = readErr
			lastStatus = http.StatusBadGateway
			log.Printf("[airouter-proxy] %s/%s (key %d) response read failed: %v", endpoint.Provider, endpoint.Model, idx, readErr)
			continue
		}

		if resp.StatusCode < 400 || !retryableProxyStatus(resp.StatusCode) {
			// Success, or a non-retryable client error: forward verbatim.
			for k, v := range resp.Header {
				if isHopByHopHeader(k) {
					continue
				}
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			if _, err := w.Write(raw); err != nil {
				log.Printf("[airouter-proxy] %s/%s response write failed: %v", endpoint.Provider, endpoint.Model, err)
			}
			return
		}

		// Retryable HTTP failure: remember it in case every key fails, then
		// try the next key.
		lastStatus = resp.StatusCode
		lastHeaders = resp.Header.Clone()
		lastBody = raw
		log.Printf("[airouter-proxy] %s/%s (key %d) returned %d, retrying next key", endpoint.Provider, endpoint.Model, idx, resp.StatusCode)
	}

	// All keys exhausted.
	if lastStatus != 0 {
		for k, v := range lastHeaders {
			if isHopByHopHeader(k) {
				continue
			}
			w.Header()[k] = v
		}
		w.WriteHeader(lastStatus)
		if _, err := w.Write(lastBody); err != nil {
			log.Printf("[airouter-proxy] %s/%s response write failed: %v", endpoint.Provider, endpoint.Model, err)
		}
		return
	}
	if lastErr != nil {
		log.Printf("[airouter-proxy] %s/%s all keys failed: %v", endpoint.Provider, endpoint.Model, lastErr)
	}
	writeAPIError(w, http.StatusBadGateway, "Upstream error", "upstream_error", "upstream_unavailable")
}

// trackWriter counts bytes that reach the client so a streaming attempt that
// failed before its first write can still return a real HTTP error instead of
// a committed 200 with an empty body.
type trackWriter struct {
	w io.Writer
	n int64
}

func (t *trackWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	// Keepalive comments (": keepalive\n\n") must not count as committed
	// client-visible output: they carry no content, so an attempt that only
	// emitted keepalives before timing out can still be retried on the next
	// API key without duplicating output.
	if string(p) != ": keepalive\n\n" {
		t.n += int64(n)
	}
	return n, err
}

func (s *providerServer) streamChat(w http.ResponseWriter, r *http.Request, body []byte, endpoint ModelEndpoint, sessionID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable, "Streaming not supported", "server_error", "streaming_unsupported")
		return
	}

	tw := &trackWriter{w: w}

	cfg := s.gateway.config.Load()
	numKeys := providerAPIKeyCount(cfg, endpoint.Provider)

	start := 0
	if sessionID != "" && numKeys > 1 {
		h := 0
		for _, c := range sessionID {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
		start = h % numKeys
	}

	for i := 0; i < numKeys; i++ {
		idx := (start + i) % numKeys
		ep := endpoint
		ep.APIKeyIndex = idx
		opts := StreamAttemptOptions{KeepaliveInterval: prefillKeepalive}
		_, _, _, err := s.gateway.proxy.StreamToClient(r.Context(), tw, flusher, body, ep, 0, sessionID, opts)
		if err == nil {
			return
		}
		var pErr *ProxyError
		var provErr *ProviderError
		proxyTag := "direct"
		if errors.As(err, &pErr) {
			if url := pErr.ProxyURL(); url != "" {
				proxyTag = "proxied via " + url
				s.gateway.proxy.MarkProxyDown(url, proxyCooldown)
			}
		} else if errors.As(err, &provErr) {
			if url := provErr.ProxyURL(); url != "" {
				proxyTag = "proxied via " + url
			}
		}
		log.Printf("[airouter-proxy] %s/%s (key %d) stream failed (%s): %v", endpoint.Provider, endpoint.Model, idx, proxyTag, err)
		// If bytes already reached the client we cannot transparently retry
		// on another key without duplicating output; stop here.
		if tw.n > 0 {
			break
		}
	}
	if tw.n == 0 {
		// Nothing has been committed to the wire yet: return a normal error
		// response, clearing the SSE headers set above.
		w.Header().Del("Content-Type")
		w.Header().Del("Cache-Control")
		w.Header().Del("Connection")
		writeAPIError(w, http.StatusBadGateway, "Upstream error", "upstream_error", "upstream_unavailable")
		return
	}
	// Output already reached the client. Keep the stream well-formed: emit a
	// single error event and the terminating sentinel so the client does not
	// hang waiting for more.
	_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":\"Upstream error\",\"type\":\"upstream_error\"}}\n\n")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	port := flag.String("port", "9091", "Port to listen on")
	gatewayAPIKey := flag.String("api-key", "", "Gateway API key (optional, can be set via GATEWAY_API_KEY env var)")
	allowNoAuth := flag.Bool("allow-no-auth", false, "Allow running without an API key (INSECURE)")
	flag.Parse()

	if *gatewayAPIKey == "" {
		*gatewayAPIKey = os.Getenv("GATEWAY_API_KEY")
	}
	if *gatewayAPIKey == "" && !*allowNoAuth {
		log.Fatalf("No gateway API key configured. Set GATEWAY_API_KEY or -api-key, or pass -allow-no-auth to disable authentication (not recommended).")
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	debug.SetMemoryLimit(50 * 1024 * 1024)

	// No cooldowns.db for this surface: it is a direct proxy, not a fallback
	// router, so cooldown state would be written but never consulted. An empty
	// path keeps the router entirely in memory.
	router := NewRouter(cfg, "")
	proxy := NewProxy(cfg)
	proxy.SetProtocolResolver(router.ProtocolFor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mandatory proxy routing is a provider property, so the pool must run
	// whenever any provider needs it (opencode2/3/4 in the shipped config).
	proxySourcesPath := filepath.Join(filepath.Dir(*configPath), "proxy_sources.txt")
	proxySvc := NewProxyService(proxySourcesPath, 15*time.Minute)
	if ti := cfg.Preferences.TorInstancesValue(); ti > 0 {
		proxySvc.EnableTor(ti)
	}
	if proxyEnabled(cfg) || proxySvc.TorMode() {
		proxy.SetProxyService(proxySvc)
		proxy.UpdateRequiredReady()
		go proxySvc.Start(ctx)
		defer proxySvc.Stop()
		if proxySvc.TorMode() {
			log.Printf("Proxy service enabled (tor mode, %d instances)", proxySvc.TorInstances())
		} else {
			log.Printf("Proxy service enabled (%d proxies must be ready)", proxySvc.RequiredReady())
		}
	}

	gateway := NewGatewayContextWithProxy(router, proxy, cfg, *configPath, *gatewayAPIKey, true, *allowNoAuth)
	router.SetMetrics(gateway.metrics)
	proxy.SetMetrics(gateway.metrics)
	defer gateway.Close()
	defer router.Close()

	go watchConfig(ctx, *configPath, gateway)

	ps := &providerServer{gateway: gateway}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", ps.handleHealth)
	mux.HandleFunc("/v1/models", ps.handleModels)
	mux.HandleFunc("/models", ps.handleModels)
	mux.HandleFunc("/v1/chat/completions", ps.handleChatCompletions)
	mux.HandleFunc("/metrics", gateway.HandleMetrics)
	mux.HandleFunc("/admin/dashboard", gateway.HandleAdminDashboard)
	mux.HandleFunc("/admin/dashboard/data", gateway.HandleAdminDashboardData)
	mux.HandleFunc("/admin/", gateway.HandleAdminDashboard)

	server := &http.Server{
		Addr:              ":" + *port,
		Handler:           recoveryMiddleware(mux),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      15 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Provider proxy error: %v", err)
		}
	}()

	prefixes := providerPrefixes(cfg)
	prefixed := make([]string, 0, len(prefixes))
	for name, p := range prefixes {
		prefixed = append(prefixed, fmt.Sprintf("%s -> %s/", name, p))
	}
	sort.Strings(prefixed)
	log.Printf("Provider proxy listening on :%s", *port)
	for _, line := range prefixed {
		log.Printf("  %s", line)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}
