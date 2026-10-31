package main

// Shared server plumbing used by both binaries: the airouter gateway and the
// standalone commandcode-proxy. It lives here rather than in main.go because
// main.go is excluded from the commandcode_proxy build (two main functions
// cannot coexist in one package), while these helpers must be present in
// both.

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

// trackingResponseWriter records whether a response has already been started
// so the panic recovery middleware can avoid clobbering a response that was
// already being streamed to the client. Writing a 500 on top of a header that
// was already sent corrupts the response instead of failing cleanly.
//
// It MUST forward the OPTIONAL interfaces of the underlying writer. Embedding
// the http.ResponseWriter INTERFACE promotes only that interface's own
// methods (Header, Write, WriteHeader), so http.Flusher and http.Hijacker are
// silently dropped. The handlers assert for them by type switch -- api.go and
// responses_api.go need http.Flusher for SSE, and the websocket upgrade needs
// http.Hijacker -- so a wrapper that omitted them made every streaming
// request fail with "Streaming not supported" (a 500) and every websocket
// upgrade fail. Forwarding them explicitly is what keeps this wrapper
// transparent.
type trackingResponseWriter struct {
	http.ResponseWriter
	started bool
}

// WriteHeader records that the response has started.
func (tw *trackingResponseWriter) WriteHeader(code int) {
	tw.started = true
	tw.ResponseWriter.WriteHeader(code)
}

// Write records that the response has started: net/http sends an implicit
// 200 when a handler writes without calling WriteHeader, so the panic
// recovery must not follow it with a 500.
func (tw *trackingResponseWriter) Write(p []byte) (int, error) {
	tw.started = true
	return tw.ResponseWriter.Write(p)
}

// Flush forwards to the underlying writer when it supports flushing. The SSE
// paths assert w.(http.Flusher) and fail the request with a 500 when the
// assertion fails, so this method must exist.
func (tw *trackingResponseWriter) Flush() {
	if f, ok := tw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer when it supports hijacking. The
// websocket upgrade path asserts w.(http.Hijacker) through gorilla/websocket,
// so this method must exist.
func (tw *trackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := tw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("airouter: underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

// Unwrap exposes the original writer to http.ResponseController.
func (tw *trackingResponseWriter) Unwrap() http.ResponseWriter {
	return tw.ResponseWriter
}

// recoveryMiddleware wraps handlers to catch panics so a single bad request
// can't take down the entire server.
//
// The wrapper must stay transparent to the handler: see
// trackingResponseWriter for why http.Flusher and http.Hijacker have to be
// forwarded. A panic that happens AFTER the handler started a response leaves
// that response alone (a superfluous 500 would corrupt it); a panic before
// any output gets a clean 500.
func recoveryMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingResponseWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, rec)
				if !tw.started {
					writeAPIError(w, http.StatusServiceUnavailable, "Internal server error", "service_unavailable_error", "internal_error")
				}
			}
		}()
		h.ServeHTTP(tw, r)
	})
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
