package main

import (
	"fmt"
	"testing"
	"time"
)

// This file pins the chain-completeness guarantees of the fallback
// walk, so the "depth-based chunk" failure mode cannot regress
// silently:
//
//   - A session's fallback is never confined to the depths at or after
//     its starting depth. Whatever depth a session starts at (rotation
//     assigns new sessions to depths 0..window-1), once its pinned
//     model fails, the walk covers the WHOLE chain, head included.
//   - When every endpoint has been tried, the walk cycles back to the
//     first eligible one -- the last model in the chain leads back to
//     the first -- so a request whose candidates have all failed still
//     gets another chance at any endpoint that has recovered.
//   - A new session whose rotation window (its depth-based chunk at the
//     head of the chain) is fully unavailable falls through to the
//     first eligible model deeper in the chain, rather than failing.
//   - When NO endpoint is eligible, the router reports the shortest
//     remaining cooldown instead of an endpoint; every fallback loop
//     bounds its retries by that wait, the wall-clock budget, and the
//     attempt counter, so a request can fail but never spins forever.

// pinSession pins sessionID to ep, the state a session is in after its
// first successful selection (or after a fallback step re-pinned it).
func pinSession(r *Router, sessionID string, ep ModelEndpoint) {
	r.mu.Lock()
	r.sessions[sessionID] = sessionEntry{ep: ep, lastUsed: time.Now()}
	r.mu.Unlock()
}

// coolFor parks an endpoint on a long cooldown, the state a real
// 429/auth failure leaves behind for the rest of the request's walk.
func coolFor(r *Router, ep *ModelEndpoint, sessionID string) {
	r.ApplyCooldownWithDuration(ep, 429, "rate limited", sessionID, time.Hour)
}

// TestFallbackReachesChainHeadFromAnyDepth pins that a session's
// fallback is never confined to a depth-based chunk: whatever depth the
// session started at, once its pinned model fails, the walk reaches the
// HEAD of the chain -- including depths the session never tried.
func TestFallbackReachesChainHeadFromAnyDepth(t *testing.T) {
	for d := 0; d < 5; d++ {
		// A fresh router per depth: each iteration cools its pinned
		// model, and cooldowns from an earlier depth must not leak
		// into the next one's expectation.
		chain := rrChain(5)
		r := rrRouter(t, chain, 0) // rotation off: pinning is deterministic
		sessionID := fmt.Sprintf("session-depth-%d", d)
		pinSession(r, sessionID, chain[d])

		// The pinned model failed in this request's fallback walk.
		tried := map[string]bool{chain[d].Key(): true}
		coolFor(r, &chain[d], sessionID)

		ep, _ := r.SelectEndpoint("work", sessionID, false, tried)
		if ep == nil {
			t.Fatalf("depth %d: no endpoint after the pinned model failed; the walk must cover the whole chain", d)
		}
		if ep.Key() == chain[d].Key() {
			t.Fatalf("depth %d: fallback returned the failed model %s", d, ep.Key())
		}
		// The walk starts at the head, so depth 0's fallback is the
		// second model and every deeper depth's fallback is the head.
		want := 0
		if d == 0 {
			want = 1
		}
		if ep.Key() != chain[want].Key() {
			t.Fatalf("depth %d: fallback returned %s, want %s -- the walk must reach the head of the chain, not stop at the session's depth", d, ep.Key(), chain[want].Key())
		}
	}
}

// TestFallbackSkipsUnavailableHeadToReachTail is the mirror of the
// rotation-window case for an established session: consecutive depths
// at the head of the chain (where the walk starts) are on cooldown, and
// the fallback must continue past all of them to the first eligible
// model deeper in the chain.
func TestFallbackSkipsUnavailableHeadToReachTail(t *testing.T) {
	chain := rrChain(4)
	r := rrRouter(t, chain, 0)
	sessionID := "head-down"
	pinSession(r, sessionID, chain[0])

	// The first two depths of the chain are unavailable.
	coolFor(r, &chain[0], sessionID)
	coolFor(r, &chain[1], sessionID)

	ep, _ := r.SelectEndpoint("work", sessionID, false, map[string]bool{})
	if ep == nil {
		t.Fatal("no endpoint after the head of the chain went down; the walk must continue past cooled models")
	}
	if ep.Key() != chain[2].Key() {
		t.Fatalf("expected the first model past the cooled head (%s), got %s", chain[2].Key(), ep.Key())
	}
}

// TestSelectEndpointCyclesBackFromLastModelToFirst pins the cycle-back
// itself: a session pinned to the LAST model in the chain, having tried
// every endpoint, still gets the FIRST one back once its cooldown has
// expired -- the walk does not stop at the end of the chain.
func TestSelectEndpointCyclesBackFromLastModelToFirst(t *testing.T) {
	chain := rrChain(3)
	r := rrRouter(t, chain, 0)
	sessionID := "cycle-back"
	// The session is pinned to the last model in the chain.
	pinSession(r, sessionID, chain[2])

	// Every endpoint has been tried in this request's fallback walk.
	tried := map[string]bool{}
	for i := range chain {
		tried[chain[i].Key()] = true
	}
	// The deeper models are still on cooldown; the head's has expired,
	// so it is eligible again.
	coolFor(r, &chain[1], sessionID)
	coolFor(r, &chain[2], sessionID)

	ep, _ := r.SelectEndpoint("work", sessionID, false, tried)
	if ep == nil {
		t.Fatal("expected the cycle-back to return the recovered head, got nil")
	}
	if ep.Key() != chain[0].Key() {
		t.Fatalf("expected the cycle-back to return the first model %s, got %s", chain[0].Key(), ep.Key())
	}
}

// TestNewSessionSurvivesFullyUnavailableRotationWindow pins that a
// request whose depth-based chunk -- the rotation window at the head of
// the chain -- is fully unavailable still gets a chance: the initial
// pick falls through to the first eligible model deeper in the chain
// instead of failing the request.
func TestNewSessionSurvivesFullyUnavailableRotationWindow(t *testing.T) {
	chain := rrChain(5)
	const window = 2
	r := rrRouter(t, chain, window)

	// The whole rotation window (depths 0 and 1) is on cooldown. The
	// session ID cannot matter here: with no eligible window member
	// there is no hash to compute, so the fall-through is deterministic.
	sessionID := "chunk-down"
	coolFor(r, &chain[0], sessionID)
	coolFor(r, &chain[1], sessionID)

	ep, _ := r.SelectEndpoint("work", sessionID, false, map[string]bool{})
	if ep == nil {
		t.Fatal("expected a deeper model when the rotation window is fully unavailable, got nil")
	}
	if ep.Key() != chain[2].Key() {
		t.Fatalf("expected the first model outside the window (%s), got %s", chain[2].Key(), ep.Key())
	}
}

// TestSelectEndpointFullyUnavailableChainIsBounded pins the failure mode
// when NO endpoint in the chain is eligible: the router reports the
// shortest remaining cooldown rather than an endpoint. Every fallback
// loop bounds its retries by that wait, the wall-clock budget, and the
// attempt counter, so a request can fail cleanly but never spins
// forever hammering cooled models.
func TestSelectEndpointFullyUnavailableChainIsBounded(t *testing.T) {
	chain := rrChain(3)
	r := rrRouter(t, chain, 2)
	sessionID := "all-down"
	for i := range chain {
		coolFor(r, &chain[i], sessionID)
	}
	tried := map[string]bool{}
	for i := range chain {
		tried[chain[i].Key()] = true
	}

	ep, wait := r.SelectEndpoint("work", sessionID, false, tried)
	if ep != nil {
		t.Fatalf("expected no endpoint when the whole chain is on cooldown, got %s", ep.Key())
	}
	if wait <= 0 {
		t.Fatal("expected a non-zero wait so the handler backs off instead of hammering cooled models")
	}
}
