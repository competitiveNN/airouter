//go:build opencode_serve

package main

// A stripped-down opencode-serve equivalent: a headless HTTP server exposing
// /global/health, /global/event (SSE), an OpenAPI 3.1 spec, with basic auth,
// CORS, and a 20MB memory ceiling. Built with: go build -tags opencode_serve -o opencode-serve .

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

func main() {
	port := flag.String("port", "4096", "Port to listen on")
	hostname := flag.String("hostname", "127.0.0.1", "Hostname to bind")
	username := flag.String("username", "opencode", "Basic auth username")
	password := flag.String("password", "", "Basic auth password (env OPENCODE_SERVER_PASSWORD if unset)")
	cors := flag.String("cors", "", "Additional CORS origins (comma-separated)")
	flag.Parse()

	if *password == "" {
		*password = os.Getenv("OPENCODE_SERVER_PASSWORD")
	}

	debug.SetMemoryLimit(20 * 1024 * 1024)

	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", handleHealth)
	mux.HandleFunc("/global/event", handleEvent)
	mux.HandleFunc("/doc", handleOpenAPI)
	mux.HandleFunc("/", handleRoot)

	handler := authMiddleware(corsMiddleware(mux, *cors), *username, *password)

	server := &http.Server{
		Addr:              *hostname + ":" + *port,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("opencode-serve listening on http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"healthy": true,
		"version": "1.0.0",
	})
}

func handleEvent(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	spec := map[string]any{
		"openapi": "3.1.1",
		"info": map[string]any{
			"title":       "opencode api",
			"version":     "1.0.0",
			"description": "opencode-serve (Go stripped-down)",
		},
		"paths": map[string]any{
			"/global/health": map[string]any{
				"get": map[string]any{
					"summary":   "Get server health and version",
					"responses": map[string]any{"200": map[string]any{"description": "healthy"}},
				},
			},
			"/global/event": map[string]any{
				"get": map[string]any{
					"summary":   "Get global events (SSE stream)",
					"responses": map[string]any{"200": map[string]any{"description": "event stream"}},
				},
			},
		},
	}
	_ = json.NewEncoder(w).Encode(spec)
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	_, _ = fmt.Fprintf(w, "opencode-serve running on %s\n", r.Host)
}

func corsMiddleware(next http.Handler, origins string) http.Handler {
	allow := origins != ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && allow {
			w.Header().Set("Access-Control-Allow-Origin", origins)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if allow {
			w.Header().Set("Access-Control-Allow-Origin", origins)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(next http.Handler, user, pass string) http.Handler {
	protected := pass != ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !protected {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
