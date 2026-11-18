package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzConfigValidation drives arbitrary YAML text through the configuration
// decoding and validation helpers (yaml decoding into Config plus Config.validate),
// asserting zero panics, graceful error handling on malformed syntax, and
// rejection of invalid structures without crashing.
func FuzzConfigValidation(f *testing.F) {
	seeds := []string{
		"providers:\n  p:\n    url: https://example.com/v1\nmodels:\n  smart:\n    chain:\n      - provider: p\n        model: m\n",
		"",
		"not: [valid\n",
		"providers: {}\nmodels: {}\n",
		"preferences:\n  max_label_cardinality: 0\nproviders:\n  p:\n    url: http://x\nmodels:\n  smart:\n    chain:\n      - provider: p\n        model: m\n",
		"providers:\n  p:\n    url: https://x\n    max_interruption: 999h\nmodels:\n  smart:\n    chain:\n      - provider: p\n        model: m\n",
		"models:\n  smart:\n    chain: []\n",
		"providers:\n  p:\n    url: not-a-url\nmodels:\n  smart:\n    chain:\n      - provider: p\n        model: m\n",
		"providers:\n  p:\n    url: https://x\nmodels:\n  smart:\n    chain:\n      - provider: unknown\n        model: m\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		var cfg Config
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			// Malformed syntax: graceful error, no panic.
			return
		}
		// Validation must not panic on any decoded structure.
		err := cfg.validate()

		// Exercise the preference accessors and chain lookups, which are
		// documented as nil-safe; a panic here would be a defect.
		prefs := cfg.Preferences
		_ = prefs.MaxLabelCardinalityValue()
		_ = prefs.MaxUpstreamConcurrencyValue()
		_ = prefs.RotationWindow()
		_ = prefs.CooldownJitterFraction()
		_ = prefs.CommandCodeProxyMaxConcurrentValue()
		_ = prefs.CircuitBreakerThresholdValue()
		_ = prefs.CircuitHalfOpenProbesValue()
		_ = prefs.DefaultMaxTokensValue()
		_ = prefs.RotationEnabled()
		for _, name := range LogicalModels {
			chain, ok := cfg.GetChain(name)
			_ = ok
			for _, ep := range chain {
				_ = ep.Key()
				_ = ep.SupportsVision()
				_ = ep.ContextWindow()
				_ = ep.UpstreamProtocol()
				_ = ep.HasIntelligence()
				_ = ep.IntelligenceEqual(ep)
			}
		}
		for name := range cfg.Providers {
			_, _ = cfg.GetProvider(name)
		}

		// A successfully validated config must round-trip through marshal +
		// unmarshal + validate without panicking or losing validation.
		if err == nil {
			data, merr := yaml.Marshal(&cfg)
			if merr != nil {
				t.Fatalf("yaml.Marshal panicked/failed on valid config: %v", merr)
			}
			var cfg2 Config
			if uerr := yaml.Unmarshal(data, &cfg2); uerr != nil {
				t.Fatalf("round-trip unmarshal failed: %v", uerr)
			}
			if vErr := cfg2.validate(); vErr != nil {
				t.Fatalf("round-trip config failed validation: %v", vErr)
			}
		}
	})
}
