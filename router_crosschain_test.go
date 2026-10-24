package main

import (
	"fmt"
	"testing"
	"time"
)

// This file pins the cross-chain session failover: the property that a
// session whose own chain has run dry is MOVED to another chain, and stays
// there, instead of failing on every turn.
//
// The failure it closes is not theoretical. A chain runs dry whenever every
// endpoint in it is on cooldown (a 429 with a Retry-After, a 404 whose
// cooldown is seven days), ineligible for the request (no vision support for
// a vision request), or already tried and failed in the current request. The
// session used to be pinned to that chain for its whole life, so a session
// that happened to start during a provider-wide outage never recovered: every
// turn re-entered the dead chain and failed, even though the other three
// chains in the same config were serving traffic happily.
//
// The chains here cover DISJOINT endpoints on purpose. A chain that shares
// endpoints with another cannot answer "which chain served this", and the
// whole question is which chain served it.

// xcChain builds n endpoints under one provider. The model name carries the
// provider, so every endpoint key is unique across the four chains.
func xcChain(provider string, n int) []ModelEndpoint {
	chain := make([]ModelEndpoint, 0, n)
	for i := 0; i < n; i++ {
		chain = append(chain, ModelEndpoint{
			Provider: provider,
			Model:    fmt.Sprintf("%s-%d", provider, i),
		})
	}
	return chain
}

// xcRouter builds a router whose four logical chains cover disjoint
// endpoints, so a selection can be attributed to the chain that served it.
func xcRouter(t *testing.T) *Router {
	t.Helper()
	cfg := &Config{
		Providers:   map[string]ProviderConfig{},
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Models: map[string]ModelConfig{
			"smart": {Chain: xcChain("smart", 2)},
			"work":  {Chain: xcChain("work", 2)},
			"large": {Chain: xcChain("large", 2)},
			"fast":  {Chain: xcChain("fast", 1)},
		},
	}
	for _, model := range cfg.Models {
		for _, ep := range model.Chain {
			cfg.Providers[ep.Provider] = ProviderConfig{URL: "https://x", APIKeyEnv: "K"}
		}
	}
	r := NewRouter(cfg, "")
	t.Cleanup(r.Close)
	return r
}

// xcCool parks an endpoint on a long cooldown: the state a real rate limit
// leaves behind for the rest of the request's walk.
func xcCool(r *Router, ep *ModelEndpoint) {
	r.ApplyCooldownWithDuration(ep, 429, "rate limited", "", time.Hour)
}

// xcCoolChain parks every endpoint of a named chain on a long cooldown.
func xcCoolChain(r *Router, name string) {
	chain := r.Config().Models[name].Chain
	for i := range chain {
		xcCool(r, &chain[i])
	}
}

// xcChainOf reports which logical chain owns ep.
func xcChainOf(r *Router, ep *ModelEndpoint) string {
	for name, model := range r.Config().Models {
		for _, c := range model.Chain {
			if c.Key() == ep.Key() {
				return name
			}
		}
	}
	return ""
}

// xcSessionChain reports the chain a session is currently pinned to.
func xcSessionChain(r *Router, sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[sessionID].chain
}

// TestChainExhaustionFailsOverToAnotherChain is the core requirement: a
// session whose own chain has no usable endpoint left is served by another
// chain, and is RE-PINNED onto it.
func TestChainExhaustionFailsOverToAnotherChain(t *testing.T) {
	r := xcRouter(t)
	xcCoolChain(r, "smart")

	ep, _ := r.SelectEndpoint("smart", "swap", false, nil)
	if ep == nil {
		t.Fatal("expected the session to be served by another chain, got no endpoint")
	}
	served := xcChainOf(r, ep)
	if served == "smart" {
		t.Fatalf("expected a substitute chain, got the dead chain %s", served)
	}
	// The swap must be DURABLE: the session is pinned onto the chain that is
	// actually serving it, so the next turn does not re-descend into the dead
	// chain and fail all over again.
	if got := xcSessionChain(r, "swap"); got != served {
		t.Fatalf("session is pinned to chain %q but is served by %q: the swap did not stick", got, served)
	}
}

// TestFailedOverSessionKeepsItsSubstituteChain pins the durability from the
// other side: the NEXT turn of the same session must still be served by the
// substitute chain. A swap that is re-derived on every request would re-enter
// the dead chain each turn and fail indefinitely, which is the exact failure
// this feature exists to remove.
func TestFailedOverSessionKeepsItsSubstituteChain(t *testing.T) {
	r := xcRouter(t)
	xcCoolChain(r, "smart")

	first, _ := r.SelectEndpoint("smart", "sticky-swap", false, nil)
	if first == nil {
		t.Fatal("expected the session to be served by a substitute chain")
	}
	firstChain := xcChainOf(r, first)

	second, _ := r.SelectEndpoint("smart", "sticky-swap", false, nil)
	if second == nil {
		t.Fatal("expected the session to keep its substitute chain on the next turn")
	}
	if got := xcChainOf(r, second); got != firstChain {
		t.Fatalf("the session moved from %s back to %s: the swap must survive the next turn", firstChain, got)
	}
}

// TestSessionKeepsSwappingChainsUntilOneWorks pins that the failover is not a
// single swap: every chain that runs dry is abandoned in turn, so the session
// keeps moving until it lands on a chain whose models work.
func TestSessionKeepsSwappingChainsUntilOneWorks(t *testing.T) {
	r := xcRouter(t)
	xcCoolChain(r, "smart")
	xcCoolChain(r, "work")

	ep, _ := r.SelectEndpoint("smart", "keep-swapping", false, nil)
	if ep == nil {
		t.Fatal("expected the session to be served by the next working chain")
	}
	if got := xcChainOf(r, ep); got != "large" {
		t.Fatalf("expected the session to land on `large`, got %s", got)
	}

	// `large` goes down as well: the session must swap AGAIN rather than
	// stay on a dead chain or fail the request.
	xcCoolChain(r, "large")
	next, _ := r.SelectEndpoint("smart", "keep-swapping", false, nil)
	if next == nil {
		t.Fatal("expected the session to swap on to the next working chain")
	}
	if got := xcChainOf(r, next); got != "fast" {
		t.Fatalf("expected the session to swap on to `fast`, got %s", got)
	}
}

// TestClientModelChangeWinsOverTheFailedOverChain pins the distinction the
// durable swap depends on: a client that asks for a DIFFERENT model is making
// a new request target, not continuing a stale failover, so the newly
// requested chain must win over the chain the session failed over to.
func TestClientModelChangeWinsOverTheFailedOverChain(t *testing.T) {
	r := xcRouter(t)
	xcCoolChain(r, "smart")

	if _, _ = r.SelectEndpoint("smart", "new-target", false, nil); false {
		t.Fatal("unreachable")
	}
	// The session must have failed over to a REAL substitute chain first:
	// without a swap there is no durable substitute to override, and the
	// distinction this test is about would not exist.
	switch got := xcSessionChain(r, "new-target"); got {
	case "smart", "":
		t.Fatalf("expected the session to fail over to a substitute chain, it is pinned to %q", got)
	}

	ep, _ := r.SelectEndpoint("fast", "new-target", false, nil)
	if ep == nil {
		t.Fatal("expected an endpoint for the newly requested model")
	}
	if got := xcChainOf(r, ep); got != "fast" {
		t.Fatalf("expected the newly requested chain `fast` to win, got %s", got)
	}
}

// TestLegacySessionEntryWithoutChainStaysSticky pins that a session entry
// written before cross-chain failover existed -- one that carries neither a
// chain nor a model -- keeps its sticky endpoint. A legacy entry is bound to
// the chain the client is asking for now, which is exactly the behaviour
// before the failover existed.
func TestLegacySessionEntryWithoutChainStaysSticky(t *testing.T) {
	r := xcRouter(t)
	chain := r.Config().Models["smart"].Chain

	r.mu.Lock()
	r.sessions["legacy"] = sessionEntry{ep: chain[1], lastUsed: time.Now()}
	r.mu.Unlock()

	ep, _ := r.SelectEndpoint("smart", "legacy", false, nil)
	if ep == nil {
		t.Fatal("expected the legacy session to be served")
	}
	if ep.Key() != chain[1].Key() {
		t.Fatalf("the legacy session moved from %s to %s: a legacy entry must keep its sticky endpoint", chain[1].Key(), ep.Key())
	}
	// The normalised entry must keep serving the same endpoint afterwards.
	ep2, _ := r.SelectEndpoint("smart", "legacy", false, nil)
	if ep2 == nil || ep2.Key() != chain[1].Key() {
		t.Fatalf("the legacy session moved on the next turn: got %v, want %s", ep2, chain[1].Key())
	}
}

// TestAllChainsExhaustedReportsTheShortestWait pins the wait that is reported
// when nothing anywhere is usable: the SHORTEST remaining cooldown across all
// the chains, not the requested chain's. Waiting out a chain whose cooldown
// outlasts another chain's buys nothing, and it is the difference between
// recovering in a minute and recovering in an hour.
func TestAllChainsExhaustedReportsTheShortestWait(t *testing.T) {
	r := xcRouter(t)

	// `smart` is down for an hour, `work` for a minute, and the rest for an
	// hour as well.
	smart := r.Config().Models["smart"].Chain
	for i := range smart {
		r.ApplyCooldownWithDuration(&smart[i], 429, "rate limited", "", time.Hour)
	}
	work := r.Config().Models["work"].Chain
	for i := range work {
		r.ApplyCooldownWithDuration(&work[i], 429, "rate limited", "", time.Minute)
	}
	xcCoolChain(r, "large")
	xcCoolChain(r, "fast")

	ep, wait := r.SelectEndpoint("smart", "shortest", false, nil)
	if ep != nil {
		t.Fatalf("expected no endpoint when every chain is down, got %s", ep.Key())
	}
	if wait <= 0 {
		t.Fatal("expected a positive wait so the handler backs off")
	}
	if wait > 2*time.Minute {
		t.Fatalf("expected the shortest cooldown across the chains (~1m), got %v", wait)
	}
}

// TestSubstituteChainOrderPutsTheSmallContextChainLast pins the order the
// substitute chains are consulted in. It is a policy, not an implementation
// detail: `fast` holds the smallest models and therefore the smallest context
// windows, so it is reached only after every chain that can carry a large
// prompt is out of the running. `work` leads because it is the broadest
// general pool, and `smart` follows `large` because in the shipped config it
// is a subset of `work` and would be exhausted whenever `work` is.
func TestSubstituteChainOrderPutsTheSmallContextChainLast(t *testing.T) {
	want := []string{"work", "large", "smart", "fast"}
	if len(substituteChains) != len(want) {
		t.Fatalf("substitute chain order is %v, want %v", substituteChains, want)
	}
	for i, name := range want {
		if substituteChains[i] != name {
			t.Fatalf("substitute chain %d is %q, want %q (the whole order is %v)", i, substituteChains[i], name, substituteChains)
		}
	}
}

// TestFailoverDoesNotReEnterTheDeadChainWithinARequest pins that the walk
// inside one request does not come back to the chain that was abandoned: the
// request moves on and stays moved.
func TestFailoverDoesNotReEnterTheDeadChainWithinARequest(t *testing.T) {
	r := xcRouter(t)
	xcCoolChain(r, "smart")

	ep, _ := r.SelectEndpoint("smart", "within-request", false, nil)
	if ep == nil {
		t.Fatal("expected the session to be served by a substitute chain")
	}
	served := xcChainOf(r, ep)

	// The request's fallback walk continues after the first endpoint failed.
	tried := map[string]bool{ep.Key(): true}
	next, _ := r.SelectEndpoint("smart", "within-request", false, tried)
	if next == nil {
		t.Fatal("expected a further endpoint for the same request")
	}
	if got := xcChainOf(r, next); got == "smart" {
		t.Fatalf("the walk re-entered the abandoned chain %s", got)
	}
	if got := xcChainOf(r, next); got != served {
		t.Fatalf("the walk left the chain the session had moved to: %s -> %s", served, got)
	}
}
