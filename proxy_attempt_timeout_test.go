package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// This file pins the per-attempt timeout.
//
// The gateway's fallback promise is that a request never spends more than a
// few seconds per endpoint before it moves on: the timeout is scaled to the
// request's size (requestTimeout), so a small prompt gets 5s and a large one
// gets proportionally more. That promise was only half-kept. The size-based
// timeout reached the response-header wait, but the phases BEFORE it -- the
// TCP dial and the TLS handshake -- were governed by the transport's global
// 15s/30s values. A provider that accepted the connection and then went
// silent therefore held a single attempt for 15s, whatever the request's
// budget said: the "long consecutive failure" the per-attempt budget exists
// to prevent, and it is invisible in the request's own accounting because the
// attempt looks like it was merely slow.

// TestApplyAttemptTimeoutsBoundsTheHeaderWait proves the per-attempt budget
// reaches the response-header wait, where a provider that accepts the
// connection and then says nothing used to sit for the transport's global 30s.
func TestApplyAttemptTimeoutsBoundsTheHeaderWait(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen locally: %v", err)
	}
	defer ln.Close()

	held := make(chan net.Conn, 8)
	defer func() {
		for {
			select {
			case c := <-held:
				c.Close()
			default:
				return
			}
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept the connection and never answer: only the client's own
			// budget can end the wait.
			held <- c
		}
	}()

	const timeout = 2 * time.Second
	tr := &http.Transport{}
	applyAttemptTimeouts(tr, timeout)
	client := &http.Client{Transport: tr}

	start := time.Now()
	resp, err := client.Get("http://" + ln.Addr().String())
	elapsed := time.Since(start)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the header wait to fail on the per-attempt budget")
	}
	if elapsed > 3*timeout {
		t.Fatalf("the header wait took %v, want about %v: the per-attempt budget did not reach it", elapsed, timeout)
	}
}

// TestApplyAttemptTimeoutsBoundsTheDial proves the budget reaches the TCP
// dial, the phase that comes before the header wait and that the transport's
// global 15s dial timeout used to decide.
func TestApplyAttemptTimeoutsBoundsTheDial(t *testing.T) {
	const timeout = 2 * time.Second

	// An outer deadline the test itself controls, so a regression here fails
	// with a readable message rather than hanging the suite: without the
	// wrapper's own deadline the dial below would never return at all.
	outer, outerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer outerCancel()

	called := make(chan struct{}, 1)
	tr := &http.Transport{}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		called <- struct{}{}
		// A dial that never completes on its own: only the deadline the
		// wrapper adds can end it.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	applyAttemptTimeouts(tr, timeout)

	start := time.Now()
	_, err := tr.DialContext(outer, "tcp", "203.0.113.1:443")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the dial to fail when the per-attempt budget runs out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
	if elapsed > 2*timeout {
		t.Fatalf("the dial took %v, want about %v: the budget did not reach the dial", elapsed, timeout)
	}
	if elapsed < timeout/2 {
		t.Fatalf("the dial gave up after %v, before its %v budget: the deadline was never applied", elapsed, timeout)
	}
	// The original dialer must be WRAPPED, not replaced: a SOCKS5 proxy
	// transport carries its own dialer, and overwriting it would silently
	// turn a proxied request into a direct one.
	select {
	case <-called:
	default:
		t.Fatal("the original dialer was replaced instead of wrapped")
	}
}

// TestApplyAttemptTimeoutsInstallsADialerWhenThereIsNone proves an attempt
// never falls back to the transport's own (unbounded-by-request) dial
// behaviour.
func TestApplyAttemptTimeoutsInstallsADialerWhenThereIsNone(t *testing.T) {
	const timeout = time.Second
	tr := &http.Transport{}
	applyAttemptTimeouts(tr, timeout)

	if tr.DialContext == nil {
		t.Fatal("expected a dialer to be installed for the attempt")
	}
	if tr.ResponseHeaderTimeout != timeout {
		t.Fatalf("header timeout is %v, want %v", tr.ResponseHeaderTimeout, timeout)
	}
	if tr.TLSHandshakeTimeout != timeout {
		t.Fatalf("TLS handshake timeout is %v, want %v", tr.TLSHandshakeTimeout, timeout)
	}
}

// TestApplyAttemptTimeoutsIgnoresANonPositiveTimeout proves a caller with no
// budget to apply (the warm-up probe) leaves the transport alone instead of
// zeroing its timeouts, which would mean "no timeout" rather than "no change".
func TestApplyAttemptTimeoutsIgnoresANonPositiveTimeout(t *testing.T) {
	tr := &http.Transport{ResponseHeaderTimeout: 30 * time.Second}
	applyAttemptTimeouts(tr, 0)

	if tr.ResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("header timeout changed to %v: a zero timeout must mean 'leave it alone'", tr.ResponseHeaderTimeout)
	}
	if tr.TLSHandshakeTimeout != 0 {
		t.Fatalf("TLS handshake timeout changed to %v: a zero timeout must mean 'leave it alone'", tr.TLSHandshakeTimeout)
	}
}

// TestAttemptClientBoundsTheAttemptWithoutMutatingTheSharedTransport proves
// the shared transport is never mutated: its timeouts are global, so a
// per-attempt budget applied to it would change every other in-flight
// request. The attempt gets its own transport instead.
func TestAttemptClientBoundsTheAttemptWithoutMutatingTheSharedTransport(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"p": {URL: "https://example.com/v1", APIKeyEnv: "K"},
		},
	}
	p := NewProxy(cfg)
	shared := p.baseTransport
	before := shared.ResponseHeaderTimeout

	const timeout = 3 * time.Second
	ep := ModelEndpoint{Provider: "p", Model: "m"}
	client, usedProxy := p.attemptClient(&ep, timeout)
	if usedProxy != nil {
		t.Fatal("expected a direct attempt: the provider configures no proxy")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport, got %T", client.Transport)
	}
	if tr == shared {
		t.Fatal("expected the attempt to get its own transport")
	}
	if tr.ResponseHeaderTimeout != timeout {
		t.Fatalf("header timeout is %v, want %v", tr.ResponseHeaderTimeout, timeout)
	}
	if tr.TLSHandshakeTimeout != timeout {
		t.Fatalf("TLS handshake timeout is %v, want %v", tr.TLSHandshakeTimeout, timeout)
	}
	if shared.ResponseHeaderTimeout != before {
		t.Fatalf("the shared transport was mutated: %v -> %v", before, shared.ResponseHeaderTimeout)
	}
}
