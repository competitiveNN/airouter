package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"net/http/httptest"
)

// A streaming client applies a per-read timeout (maki: 30s). During a cooldown
// wait, before the first token of a large-context attempt, or whenever the
// upstream goes silent mid-generation, the gateway must keep writing SSE
// comments or the client reports "connection error" while the gateway is still
// working (measured 2026-11-07).

func TestPreOutputKeepaliveStopsOnTheFirstRealWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	ka, stop := startSSEKeepalive(rec, rec, 10*time.Millisecond)
	defer stop()
	w := keepaliveResponseWriter{ResponseWriter: rec, keepalive: ka}

	time.Sleep(35 * time.Millisecond)

	// The real write joins the keepalive goroutine, so the recorder is not
	// being written to while we read it.
	if _, err := w.Write([]byte("data: {\"ok\":true}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	body := rec.Body.String()
	if strings.Count(body, ": keepalive") == 0 {
		t.Fatal("no pre-output keepalive was written")
	}
	before := strings.Count(body, ": keepalive")
	time.Sleep(40 * time.Millisecond)
	body = rec.Body.String()
	if !strings.Contains(body, `data: {"ok":true}`) {
		t.Fatalf("real write missing: %s", body)
	}
	if got := strings.Count(body, ": keepalive"); got != before {
		t.Fatalf("keepalive continued after the first real write: %d -> %d", before, got)
	}
}

func TestIdleKeepaliveEmitsDuringSilence(t *testing.T) {
	rec := httptest.NewRecorder()
	_, stop := startSSEIdleKeepalive(rec, rec, 10*time.Millisecond)
	defer stop()

	time.Sleep(45 * time.Millisecond)
	stop() // join the ticker before reading the recorder
	if n := strings.Count(rec.Body.String(), ": keepalive"); n < 2 {
		t.Fatalf("wrote %d idle comments over 45ms at 10ms, want at least 2", n)
	}
}

func TestIdleKeepaliveStaysQuietWhileDataFlows(t *testing.T) {
	rec := httptest.NewRecorder()
	k, stop := startSSEIdleKeepalive(rec, rec, 20*time.Millisecond)
	defer stop()

	var stopWriting atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stopWriting.Load() {
			_, _ = k.Write([]byte("data: x\n\n"))
			time.Sleep(4 * time.Millisecond)
		}
	}()
	time.Sleep(60 * time.Millisecond)
	stopWriting.Store(true)
	<-done
	stop() // join the ticker before reading the recorder

	if n := strings.Count(rec.Body.String(), ": keepalive"); n != 0 {
		t.Fatalf("wrote %d comments while data was flowing every 4ms", n)
	}
}
