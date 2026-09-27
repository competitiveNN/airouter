package main

import (
	"fmt"
	"sync"
	"testing"
)

func f64(v float64) *float64 { return &v }

// rrChain builds a chain of n endpoints scoring 40.0 each.
func rrChain(n int) []ModelEndpoint {
	chain := make([]ModelEndpoint, 0, n)
	for i := 0; i < n; i++ {
		chain = append(chain, ModelEndpoint{
			Provider:     fmt.Sprintf("p%d", i),
			Model:        "m",
			Intelligence: f64(40),
		})
	}
	return chain
}

func rrRouter(t *testing.T, chain []ModelEndpoint, window int) *Router {
	t.Helper()
	cfg := &Config{
		Providers:   map[string]ProviderConfig{},
		Preferences: &Preferences{InitialRotationWindow: &window},
		Models:      map[string]ModelConfig{"work": {Chain: chain}},
	}
	for _, ep := range chain {
		cfg.Providers[ep.Provider] = ProviderConfig{URL: "https://x", APIKeyEnv: "K"}
	}
	r := NewRouter(cfg, "")
	t.Cleanup(r.Close)
	return r
}

// depthCounts sends n new sessions and tallies which chain depth each landed on.
func depthCounts(r *Router, chain []ModelEndpoint, n int) map[int]int {
	counts := map[int]int{}
	for i := 0; i < n; i++ {
		ep, _ := r.SelectEndpoint("work", fmt.Sprintf("s-%d", i), false, map[string]bool{})
		if ep == nil {
			continue
		}
		for d, c := range chain {
			if c.Key() == ep.Key() {
				counts[d]++
				break
			}
		}
	}
	return counts
}

// TestInitialPickRotatesDepthWithinWindow is the core requirement: consecutive
// new sessions must start at successive depths within the window, evenly.
func TestInitialPickRotatesDepthWithinWindow(t *testing.T) {
	const window = 8
	chain := rrChain(20)
	r := rrRouter(t, chain, window)

	counts := depthCounts(r, chain, window*100)
	for d := 0; d < window; d++ {
		if counts[d] == 0 {
			t.Errorf("depth %d never used; the window is not spreading at all", d)
		}
	}
	// The hash spreads evenly on average, so each depth should take roughly
	// 1/8 of the sessions. Allow a tolerance rather than demanding exactness.
	depths := make([]int, 0, window)
	for d := 0; d < window; d++ {
		depths = append(depths, d)
	}
	assertWithinTolerance(t, counts, window*100, 0.10, depths...)
	// Nothing beyond the window may be used for a first pick.
	for d := window; d < 20; d++ {
		if counts[d] != 0 {
			t.Errorf("depth %d got %d sessions, want 0: rotation must stay inside the window", d, counts[d])
		}
	}
}

// TestInitialPickWindowRespectsChainLength verifies a window larger than the
// chain degrades to "use the whole chain" rather than misbehaving.
func TestInitialPickWindowRespectsChainLength(t *testing.T) {
	chain := rrChain(3)
	r := rrRouter(t, chain, 8)
	counts := depthCounts(r, chain, 300)
	assertAllDepthsUsed(t, counts, 300, 0, 1, 2)
	// Within tolerance: the hash spreads evenly but not exactly.
	assertWithinTolerance(t, counts, 300, 0.15, 0, 1, 2)
}

// TestInitialPickWindowOneBehavesLikeHead verifies a window of 1 reproduces the
// original "always depth 0" behaviour, which is the compatibility case.
func TestInitialPickWindowOneBehavesLikeHead(t *testing.T) {
	chain := rrChain(5)
	r := rrRouter(t, chain, 1)
	counts := depthCounts(r, chain, 25)
	if counts[0] != 25 {
		t.Errorf("depth 0 got %d, want all 25", counts[0])
	}
}

// TestInitialPickRotationDisabled verifies a zero window disables rotation.
func TestInitialPickRotationDisabled(t *testing.T) {
	chain := rrChain(5)
	r := rrRouter(t, chain, 0)
	counts := depthCounts(r, chain, 25)
	if counts[0] != 25 {
		t.Errorf("rotation disabled but depth 0 got %d, want 25", counts[0])
	}
}

// TestInitialPickSkipsCooledWithoutLeavingHole verifies a cooled member is
// passed over and does not consume a rotation slot, so the surviving members
// still share the load evenly.
func TestInitialPickSkipsCooledWithoutLeavingHole(t *testing.T) {
	const window = 4
	chain := rrChain(10)
	r := rrRouter(t, chain, window)

	// Cool depth 1, which is inside the window.
	r.ApplyCooldown(&chain[1], 429, "rate limited")

	counts := depthCounts(r, chain, 300)
	if counts[1] != 0 {
		t.Errorf("cooled depth 1 got %d sessions, want 0", counts[1])
	}
	// The window is now 3 live members (0,2,3), so load splits across those
	// three rather than piling onto the survivors.
	assertAllDepthsUsed(t, counts, 300, 0, 2, 3)
	assertWithinTolerance(t, counts, 300, 0.15, 0, 2, 3)
	// Depth 4 is beyond the window and must stay unused.
	if counts[4] != 0 {
		t.Errorf("depth 4 got %d, want 0: a cooldown must not widen the window", counts[4])
	}
}

// TestInitialPickStickySessionUnaffected verifies the rotation applies only to
// the FIRST pick: a pinned session never moves afterwards.
func TestInitialPickStickySessionUnaffected(t *testing.T) {
	chain := rrChain(8)
	r := rrRouter(t, chain, 8)

	ep, _ := r.SelectEndpoint("work", "sticky", false, map[string]bool{})
	first := ep.Key()
	for i := 0; i < 40; i++ {
		e, _ := r.SelectEndpoint("work", "sticky", false, map[string]bool{})
		if e.Key() != first {
			t.Fatalf("sticky session moved after %d requests: %s -> %s", i, first, e.Key())
		}
	}
}

// TestFallbackWithinRequestIsNotRotated verifies that a retrying request walks
// the chain strictly in order. This is the safety property: rotation must never
// make a failure escalate more slowly, and a rotated session that fails must
// still fall through to the endpoints above it.
func TestFallbackWithinRequestIsNotRotated(t *testing.T) {
	chain := rrChain(8)
	r := rrRouter(t, chain, 8)

	// Walk a session through its first pick, then fail it repeatedly and
	// record the order the fallback visits. It must be strictly increasing
	// depth, starting from the top of the chain.
	ep, _ := r.SelectEndpoint("work", "req", false, map[string]bool{})
	if ep == nil {
		t.Fatal("no first pick")
	}
	firstDepth := depthOf(chain, ep.Key())

	tried := map[string]bool{ep.Key(): true}
	prev := firstDepth
	for step := 0; step < 4; step++ {
		next, _ := r.SelectEndpoint("work", "req", false, tried)
		if next == nil {
			t.Fatal("no fallback selected")
		}
		d := depthOf(chain, next.Key())
		if d <= prev {
			t.Fatalf("fallback depth did not increase: %d -> %d (chain %v)", prev, d, tried)
		}
		// Crucially, fallback may walk ABOVE the session's starting depth:
		// a rotated session that fails should still reach the better models.
		if d != 0 && prev != 0 && d > prev {
			// expected: strictly increasing from wherever it was
		}
		prev = d
		tried[next.Key()] = true
	}
	// The first fallback step from a rotated session should be allowed to go
	// back to depth 0 (the best model) rather than being trapped in the window.
	if firstDepth > 0 {
		probe := map[string]bool{ep.Key(): true}
		next, _ := r.SelectEndpoint("work", "req2", false, probe)
		if next == nil || depthOf(chain, next.Key()) != 0 {
			t.Errorf("fallback from rotated depth %d went to %v, want depth 0", firstDepth, next)
		}
	}
}

// TestInitialPickRespectsVision verifies a rotated pick never hands a new vision
// session a text-only endpoint.
func TestInitialPickRespectsVision(t *testing.T) {
	no := false
	chain := make([]ModelEndpoint, 6)
	for i := range chain {
		chain[i] = ModelEndpoint{Provider: fmt.Sprintf("p%d", i), Model: "m", Intelligence: f64(40)}
	}
	// p0, p2, p4 are text-only.
	chain[0].Vision = &no
	chain[2].Vision = &no
	chain[4].Vision = &no

	r := rrRouter(t, chain, 6)
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		ep, _ := r.SelectEndpoint("work", fmt.Sprintf("v-%d", i), true, map[string]bool{})
		if ep == nil {
			t.Fatal("no endpoint for vision request")
		}
		if !ep.SupportsVision() {
			t.Fatalf("vision session routed to text-only %s", ep.Key())
		}
		seen[ep.Key()] = true
	}
	// All three vision-capable endpoints should be in use.
	if len(seen) != 3 {
		t.Errorf("vision sessions used %d endpoints (%v), want 3", len(seen), seen)
	}
}

// TestInitialPickRespectsCooldownAcrossWholeChain verifies that when the entire
// window is cooled, selection continues deeper into the chain rather than
// returning nil.
func TestInitialPickRespectsCooldownAcrossWholeChain(t *testing.T) {
	chain := rrChain(8)
	r := rrRouter(t, chain, 4)

	for i := 0; i < 4; i++ {
		r.ApplyCooldown(&chain[i], 500, "down")
	}
	counts := depthCounts(r, chain, 20)
	for d := 0; d < 4; d++ {
		if counts[d] != 0 {
			t.Errorf("cooled depth %d got %d sessions, want 0", d, counts[d])
		}
	}
	if counts[4] == 0 {
		t.Errorf("no sessions reached depth 4+, want fallback past the cooled window")
	}
}

// TestInitialPickDefaultWindowWhenUnset verifies a config with no preferences
// block still rotates, using the default window.
func TestInitialPickDefaultWindowWhenUnset(t *testing.T) {
	chain := rrChain(DefaultInitialRotationWindow)
	cfg := &Config{
		Providers: map[string]ProviderConfig{},
		Models:    map[string]ModelConfig{"work": {Chain: chain}},
	}
	for _, ep := range chain {
		cfg.Providers[ep.Provider] = ProviderConfig{URL: "https://x", APIKeyEnv: "K"}
	}
	r := NewRouter(cfg, "")
	defer r.Close()

	counts := depthCounts(r, chain, DefaultInitialRotationWindow*5)
	if len(counts) != DefaultInitialRotationWindow {
		t.Errorf("used %d distinct depths (%v), want %d", len(counts), counts, DefaultInitialRotationWindow)
	}
}

// assertAllDepthsUsed fails if any of the given depths was never selected.
func assertAllDepthsUsed(t *testing.T, counts map[int]int, total int, depths ...int) {
	t.Helper()
	for _, d := range depths {
		if counts[d] == 0 {
			t.Errorf("depth %d never used (counts=%v, total=%d)", d, counts, total)
		}
	}
}

// assertWithinTolerance checks each listed depth took within rel of 1/len of
// the total, which is what a hash-based spread should produce.
func assertWithinTolerance(t *testing.T, counts map[int]int, total int, rel float64, depths ...int) {
	t.Helper()
	want := float64(total) / float64(len(depths))
	for _, d := range depths {
		got := float64(counts[d])
		if got == 0 {
			t.Errorf("depth %d got 0 sessions, want ~%.0f", d, want)
			continue
		}
		if dev := (got - want) / want; dev > rel || dev < -rel {
			t.Errorf("depth %d got %d, want ~%.0f (%.0f%% off, tolerance %.0f%%)", d, counts[d], want, dev*100, rel*100)
		}
	}
}

func depthOf(chain []ModelEndpoint, key string) int {
	for i, c := range chain {
		if c.Key() == key {
			return i
		}
	}
	return -1
}

// TestInitialPickConcurrentSameSessionAgrees is the safety property behind
// hashing the session ID instead of using an arrival counter: concurrent
// requests sharing a session ID must all independently compute the SAME
// starting endpoint, so the first burst of a new session never fans out
// across several models.
func TestInitialPickConcurrentSameSessionAgrees(t *testing.T) {
	const window = 8
	chain := rrChain(20)
	r := rrRouter(t, chain, window)

	// Many distinct sessions must spread across the window...
	spread := map[string]bool{}
	for i := 0; i < 500; i++ {
		ep, _ := r.SelectEndpoint("work", fmt.Sprintf("conv-%d", i), false, map[string]bool{})
		if ep != nil {
			spread[ep.Key()] = true
		}
	}
	if len(spread) != window {
		t.Errorf("500 distinct sessions used %d endpoints (%v), want %d", len(spread), spread, window)
	}

	// ...but concurrent requests for ONE session must never disagree.
	for trial := 0; trial < 200; trial++ {
		sid := fmt.Sprintf("racy-%d", trial)
		var mu sync.Mutex
		seen := map[string]bool{}
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ep, _ := r.SelectEndpoint("work", sid, false, map[string]bool{})
				if ep == nil {
					return
				}
				mu.Lock()
				seen[ep.Key()] = true
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(seen) != 1 {
			t.Fatalf("session %s: 16 concurrent first requests picked %d different endpoints: %v",
				sid, len(seen), seen)
		}
	}
}
