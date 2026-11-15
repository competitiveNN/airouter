package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestConnectionPoolWarmerHitsUpstreamEndpoints pins that the background pool
// warmer actually reaches every upstream base URL. Without this the warmer
// could silently no-op (bad config shape, wrong map walked) and the cold-start
// dial penalty it exists to remove would come back unnoticed.
func TestConnectionPoolWarmerHitsUpstreamEndpoints(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p1": {URL: upstream.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "p1", Model: "m1"},
				{Provider: "p1", Model: "m2"},
			}},
			"work": {Chain: []ModelEndpoint{
				{Provider: "p1", Model: "m3"},
			}},
		},
	}
	p := NewProxy(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartConnectionPoolWarmer(ctx, 50*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pool warmer made %d requests, want at least 2 (one per tick)", hits.Load())
}

// TestConnectionPoolWarmerStopsOnContextCancel pins that the warmer's
// goroutine does not outlive the process context: a leaked warmer would keep
// dialling upstreams after shutdown.
func TestConnectionPoolWarmerStopsOnContextCancel(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p1": {URL: upstream.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "p1", Model: "m1"}}},
		},
	}
	p := NewProxy(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	p.StartConnectionPoolWarmer(ctx, 50*time.Millisecond)

	// Wait for at least one warm before cancelling.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	// Let any in-flight warm finish, then confirm no further warms happen.
	time.Sleep(100 * time.Millisecond)
	after := hits.Load()
	time.Sleep(400 * time.Millisecond)
	if got := hits.Load(); got != after {
		t.Fatalf("warmer kept running after cancel: hits went from %d to %d", after, got)
	}
}

// TestConnectionPoolWarmerToleratesDeadUpstream pins that a dead upstream does
// not stop the warmer from serving the live ones: one broken provider must not
// take the pool warmer down with it.
func TestConnectionPoolWarmerToleratesDeadUpstream(t *testing.T) {
	var hits atomic.Int64
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	dead := releasedPortUrl(t)
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"dead": {URL: dead},
			"live": {URL: live.URL},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "dead", Model: "m-dead"},
				{Provider: "live", Model: "m-live"},
			}},
		},
	}
	p := NewProxy(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartConnectionPoolWarmer(ctx, 50*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pool warmer never reached the live upstream (hits=%d)", hits.Load())
}
