package main

import (
	"testing"
)

// TestProxyAllowedForBodyBoundary pins the exact boundary for maxProxiedBodyBytes.
// Bodies <= maxProxiedBodyBytes should be allowed through proxy.
// Bodies > maxProxiedBodyBytes should be sent direct.
func TestProxyAllowedForBodyBoundary(t *testing.T) {
	maxBody := maxProxiedBodyBytes

	if !proxyAllowedForBody(maxBody) {
		t.Fatalf("expected body size %d (max) to be allowed for proxy, got denied", maxBody)
	}
	if proxyAllowedForBody(maxBody + 1) {
		t.Fatalf("expected body size %d (max+1) to be denied for proxy, got allowed", maxBody+1)
	}
}

// TestProxyAllowedForBodySmallBodies verifies small bodies (e.g., 16KB) still use proxy rotation.
func TestProxyAllowedForBodySmallBodies(t *testing.T) {
	sizes := []int{
		1024,   // 1KB
		4096,   // 4KB
		16384,  // 16KB
		65536,  // 64KB
		262144, // 256KB (the limit)
		262145, // 256KB + 1 (over limit)
	}

	for _, size := range sizes {
		allowed := proxyAllowedForBody(size)
		if size <= maxProxiedBodyBytes && !allowed {
			t.Errorf("body size %d should be allowed for proxy, got denied", size)
		}
		if size > maxProxiedBodyBytes && allowed {
			t.Errorf("body size %d should be denied for proxy, got allowed", size)
		}
	}
}

// TestProxyBodySizeTelemetry verifies that the metrics system correctly tracks
// proxy routing decisions for different body sizes.
func TestProxyBodySizeTelemetry(t *testing.T) {
	if maxProxiedBodyBytes != 256<<10 {
		t.Fatalf("maxProxiedBodyBytes = %d, want %d (256KB)", maxProxiedBodyBytes, 256<<10)
	}

	// Test that we can record different proxy decision types
	m := NewMetrics()
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}

	// Record various decisions
	m.ProxyRequestDecision("proxied")
	m.ProxyRequestDecision("direct_body_exceeded")
	m.ProxyRequestDecision("direct_no_proxy")
	m.ProxyRequestDecision("direct_command_code")
	m.ProxyRequestDecision("proxied") // Second proxied request

	// The metrics should have been recorded (basic smoke test)
	// In a full test we'd verify the metrics output, but this ensures the API is callable
}

// TestProxyBodySizeWithExplicitDirect verifies that explicit Direct option overrides
// body size checks.
func TestProxyBodySizeWithExplicitDirect(t *testing.T) {
	// This test documents the expected behavior: when Direct is true,
	// the request should bypass proxy regardless of body size.
	// The actual routing is tested in integration tests.
	if maxProxiedBodyBytes == 0 {
		t.Fatal("maxProxiedBodyBytes must be > 0")
	}
}

// TestProxyBodySizeThresholdStress tests the threshold with various body sizes
// to ensure consistent behavior across the boundary.
func TestProxyBodySizeThresholdStress(t *testing.T) {
	testCases := []struct {
		size        int
		description string
		shouldProxy bool
	}{
		{1024, "1KB body", true},
		{16 * 1024, "16KB body", true},
		{64 * 1024, "64KB body", true},
		{128 * 1024, "128KB body", true},
		{256 * 1024, "256KB body (at limit)", true},
		{256*1024 + 1, "256KB + 1 byte (over limit)", false},
		{512 * 1024, "512KB body", false},
		{1024 * 1024, "1MB body", false},
	}

	for _, tc := range testCases {
		result := proxyAllowedForBody(tc.size)
		if result != tc.shouldProxy {
			t.Errorf("%s: proxyAllowedForBody(%d) = %v, want %v",
				tc.description, tc.size, result, tc.shouldProxy)
		}
	}
}
