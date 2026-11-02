package main

import "testing"

// TestIsRetryableForChainClassifies5xxRetryable pins which HTTP statuses
// isRetryableForChain lets advance the fallback chain.
//
// A 5xx is a transient upstream failure: the endpoint answered, but it did not
// serve the request, so the next chain entry may still answer. If the 5xx
// branch is dropped or neutered, a server error holds the client until the
// wall-clock request budget expires instead of failing over to a healthy model.
// That defect is indistinguishable from a slow provider in a chain with a
// single fallback, but it is unambiguous to this classification.
//
// 400/401/403/404 are deliberately non-retryable: the request is unacceptable
// or the endpoint is misconfigured, so re-sending it elsewhere buys nothing.
func TestIsRetryableForChainClassifies5xxRetryable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"transport error has no status", 0, "", true},
		{"429 rate limit", 429, "", true},
		{"500 internal server error", 500, "", true},
		{"502 bad gateway", 502, "", true},
		{"503 service unavailable", 503, "", true},
		{"599 upper 5xx boundary", 599, "", true},
		{"400 bad request", 400, "", false},
		{"401 unauthorized", 401, "", false},
		{"403 forbidden", 403, "", false},
		{"404 not found", 404, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryableForChain(c.status, c.body); got != c.want {
				t.Fatalf("isRetryableForChain(%d, %q) = %v, want %v",
					c.status, c.body, got, c.want)
			}
		})
	}
}
