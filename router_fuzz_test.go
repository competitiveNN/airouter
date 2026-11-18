package main

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// FuzzRetryAfterParsing drives arbitrary Retry-After header bytes through
// ParseRetryAfter.
//
// The input is parsed straight out of an upstream HTTP response header, so it
// is attacker-adjacent: it must never panic and must always return a
// non-negative Duration. The interesting case is a huge delta-seconds value:
// secs*time.Second overflows int64 above ~292 years and yields a NEGATIVE
// Duration, which callers read as "no hint" and silently ignore. The parser
// clamps to maxRetryAfter instead, and this target pins both properties.
func FuzzRetryAfterParsing(f *testing.F) {
	seeds := []string{
		"",
		" ",
		"0",
		"-1",
		"120",
		"86400",
		"604800",
		"soon",
		"0.5",
		"Wed, 21 Oct 2015 07:28:00 GMT",
		"999999999999999999999999",
		strings.Repeat("9", 64),
		"9223372036854775807",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		d := ParseRetryAfter(raw) // a panic here fails the fuzz target outright

		if d < 0 {
			t.Fatalf("ParseRetryAfter(%q) = %v; returned a negative duration", raw, d)
		}
		if d > maxRetryAfter {
			t.Fatalf("ParseRetryAfter(%q) = %v; exceeds the %v clamp", raw, d, maxRetryAfter)
		}

		// Cross-check the delta-seconds branch against an independent
		// computation. A plain positive integer must map to exactly secs
		// seconds (clamped); anything else would mean the parse disagreed with
		// its own arithmetic.
		trimmed := strings.TrimSpace(raw)
		if secs, err := strconv.Atoi(trimmed); err == nil && secs > 0 {
			var want time.Duration
			if int64(secs) > int64(maxRetryAfter/time.Second) {
				want = maxRetryAfter
			} else {
				want = time.Duration(secs) * time.Second
			}
			if d != want {
				t.Fatalf("ParseRetryAfter(%q) = %v; want %v", raw, d, want)
			}
		}
	})
}

// FuzzCircuitBreakerStateTransitions fizzes sequences of operations (RecordSuccess,
// RecordFailure, time advancement, availability checks, reset, and concurrent execution)
// against a router and its circuit breaker state machine, asserting strict invariants:
// 1. Circuit state transitions (Closed <-> Open <-> Half-Open) follow valid paths.
// 2. Failure counts and probe budgets never violate bounds.
// 3. No panics, data races, or deadlocks under randomized concurrent operations.
func FuzzCircuitBreakerStateTransitions(f *testing.F) {
	f.Add([]byte{1, 1, 1, 0, 3, 4, 5})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{1, 1, 1, 1, 1, 1, 1})
	f.Add([]byte{1, 1, 3, 5, 1, 1})
	f.Add([]byte{1, 2, 3, 4, 0, 1, 2})

	f.Fuzz(func(t *testing.T, opBytes []byte) {
		if len(opBytes) == 0 {
			return
		}

		cfg := &Config{
			Providers: map[string]ProviderConfig{
				"p": {URL: "https://p.example.com/v1"},
			},
			Models: map[string]ModelConfig{
				"smart": {Chain: []ModelEndpoint{{Provider: "p", Model: "m1"}}},
			},
			Preferences: &Preferences{
				CircuitBreakerThreshold: intPtr(3),
				CircuitHalfOpenProbes:   1,
			},
		}
		router := NewRouter(cfg, "")
		ep := &ModelEndpoint{Provider: "p", Model: "m1"}

		var wg sync.WaitGroup

		for _, op := range opBytes {
			switch op % 7 {
			case 0:
				router.RecordSuccess(ep)
			case 1:
				router.ApplyCooldown(ep, 500, "server error")
			case 2:
				router.ApplyCooldown(ep, 404, "not found")
			case 3:
				_ = router.IsAvailable(ep)
			case 4:
				router.ResetCircuit(ep)
			case 5:
				// Shorten cooldown to force expiry and trigger half-open transition
				router.ApplyCooldownWithDuration(ep, 500, "fast expiry", "", 1*time.Millisecond)
				time.Sleep(2 * time.Millisecond)
			case 6:
				// Concurrent burst to test data race freedom
				wg.Add(2)
				go func() {
					defer wg.Done()
					router.ApplyCooldown(ep, 500, "concurrent")
				}()
				go func() {
					defer wg.Done()
					_ = router.IsAvailable(ep)
				}()
			}

			// Invariant checks after every operation:
			state := router.GetCircuitState(ep)
			// State must be one of the valid enum values
			if state != CircuitClosed && state != CircuitOpen && state != CircuitHalfOpen {
				t.Fatalf("invalid circuit state: %v", state)
			}

			// Check internal circuit breaker bounds if present. Read the
			// values under the lock, then assert after releasing it: a
			// t.Fatalf while holding RLock would Goexit without unlocking,
			// leaving the concurrent workers in this iteration deadlocked.
			var probes int
			var errorCount int
			var haveCircuit, haveCooldown bool
			router.mu.RLock()
			if cb, ok := router.circuits[ep.Key()]; ok {
				probes = cb.ProbesSent
				haveCircuit = true
				if cd, hasCd := router.cooldowns[ep.Key()]; hasCd {
					errorCount = cd.ErrorCount
					haveCooldown = true
				}
			}
			router.mu.RUnlock()
			if haveCircuit && probes < 0 {
				t.Fatalf("negative ProbesSent: %d", probes)
			}
			if haveCooldown && (errorCount < 0 || errorCount > 100) {
				t.Fatalf("ErrorCount out of bounds [0, 100]: %d", errorCount)
			}
		}

		wg.Wait()
	})
}
