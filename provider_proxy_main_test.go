//go:build provider_proxy

package main

import "testing"

func TestProviderPrefixes(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderConfig{
		"opencode":    {},
		"opencode2":   {},
		"opencode3":   {},
		"opencode4":   {},
		"gemini":      {},
		"gemini2":     {},
		"nvidia":      {},
		"nvidia2":     {},
		"kilocode":    {},
		"ollama":      {},
		"commandcode": {},
	}}

	got := providerPrefixes(cfg)
	want := map[string]string{
		"opencode":    "opencode1",
		"opencode2":   "opencode2",
		"opencode3":   "opencode3",
		"opencode4":   "opencode4",
		"gemini":      "gemini1",
		"gemini2":     "gemini2",
		"nvidia":      "nvidia1",
		"nvidia2":     "nvidia2",
		"kilocode":    "kilocode",
		"ollama":      "ollama",
		"commandcode": "commandcode",
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("providerPrefixes()[%q] = %q, want %q", name, got[name], expected)
		}
	}
}

func TestProviderPrefixesAvoidsCollision(t *testing.T) {
	// A bare name and an explicit *1 together must not collide: the bare
	// provider takes the next free number.
	cfg := &Config{Providers: map[string]ProviderConfig{
		"gemini":  {},
		"gemini1": {},
		"gemini2": {},
	}}
	got := providerPrefixes(cfg)
	if got["gemini1"] != "gemini1" || got["gemini2"] != "gemini2" {
		t.Fatalf("numbered providers changed: %#v", got)
	}
	if got["gemini"] == "" || got["gemini"] == "gemini1" || got["gemini"] == "gemini2" {
		t.Fatalf("bare provider got a colliding prefix: %q", got["gemini"])
	}
}

func TestResolveModel(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderConfig{
		"opencode":  {},
		"opencode2": {},
	}}
	s := &providerServer{}

	name, upstream, ok := s.resolveModel(cfg, "opencode1/deepseek-ai/deepseek-v3")
	if !ok || name != "opencode" || upstream != "deepseek-ai/deepseek-v3" {
		t.Fatalf("resolveModel = (%q, %q, %v), want (opencode, deepseek-ai/deepseek-v3, true)", name, upstream, ok)
	}

	if _, _, ok := s.resolveModel(cfg, "deepseek-ai/deepseek-v3"); ok {
		t.Fatal("resolveModel accepted an unprefixed model")
	}
	if _, _, ok := s.resolveModel(cfg, "unknown/model"); ok {
		t.Fatal("resolveModel accepted an unknown prefix")
	}
}

func TestSplitTrailingNumber(t *testing.T) {
	cases := []struct {
		in   string
		base string
		num  int
	}{
		{"opencode", "opencode", 0},
		{"opencode2", "opencode", 2},
		{"nvidia10", "nvidia", 10},
		{"kilocode", "kilocode", 0},
		{"2fast", "2fast", 0},
	}
	for _, c := range cases {
		base, num := splitTrailingNumber(c.in)
		if base != c.base || num != c.num {
			t.Errorf("splitTrailingNumber(%q) = (%q, %d), want (%q, %d)", c.in, base, num, c.base, c.num)
		}
	}
}
