package main

import (
	"fmt"
	"os"
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
