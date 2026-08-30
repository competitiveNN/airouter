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

	cooldownPath := filepath.Join(filepath.Dir(*configPath), "cooldowns.json")
	router := NewRouter(cfg, cooldownPath)
	proxy := NewProxy(cfg)
	gateway := NewGatewayContext(router, proxy, cfg, *configPath, *gatewayAPIKey)

	ctx, cancel := context.WithCancel(context.Background())

	// Hot-reload the config when the file changes on disk so fallback chains
	// and providers can be updated without restarting the server.
	go watchConfig(ctx, *configPath, gateway)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", gateway.HandleModels)
	mux.HandleFunc("/v1/chat/completions", gateway.HandleChatCompletions)
	mux.HandleFunc("/health", gateway.HandleHealth)
	mux.HandleFunc("/admin/providers", gateway.HandleAdminProviders)
	mux.HandleFunc("/admin/sessions", gateway.HandleAdminSessions)
	mux.HandleFunc("/admin/cooldowns", gateway.HandleAdminCooldowns)
	mux.HandleFunc("/admin/config", gateway.HandleAdminConfig)
	mux.HandleFunc("/admin/", gateway.HandleAdminDashboard)
	mux.HandleFunc("/admin/dashboard", gateway.HandleAdminDashboard)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	server := &http.Server{
		Addr:         ":" + *port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
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
	log.Println("Server stopped")
}

// watchConfig polls the config file and reloads it in memory whenever its
// mtime/size changes, so fallback chains and providers can be updated without a
// restart. Invalid configs are logged and skipped (the last good config stays
// active). The reload swaps the config pointers atomically; sessions and
// cooldowns are preserved. It exits when ctx is cancelled (graceful shutdown).
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
			lastMod, lastSize = fi.ModTime().UnixNano(), fi.Size()
			cfg, err := LoadConfig(path)
			if err != nil {
				log.Printf("[debug] hot reload skipped: config parse error: %v", err)
				continue
			}
			gateway.ReloadConfig(cfg)
			log.Printf("Config hot-reloaded from %s", path)
		}
	}
}
