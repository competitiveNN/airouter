package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	port := flag.String("port", "8080", "Port to listen on")
	gatewayAPIKey := flag.String("api-key", "", "Gateway API key (optional, can be set via GATEWAY_API_KEY env var)")
	allowNoAuth := flag.Bool("allow-no-auth", false, "Allow running without an API key (INSECURE: all endpoints including admin become unauthenticated)")
	warmup := flag.Bool("warmup", false, "Warm up model profiles on startup (fires test requests to each provider)")
	toolCalls := flag.Bool("tool-calls", false, "Enable synthesized tool-call events in streaming responses (disabled by default for compatibility)")
	pruneStats := flag.String("prune-stats", "", "Prune the dashboard stats journal at PATH to the 30-day retention window, then exit")
	flag.Parse()

	// The prune mode is a maintenance operation, not a gateway: it
	// rewrites one file and exits before anything else starts, so the
	// systemd timer that runs it needs no API key and no config.
	if *pruneStats != "" {
		kept, err := pruneDashboardJournal(*pruneStats)
		if err != nil {
			log.Fatalf("Failed to prune stats journal %s: %v", *pruneStats, err)
		}
		log.Printf("Pruned stats journal %s: %d ticks kept", *pruneStats, kept)
		return
	}

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

	cooldownPath := filepath.Join(filepath.Dir(*configPath), "cooldowns.json")
	// The dashboard journal lives beside the config for the same
	// reason cooldowns.json does: it is state the gateway owns, and
	// the config's directory is the one place the operator already
	// associates with the gateway's data.
	journalPath := filepath.Join(filepath.Dir(*configPath), "metrics-ring.jsonl")
	router := NewRouter(cfg, cooldownPath)
	proxy := NewProxy(cfg)
	proxy.SetProtocolResolver(router.ProtocolFor)

	ctx, cancel := context.WithCancel(context.Background())

	// Proxy service: fetches proxy lists from proxy_sources.txt every 15 min,
	// geolocates each proxy, and maintains a priority-sorted pool (EU/NA
	// preferred, all others as backup). It only runs when at least one
	// provider or endpoint has proxying enabled; otherwise the periodic
	// fetching is unnecessary overhead.
	// The built-in source list lives in proxysources.go; proxy_sources.txt is
	// an optional file that can add further sources (see docs/proxy_sources.txt
	// for the reference copy of the built-in list).
	proxySvc := NewProxyService("proxy_sources.txt", 15*time.Minute)
	if proxyEnabled(cfg) {
		proxy.SetProxyService(proxySvc)
		// Seed the ready-pool requirement before the loop starts so the first
		// top-up already knows how many proxies must be proven reachable.
		proxy.UpdateRequiredReady()
		go proxySvc.Start(ctx)
		log.Printf("Proxy service enabled (pool refresh every 15 min, %d proxies must be ready)",
			proxySvc.RequiredReady())
	}

	gateway := NewGatewayContext(router, proxy, cfg, *configPath, *gatewayAPIKey, *allowNoAuth)
	router.SetMetrics(gateway.metrics)
	proxy.SetMetrics(gateway.metrics)
	proxy.SetToolCalls(*toolCalls)

	// Hot-reload the config when the file changes on disk so fallback chains
	// and providers can be updated without restarting the server.
	go watchConfig(ctx, *configPath, gateway)

	mux := http.NewServeMux()
	mux.HandleFunc("/chat", gateway.HandleChatPage)
	mux.HandleFunc("/chat/completions", gateway.HandleWebChatCompletions)
	mux.HandleFunc("/chat/responses", gateway.HandleWebChatResponses)
	mux.HandleFunc("/chat/models", gateway.HandleWebChatModels)
	mux.HandleFunc("/v1/models", gateway.HandleModels)
	mux.HandleFunc("/v1/chat/completions", gateway.HandleChatCompletions)
	mux.HandleFunc("/v1/responses", gateway.HandleResponses)
	mux.HandleFunc("/v1/responses/ws", gateway.HandleResponsesWebSocket)
	mux.HandleFunc("/health", gateway.HandleHealth)
	mux.HandleFunc("/metrics", gateway.HandleMetrics)
	mux.HandleFunc("/v1/airouter/state", gateway.HandleAirouterState)
	mux.HandleFunc("/admin/providers", gateway.HandleAdminProviders)
	mux.HandleFunc("/admin/sessions", gateway.HandleAdminSessions)
	mux.HandleFunc("/admin/cooldowns", gateway.HandleAdminCooldowns)
	mux.HandleFunc("/admin/config", gateway.HandleAdminConfig)
	mux.HandleFunc("/admin/dashboard/data", gateway.HandleAdminDashboardData)
	mux.HandleFunc("/admin/", gateway.HandleAdminDashboard)
	mux.HandleFunc("/admin/dashboard", gateway.HandleAdminDashboard)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	// recoveryMiddleware wraps handlers to catch panics so a single bad
	// request can't take down the entire server.
	recoveryMiddleware := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, rec)
					writeAPIError(w, 500, "Internal server error", "server_error", "internal_error")
				}
			}()
			h.ServeHTTP(w, r)
		})
	}

	server := &http.Server{
		Addr:              ":" + *port,
		Handler:           recoveryMiddleware(mux),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("Starting airouter on port %s", *port)
	log.Printf("Config: %s", *configPath)
	log.Printf("Models: %v", LogicalModels)
	log.Printf("Tool calls (coalesced): %v", *toolCalls)

	if *warmup {
		log.Println("Warming up model profiles in the background...")
		go proxy.Warmup(ctx)
	}

	// Start the dashboard attempt ring so the 1d/7d leaderboards are live
	// from startup rather than only after the first dashboard scrape.
	stopDashboard := gateway.metrics.StartDashboardTelemetry(journalPath)
	defer stopDashboard()
	for name, p := range cfg.Providers {
		keyStatus := "no key"
		if p.APIKey() != "" {
			keyStatus = "configured"
		}
		log.Printf("Provider: %s (%s) - %s", name, p.URL, keyStatus)
	}
	for name, m := range cfg.Models {
		log.Printf("Model: %s -> chain length %d", name, len(m.Chain))
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	<-ctx.Done()
	router.Close()
	log.Println("Server stopped")
}

func flagBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

func watchConfig(ctx context.Context, path string, gateway *GatewayContext) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var lastMod int64
	var lastSize int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			if fi.ModTime().UnixNano() == lastMod && fi.Size() == lastSize {
				continue
			}
			newMod, newSize := fi.ModTime().UnixNano(), fi.Size()
			cfg, err := LoadConfig(path)
			if err != nil {
				log.Printf("[debug] hot reload skipped: config parse error: %v", err)
				// Don't update lastMod/lastSize — retry on next tick so a
				// transient write (e.g. editor saving) doesn't get stuck.
				continue
			}
			lastMod, lastSize = newMod, newSize
			gateway.ReloadConfig(cfg)
			log.Printf("Config hot-reloaded from %s", path)
		}
	}
}
