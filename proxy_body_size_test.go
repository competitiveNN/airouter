package main

import "testing"

// TestProxyAllowedForBody pins the cut-off for sending a request through the
// public proxy pool. A free proxy that cannot carry a prefill-sized upload
// burns the attempt budget; direct works (measured: 134KB in ~1s, 600KB in
// ~3s to kilocode).
func TestProxyAllowedForBody(t *testing.T) {
	if !proxyAllowedForBody(16 << 10) {
		t.Fatal("small requests keep the proxy rotation")
	}
	if proxyAllowedForBody(maxProxiedBodyBytes + 1) {
		t.Fatal("a body above the limit must go direct")
	}
	if !proxyAllowedForBody(maxProxiedBodyBytes) {
		t.Fatal("the boundary itself is allowed")
	}
}
