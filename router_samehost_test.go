package main

import (
	"testing"
)

// TestRouterSkipSameHostMarksEverySiblingKey pins the mechanism that keeps a
// dead HOST from eating a request's whole fallback budget.
//
// Key groups (nvidia/nvidia2/nvidia3, commandcode/commandcode2) intentionally
// sit next to each other in a chain so a 429 on one key falls through to its
// sibling. They also share one upstream URL, so when the host itself is
// unreachable each sibling is a guaranteed repeat of the same timeout. The
// per-request skip must retire the whole group and leave other providers alone.
func TestRouterSkipSameHostMarksEverySiblingKey(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"nv1":   {URL: "https://integrate.api.nvidia.com/v1"},
			"nv2":   {URL: "https://integrate.api.nvidia.com/v1/"},
			"nv3":   {URL: "https://INTEGRATE.api.nvidia.com/v1"},
			"other": {URL: "https://api.kilocode.ai/v1"},
			"empty": {},
		},
		Models: map[string]ModelConfig{
			"work": {Chain: []ModelEndpoint{
				{Provider: "nv1", Model: "m"},
				{Provider: "nv1", Model: "m-sibling"},
				{Provider: "nv2", Model: "m"},
				{Provider: "nv3", Model: "m"},
				{Provider: "other", Model: "m"},
			}},
			"fast": {Chain: []ModelEndpoint{
				{Provider: "empty", Model: "m"},
				{Provider: "nv2", Model: "m2"},
			}},
		},
	}
	r := NewRouter(cfg, "")
	defer r.Close()

	tried := map[string]bool{}
	r.SkipSameHost(ModelEndpoint{Provider: "nv1", Model: "m"}, tried)

	for _, key := range []string{"nv1:m", "nv1:m-sibling", "nv2:m", "nv3:m", "nv2:m2"} {
		if !tried[key] {
			t.Errorf("%s shares the failing host and must be skipped for this request", key)
		}
	}
	for _, key := range []string{"other:m", "empty:m"} {
		if tried[key] {
			t.Errorf("%s does not share the failing host and must stay eligible", key)
		}
	}
}

// TestRouterSkipSameHostIsANoOpForUnknownProviders keeps the skip from
// panicking or retiring unrelated endpoints when the config has no record of
// the provider (a hot reload can remove one mid-request).
func TestRouterSkipSameHostIsANoOpForUnknownProviders(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"other": {URL: "https://api.kilocode.ai/v1"},
		},
		Models: map[string]ModelConfig{
			"work": {Chain: []ModelEndpoint{{Provider: "other", Model: "m"}}},
		},
	}
	r := NewRouter(cfg, "")
	defer r.Close()

	tried := map[string]bool{}
	r.SkipSameHost(ModelEndpoint{Provider: "gone", Model: "m"}, tried)
	if len(tried) != 0 {
		t.Fatalf("an unknown provider must not skip anything, got %v", tried)
	}
}
