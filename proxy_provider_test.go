package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The proxy setting lives on the provider so a user enables proxying with one
// line instead of one per chain entry. These tests pin the resolution order
// (endpoint beats provider, provider beats default-direct) and the degenerate
// cases a bare &Proxy{} in other suites hits.

func TestProxyTriesEndpointOverridesProvider(t *testing.T) {
	p := NewProxy(&Config{Providers: map[string]ProviderConfig{
		"prov": {Proxy: 3},
	}})
	ep := &ModelEndpoint{Provider: "prov", Model: "m", Proxy: 7}

	if got := p.proxyTries(ep); got != 7 {
		t.Fatalf("endpoint override: proxyTries = %d, want 7", got)
	}
}

func TestProxyTriesInheritsProviderValue(t *testing.T) {
	p := NewProxy(&Config{Providers: map[string]ProviderConfig{
		"prov": {Proxy: 3},
	}})
	ep := &ModelEndpoint{Provider: "prov", Model: "m"}

	if got := p.proxyTries(ep); got != 3 {
		t.Fatalf("provider inheritance: proxyTries = %d, want 3", got)
	}
}

// Providers that proxy while others do not must not leak their budget across
// the chain: resolve per endpoint, not globally.
func TestProxyTriesResolvesPerProvider(t *testing.T) {
	p := NewProxy(&Config{Providers: map[string]ProviderConfig{
		"proxied":  {Proxy: 4},
		"direct":   {Proxy: 0},
		"unset":    {},
		"unlisted": {},
	}})

	cases := []struct {
		provider string
		want     int
	}{
		{"proxied", 4},
		{"direct", 0},
		{"unset", 0},
		{"missing-from-config", 0},
	}
	for _, tc := range cases {
		ep := &ModelEndpoint{Provider: tc.provider, Model: "m"}
		if got := p.proxyTries(ep); got != tc.want {
			t.Errorf("provider %q: proxyTries = %d, want %d", tc.provider, got, tc.want)
		}
	}
}

// A negative value is nonsense YAML, but it must not resolve into a proxy
// budget: api.go's retry loop would then never execute an attempt.
func TestProxyTriesNegativeProviderIsDirect(t *testing.T) {
	p := NewProxy(&Config{Providers: map[string]ProviderConfig{
		"prov": {Proxy: -5},
	}})
	ep := &ModelEndpoint{Provider: "prov", Model: "m"}

	if got := p.proxyTries(ep); got != 0 {
		t.Fatalf("negative provider value: proxyTries = %d, want 0", got)
	}
}

// proxyTries is called on the request path from Forward/StreamToClient, which
// several unit tests reach with a bare &Proxy{} whose config was never stored.
// A nil config must read as "direct", not panic.
func TestProxyTriesNilReceiverAndNilConfig(t *testing.T) {
	ep := &ModelEndpoint{Provider: "prov", Model: "m"}

	var nilProxy *Proxy
	if got := nilProxy.proxyTries(ep); got != 0 {
		t.Errorf("nil receiver: proxyTries = %d, want 0", got)
	}
	if got := (&Proxy{}).proxyTries(ep); got != 0 {
		t.Errorf("unconfigured proxy: proxyTries = %d, want 0", got)
	}
}

// The endpoint's own value still resolves when the proxy is unconfigured, so
// a bare &Proxy{} in another suite does not silently drop configured behaviour.
func TestProxyTriesNilProxyKeepsEndpointValue(t *testing.T) {
	ep := &ModelEndpoint{Provider: "prov", Model: "m", Proxy: 5}

	for name, p := range map[string]*Proxy{"nil receiver": nil, "unconfigured config": {}} {
		if got := p.proxyTries(ep); got != 5 {
			t.Errorf("%s: proxyTries = %d, want 5", name, got)
		}
	}
}

// proxyEnabled gates the pool-fetching goroutine in main.go. Enabling the
// provider alone must start it -- that is the whole point of the provider-level
// key, since no chain entry carries a value of its own.
func TestProxyEnabledProviderLevel(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"prov": {Proxy: 2}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "prov", Model: "m"}}}},
	}
	if !proxyEnabled(cfg) {
		t.Fatal("proxyEnabled = false, want true: provider sets proxy and no endpoint overrides it")
	}
}

func TestProxyEnabledEndpointLevelOnly(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"prov": {}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "prov", Model: "m", Proxy: 2}}}},
	}
	if !proxyEnabled(cfg) {
		t.Fatal("proxyEnabled = false, want true: endpoint carries its own proxy value")
	}
}

func TestProxyEnabledAllDirect(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"prov": {Proxy: 0}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "prov", Model: "m", Proxy: 0}}}},
	}
	if proxyEnabled(cfg) {
		t.Fatal("proxyEnabled = true, want false: everything is direct")
	}
	if proxyEnabled(nil) {
		t.Fatal("proxyEnabled(nil) = true, want false")
	}
	if proxyEnabled(&Config{}) {
		t.Fatal("proxyEnabled(empty) = true, want false")
	}
}

// End-to-end through the YAML tags the config file actually uses, so a renamed
// or mistyped key cannot silently resolve to direct forever.
func TestProxyFieldsParseFromYAML(t *testing.T) {
	raw := `
providers:
  proxied-provider:
    url: https://example.invalid/v1
    api_key_env: PROXIED_API_KEY
    proxy: 6
  plain-provider:
    url: https://example.invalid/v1
    api_key_env: PLAIN_API_KEY
models:
  smart:
    chain:
      - provider: proxied-provider
        model: inherits
      - provider: proxied-provider
        model: overrides
        proxy: 9
      - provider: plain-provider
        model: direct
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := cfg.Providers["proxied-provider"].Proxy; got != 6 {
		t.Errorf("provider proxy = %d, want 6", got)
	}
	if got := cfg.Providers["plain-provider"].Proxy; got != 0 {
		t.Errorf("provider without the key: proxy = %d, want 0", got)
	}

	chain := cfg.Models["smart"].Chain
	if len(chain) != 3 {
		t.Fatalf("chain len = %d, want 3", len(chain))
	}

	p := NewProxy(&cfg)
	want := []struct {
		provider string
		model    string
		tries    int
	}{
		{"proxied-provider", "inherits", 6},
		{"proxied-provider", "overrides", 9},
		{"plain-provider", "direct", 0},
	}
	for i, tc := range want {
		ep := &chain[i]
		if ep.Provider != tc.provider || ep.Model != tc.model {
			t.Fatalf("chain[%d] = %s/%s, want %s/%s", i, ep.Provider, ep.Model, tc.provider, tc.model)
		}
		if got := p.proxyTries(ep); got != tc.tries {
			t.Errorf("%s/%s: proxyTries = %d, want %d", tc.provider, tc.model, got, tc.tries)
		}
	}

	if !proxyEnabled(&cfg) {
		t.Error("proxyEnabled = false, want true")
	}
}
