package main

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRealConfigInitialSpread loads the project's real config.yaml and checks
// that new sessions actually spread across the equal-score group at the head
// of each chain. This is the end-to-end check that the intelligence field is
// populated and that the rotation has something to rotate over.
func TestRealConfigInitialSpread(t *testing.T) {
	raw, err := os.ReadFile("config.yaml")
	if err != nil {
		t.Skipf("config.yaml not available: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}

	for _, name := range LogicalModels {
		mc, ok := cfg.Models[name]
		if !ok || len(mc.Chain) == 0 {
			t.Errorf("%s: no chain", name)
			continue
		}
		scored := 0
		for _, ep := range mc.Chain {
			if ep.HasIntelligence() {
				scored++
			}
		}
		if scored == 0 {
			t.Errorf("%s: no endpoint carries an intelligence score; round-robin cannot engage", name)
			continue
		}

		r := NewRouter(&cfg, "")
		counts := map[string]int{}
		const n = 200
		for i := 0; i < n; i++ {
			ep, _ := r.SelectEndpoint(name, fmt.Sprintf("spread-%d", i), false, map[string]bool{})
			if ep == nil {
				t.Errorf("%s: no endpoint for new session %d", name, i)
				break
			}
			counts[ep.Key()]++
		}
		r.Close()

		distinct := len(counts)
		t.Logf("%s: %d/%d endpoints scored, %d distinct endpoints used over %d new sessions",
			name, scored, len(mc.Chain), distinct, n)
		if distinct < 2 {
			t.Errorf("%s: all %d new sessions hit a single endpoint %v; the tied group at the head is not rotating",
				name, n, counts)
		}
		// No single endpoint should own everything.
		for k, c := range counts {
			if c == n {
				t.Errorf("%s: endpoint %s took all %d sessions", name, k, n)
			}
		}
	}
}

// TestRealConfigChainInvariants asserts, from the consumer's side, the shape
// the config generator is supposed to produce. The generator has its own
// tests, but they exercise synthetic input; a config that violates a chain
// invariant still loads here and fails quietly at request time, which is how
// an unroutable profile and a terminator that 403s on every call both shipped.
//
// Each invariant below has already broken at least one release:
//   - an extra `test` profile parsed fine and was permanently unroutable
//     ("Unknown model"), because LogicalModels is exactly the four names;
//   - a chain ending on opencode/big-pickle, which the opencode key is not
//     entitled to serve, made the last-resort endpoint a guaranteed failure;
//   - a missing `intelligence` field silently dropped an endpoint out of
//     initial-session rotation;
//   - a single-key NVIDIA or CommandCode endpoint left a 429 with nowhere to
//     fall through to.
func TestRealConfigChainInvariants(t *testing.T) {
	raw, err := os.ReadFile("config.yaml")
	if err != nil {
		t.Skipf("config.yaml not available: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}

	if len(cfg.Models) != len(LogicalModels) {
		var extra []string
		for name := range cfg.Models {
			if !slices.Contains(LogicalModels, name) {
				extra = append(extra, name)
			}
		}
		t.Errorf("models section has %d profiles, want exactly %v (unroutable: %v)",
			len(cfg.Models), LogicalModels, extra)
	}

	// A key-group source: the same model id on every key of the group, so a
	// rate-limited key has a sibling to fall through to.
	groups := map[string][]string{
		"nvidia-nim":  {"nvidia", "nvidia2", "nvidia3"},
		"commandcode": {"commandcode", "commandcode2"},
	}

	for _, name := range LogicalModels {
		mc, ok := cfg.Models[name]
		if !ok {
			t.Errorf("%s: profile missing", name)
			continue
		}
		chain := mc.Chain
		if len(chain) == 0 {
			t.Errorf("%s: empty chain", name)
			continue
		}

		// The terminator is the chain's last resort, so it must be a
		// meta-router, must be callable, and must not pretend to be a model
		// with a quality score.
		last := chain[len(chain)-1]
		if last.Model != "kilo-auto/free" && last.Model != "big-pickle" {
			t.Errorf("%s: last entry is %s/%s, want an auto router", name, last.Provider, last.Model)
		}
		if last.Provider != "kilocode" {
			// The opencode key is authenticated but not entitled to the free
			// tier: 11 of its 12 models return 403 FreeTierError, paid
			// big-pickle included (re-probed 2026-09-30, still 403). A
			// terminator there fails on every call.
			//
			// regenerate_config.py will promote opencode/big-pickle in exactly
			// one situation: the model list says kilocode's auto router was
			// refused AND opencode's answered a real completion. That is a
			// data-driven emergency a human should see, not a routine state,
			// so the Go suite keeps failing on it — this test has no access to
			// the probe records and cannot tell a justified rescue from a
			// regression. scripts/check-rules.py is the check that can.
			t.Errorf("%s: terminator is on %s, not kilocode; the opencode auto router "+
				"is not entitled to the free tier, so this is only valid when the model "+
				"list records kilocode's router as refused and opencode's as live "+
				"(verify with scripts/check-rules.py)", name, last.Provider)
		}
		if last.Vision == nil || !*last.Vision {
			t.Errorf("%s: terminator is a meta-router and must be marked vision-capable", name)
		}
		if last.HasIntelligence() {
			t.Errorf("%s: terminator carries intelligence %v; it is a router, not a model",
				name, *last.Intelligence)
		}

		for i, ep := range chain[:len(chain)-1] {
			if !ep.HasIntelligence() {
				t.Errorf("%s[%d] %s/%s: no intelligence score, so it is excluded from "+
					"initial-session rotation", name, i, ep.Provider, ep.Model)
			}
			if ep.Vision == nil {
				t.Errorf("%s[%d] %s/%s: vision is nil; a nil flag reads as "+
					"vision-capable, which is a guess rather than a fact",
					name, i, ep.Provider, ep.Model)
			}
		}

		for source, provs := range groups {
			per := map[string]map[string]bool{}
			for _, p := range provs {
				per[p] = map[string]bool{}
			}
			for _, ep := range chain {
				if _, in := per[ep.Provider]; in && ep.Model != "kilo-auto/free" {
					per[ep.Provider][ep.Model] = true
				}
			}
			var all map[string]bool
			for _, mids := range per {
				if all == nil {
					all = mids
					continue
				}
				for mid := range mids {
					all[mid] = true
				}
			}
			for _, p := range provs {
				for mid := range all {
					if !per[p][mid] {
						t.Errorf("%s: %s provider %s is missing %s, which its other %s key(s) serve",
							name, source, p, mid, source)
					}
				}
			}
		}
	}
}
