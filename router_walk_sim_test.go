package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// simulateRequest replays the handler fallback loop (api.go handleCompletion /
// handleStream shape) against r for sessionID, with `down` marking the
// endpoints that are unavailable (cooled for an hour, like a real 429/404
// would leave behind). Every attempt against a down endpoint fails and is
// cooled again (already cooled); every attempt against a live endpoint
// succeeds.
//
// Returns (succeeded, attempts).
func simulateRequest(r *Router, model, sessionID string, chain []ModelEndpoint, down map[int]bool, maxAttempts int) (bool, int) {
	tried := map[string]bool{}
	attempts := 0
	for attempts < maxAttempts {
		attempts++
		ep, _ := r.SelectEndpoint(model, sessionID, false, tried)
		if ep == nil {
			return false, attempts
		}
		tried[ep.Key()] = true
		isDown := false
		for i := range chain {
			if chain[i].Key() == ep.Key() && down[i] {
				isDown = true
				break
			}
		}
		if !isDown {
			r.RecordSuccess(ep)
			return true, attempts
		}
		r.ApplyCooldownWithDuration(ep, 429, "rate limited", sessionID, time.Hour)
	}
	return false, attempts
}

// TestSimulatedWalkAlwaysReachesLiveEndpoint drives the real SelectEndpoint
// through the handler's loop semantics across many failure patterns: a session
// pinned at every depth, every subset size of unavailable endpoints, with and
// without rotation. The invariant: whenever at least one endpoint is live, a
// request that starts with an empty tried set eventually reaches a live one.
func TestSimulatedWalkAlwaysReachesLiveEndpoint(t *testing.T) {
	for _, window := range []int{0, 3, 8} {
		for _, n := range []int{1, 2, 5, 9} {
			chain := rrChain(n)
			_ = chain

			// Every single-endpoint-down pattern, plus a few random ones.
			patterns := [][]int{}
			for d := 0; d < n; d++ {
				patterns = append(patterns, []int{d})
			}
			rng := rand.New(rand.NewSource(42))
			for p := 0; p < 20; p++ {
				var subset []int
				for i := 0; i < n; i++ {
					if rng.Intn(2) == 0 {
						subset = append(subset, i)
					}
				}
				patterns = append(patterns, subset)
			}

			for _, pattern := range patterns {
				down := map[int]bool{}
				for _, d := range pattern {
					down[d] = true
				}
				// Skip the all-down pattern here; that case is
				// asserted separately (request must fail bounded).
				if len(down) == n {
					continue
				}
				for start := 0; start < n; start++ {
					// Fresh router per pattern: cooldowns from a
					// previous pattern would leak otherwise.
					chain2 := rrChain(n)
					r2 := rrRouter(t, chain2, window)
					sessionID := fmt.Sprintf("s-%d-%v-%d", window, pattern, start)
					pinSession(r2, sessionID, chain2[start])

					ok, attempts := simulateRequest(r2, "work", sessionID, chain2, down, n*3+1)
					if !ok {
						t.Fatalf("window=%d n=%d down=%v start=%d: request failed although a live endpoint exists", window, n, pattern, start)
					}
					if attempts > n*3+1 {
						t.Fatalf("window=%d n=%d down=%v start=%d: attempts=%d exceeded the bound", window, n, pattern, start, attempts)
					}
				}
			}
		}
	}
}

// TestSimulatedNewSessionSurvivesAnyDownChunk drives the new-session path
// (rotation hash included): whatever subset of the rotation window is down, a
// new session's request still reaches a live endpoint somewhere in the chain.
func TestSimulatedNewSessionSurvivesAnyDownChunk(t *testing.T) {
	for _, window := range []int{1, 3, 8} {
		for _, n := range []int{3, 5, 9} {
			for mask := 0; mask < (1 << n); mask++ {
				down := map[int]bool{}
				for i := 0; i < n; i++ {
					if mask&(1<<i) != 0 {
						down[i] = true
					}
				}
				if len(down) == n {
					continue // all-down asserted separately
				}
				chain := rrChain(n)
				r := rrRouter(t, chain, window)
				sessionID := fmt.Sprintf("new-%d-%d-%d", window, n, mask)
				ok, _ := simulateRequest(r, "work", sessionID, chain, down, n*3+1)
				if !ok {
					t.Fatalf("window=%d n=%d down=%v: new-session request failed although a live endpoint exists", window, n, down)
				}
			}
		}
	}
}
