//go:build !commandcode_proxy && !airouter_proxy && !opencode_serve

package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
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

	// Cap the process heap at 20MB. The gateway is a thin proxy that
	// buffers at most one attempt body at a time, so this is a soft
	// ceiling: it pressures the GC to keep long-lived objects collected
	// rather than letting heap usage drift upward across long-running
	// sessions. (debug.SetMemoryLimit sets GOMEMLIMIT-equivalent; it is
	// advisory — the GC tries to stay under it — but keeps the process
	// small for operator deployments on constrained hosts.)
	debug.SetMemoryLimit(20 * 1024 * 1024)

	cooldownPath := filepath.Join(filepath.Dir(*configPath), defaultCooldownDBName)
	router := NewRouter(cfg, cooldownPath)
	proxy := NewProxy(cfg)
	proxy.SetProtocolResolver(router.ProtocolFor)

	ctx, cancel := context.WithCancel(context.Background())

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
	mux.HandleFunc("/health", gateway.HandleHealth)

	// recoveryMiddleware wraps handlers to catch panics so a single bad
	// request can't take down the entire server.
	server := &http.Server{
		Addr:              ":" + *port,
		Handler:           recoveryMiddleware(mux),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
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

	log.Printf("Starting minimal airouter on port %s", *port)
	log.Printf("Config: %s", *configPath)
	log.Printf("Models: %v", LogicalModels)
	log.Printf("Tool calls (coalesced): %v", *toolCalls)

	if *warmup {
		log.Println("Warming up model profiles in the background...")
		go proxy.Warmup(ctx)
	}
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
	gateway.Close()
	router.Close()
	log.Println("Server stopped")
}

func flagBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}
