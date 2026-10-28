//go:build commandcode_proxy

package main

// Standalone commandcode-proxy: the Command Code protocol translation exposed
// as an OpenAI-compatible endpoint, without the four logical models or the
// dashboard. It is built with `make commandcode-proxy` and shares the router,
// proxy, cooldown store and config loader with the gateway, so both surfaces
// observe the same providers and rate limits.

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	port := flag.String("port", "3050", "Port to listen on")
	gatewayAPIKey := flag.String("api-key", "", "Gateway API key (optional, can be set via GATEWAY_API_KEY env var)")
	allowNoAuth := flag.Bool("allow-no-auth", false, "Allow running without an API key (INSECURE)")
	providers := flag.String("providers", "commandcode,commandcode2", "Command Code providers to expose, in failover order")
	flag.Parse()

	if *gatewayAPIKey == "" {
		*gatewayAPIKey = os.Getenv("GATEWAY_API_KEY")
	}
	if *gatewayAPIKey == "" && !*allowNoAuth {
		log.Fatalf("No gateway API key configured. Set GATEWAY_API_KEY or -api-key, or pass -allow-no-auth to disable authentication (not recommended).")
	}
	if *port == "" {
		log.Fatalf("No listen port configured. Pass -port.")
	}
	if strings.TrimSpace(*providers) == "" {
		log.Fatalf("No Command Code providers configured. Pass -providers.")
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Same soft heap ceiling as the gateway: this is a thin proxy, so a small
	// GOMEMLIMIT keeps the process compact on constrained hosts.
	debug.SetMemoryLimit(50 * 1024 * 1024)

	cooldownPath := filepath.Join(filepath.Dir(*configPath), "cooldowns.json")
	router := NewRouter(cfg, cooldownPath)
	proxy := NewProxy(cfg)
	proxy.SetProtocolResolver(router.ProtocolFor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Only Command Code providers are reachable through this binary, but a
	// proxy-enabled provider still needs the proxy pool, so mirror the
	// gateway's conditional service startup.
	proxySourcesPath := filepath.Join(filepath.Dir(*configPath), "proxy_sources.txt")
	proxySvc := NewProxyService(proxySourcesPath, 15*time.Minute)
	if proxyEnabled(cfg) {
		proxy.SetProxyService(proxySvc)
		proxy.UpdateRequiredReady()
		go proxySvc.Start(ctx)
		log.Printf("Proxy service enabled (pool refresh every 15 min, %d proxies must be ready)",
			proxySvc.RequiredReady())
	}

	gateway := NewGatewayContext(router, proxy, cfg, *configPath, *gatewayAPIKey, *allowNoAuth)
	router.SetMetrics(gateway.metrics)
	proxy.SetMetrics(gateway.metrics)

	// Config hot-reload: a provider added or a key rotated on disk is picked
	// up within 3 s, exactly like the gateway.
	go watchConfig(ctx, *configPath, gateway)

	shim := NewCommandCodeShim(gateway, strings.Split(*providers, ","))
	if !shim.Enabled() {
		log.Fatalf("Command Code proxy disabled: none of %q is configured in %s", *providers, *configPath)
	}

	server := &http.Server{
		Addr:              ":" + *port,
		Handler:           recoveryMiddleware(shim.Handler()),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

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

	log.Printf("Starting commandcode-proxy on port %s", *port)
	log.Printf("Config: %s", *configPath)
	log.Printf("Providers: %s", *providers)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	<-ctx.Done()
	router.Close()
	log.Println("Server stopped")
}
