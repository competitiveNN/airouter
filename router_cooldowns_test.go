package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// loadCooldownsTestConfig builds a minimal config with one provider and one
// logical model, enough for NewRouter to construct a router whose cooldown
// file we can point at a temp dir.
func loadCooldownsTestConfig(t *testing.T) *Config {
	t.Helper()
	raw := `
providers:
  test:
    url: https://example.invalid/v1
    api_key_env: TEST_KEY
models:
  smart:
    chain:
      - provider: test
        model: m1
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return &cfg
}

// TestLoadCooldownsSurvivesMalformedCircuit is a regression test for a real
// operational incident: a hand-edited cooldowns.json carried a circuits entry
// with "state":"open" (a string) where CircuitState is an int (router.go
// CircuitState type). The merged-envelope json.Unmarshal is all-or-nothing, so
// that single bad value aborted the parse and dropped EVERY cooldown on disk.
//
// The result was that a manual cooldown edit silently reset all backoff state,
// and the router re-hammered every failing endpoint from a cold start. A bad
// circuit must now cost only that circuit.
func TestLoadCooldownsSurvivesMalformedCircuit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cooldowns.json")

	expiry := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	// "state":"open" is a string where the decoder expects a number — the exact
	// shape that used to wipe the whole file.
	content := `{
  "cooldowns": {
    "test:good-1": {"expiry":"` + expiry + `","status_code":429,"error_count":3,"last_error":"rate limited"},
    "test:good-2": {"expiry":"` + expiry + `","status_code":500,"error_count":2,"last_error":"boom"}
  },
  "circuits": {
    "test:bad-circuit": {"state":"open","opened_at":"` + expiry + `","probes_sent":0}
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cooldowns: %v", err)
	}

	r := NewRouter(loadCooldownsTestConfig(t), path)
	defer r.Close()

	got := r.GetAllCooldowns()
	if len(got) != 2 {
		t.Fatalf("malformed circuit discarded cooldowns: got %d, want 2 (%v)", len(got), keysOf(got))
	}
	for _, k := range []string{"test:good-1", "test:good-2"} {
		if _, ok := got[k]; !ok {
			t.Errorf("cooldown %s lost to a malformed sibling circuit", k)
		}
	}
	// The malformed circuit itself must be dropped rather than half-applied.
	if _, ok := r.GetAllCircuits()["test:bad-circuit"]; ok {
		t.Error("malformed circuit should not have been restored")
	}
}

// TestLoadCooldownsValidFileUnchanged guards the fix: a well-formed file must
// still restore both cooldowns and circuits exactly as before.
func TestLoadCooldownsValidFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cooldowns.json")

	expiry := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	content := `{
  "cooldowns": {
    "test:good-1": {"expiry":"` + expiry + `","status_code":429,"error_count":3,"last_error":"rate limited"}
  },
  "circuits": {
    "test:good-1": {"state":1,"opened_at":"` + expiry + `","probes_sent":0}
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cooldowns: %v", err)
	}

	r := NewRouter(loadCooldownsTestConfig(t), path)
	defer r.Close()

	if got := len(r.GetAllCooldowns()); got != 1 {
		t.Errorf("cooldowns: got %d, want 1", got)
	}
	cb, ok := r.GetAllCircuits()["test:good-1"]
	if !ok {
		t.Fatal("open circuit not restored from valid file")
	}
	if cb.State != CircuitOpen {
		t.Errorf("circuit state: got %v, want CircuitOpen", cb.State)
	}
}

func keysOf(m map[string]CooldownEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- DELETE /admin/cooldowns -------------------------------------------------

// TestAdminCooldownsDeleteOne verifies the operator can clear a single
// endpoint's backoff state without restarting the daemon, which is the whole
// point of the endpoint: hand-editing cooldowns.json under a running router
// gets silently overwritten from its in-memory copy.
func TestAdminCooldownsDeleteOne(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{
		InitialRotationWindow:   &noRotation,
		CircuitBreakerThreshold: intPtr(2),
	}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	bad := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	good := &ModelEndpoint{Provider: "openai", Model: "gpt-4o"}
	for _, ep := range []*ModelEndpoint{bad, good} {
		router.ApplyCooldown(ep, 500, "server error")
		router.ApplyCooldown(ep, 500, "server error")
	}
	if len(router.GetAllCooldowns()) != 2 {
		t.Fatalf("setup: want 2 cooldowns, got %d", len(router.GetAllCooldowns()))
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns?model="+bad.Key(), nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Cleared int    `json:"cleared"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Cleared == 0 {
		t.Error("expected cleared > 0")
	}
	if resp.Model != bad.Key() {
		t.Errorf("model: got %q, want %q", resp.Model, bad.Key())
	}

	// The named endpoint is gone; the untouched one must survive.
	got := router.GetAllCooldowns()
	if _, ok := got[bad.Key()]; ok {
		t.Error("targeted cooldown was not cleared")
	}
	if _, ok := got[good.Key()]; !ok {
		t.Error("clearing one model also cleared an unrelated one")
	}
}

// TestAdminCooldownsDeleteAll covers the bulk path: no model= clears
// everything, which is the operation that most needs to be correct because it
// drops all backoff protection at once.
func TestAdminCooldownsDeleteAll(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{
		InitialRotationWindow:   &noRotation,
		CircuitBreakerThreshold: intPtr(2),
	}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	for _, m := range []string{"gpt-4", "gpt-4o", "o3"} {
		ep := &ModelEndpoint{Provider: "openai", Model: m}
		router.ApplyCooldown(ep, 500, "server error")
		router.ApplyCooldown(ep, 500, "server error")
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(router.GetAllCooldowns()); n != 0 {
		t.Errorf("cooldowns after clear-all: got %d, want 0", n)
	}
	if n := len(router.GetAllCircuits()); n != 0 {
		t.Errorf("circuits after clear-all: got %d, want 0", n)
	}
}

// TestAdminCooldownsDeleteRequiresAuth: the endpoint clears backoff state and
// therefore exposes which endpoints are failing, and it lets an attacker force
// traffic back at a rate-limited provider. It must be behind the same auth as
// the GET it shares a route with.
func TestAdminCooldownsDeleteRequiresAuth(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{InitialRotationWindow: &noRotation}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	ep := &ModelEndpoint{Provider: "openai", Model: "gpt-4"}
	router.ApplyCooldown(ep, 500, "server error")

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns", nil)
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 401 {
		t.Fatalf("unauthenticated DELETE: expected 401, got %d", rec.Code)
	}
	if n := len(router.GetAllCooldowns()); n != 1 {
		t.Errorf("unauthenticated DELETE still cleared state (%d left)", n)
	}
}

// TestAdminCooldownsDeleteUnknownModelIs404 keeps a typo from looking like a
// successful clear — the operator needs to know nothing was reset.
func TestAdminCooldownsDeleteUnknownModelIs404(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Preferences = &Preferences{InitialRotationWindow: &noRotation}
	router := NewRouter(cfg, "")
	gateway := NewGatewayContext(router, NewProxy(cfg), cfg, "", "secret-key")

	req := httptest.NewRequest(http.MethodDelete, "/admin/cooldowns?model=nope%3Anope", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	gateway.HandleAdminCooldowns(rec, req)

	if rec.Code != 404 {
		t.Fatalf("expected 404 for unknown model, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- fallback wall-clock bound -----------------------------------------------

// TestFallbackWallClockBoundIsSane guards the per-request fallback budget.
//
// maxAttempts is chainLen*3+1, which for the shipped config is 82 (smart),
// 145 (work) and 115 (large). At the 5s per-attempt timeout that applies to a
// small request, the loop could in principle run for minutes. A 4-token `smart`
// request was measured taking 62.6s on 2026-09-29, walking 20+ nvidia
// endpoints at 5s apiece. The wall-clock bound caps that.
func TestFallbackWallClockBoundIsSane(t *testing.T) {
	if maxFallbackWallClock <= 0 {
		t.Fatal("maxFallbackWallClock must be positive")
	}
	// A client should never wait longer than this; keep it under a minute so
	// proxies/clients with their own idle timeouts don't cut us off first.
	if maxFallbackWallClock > time.Minute {
		t.Errorf("maxFallbackWallClock = %v, want <= 1m", maxFallbackWallClock)
	}
	// It must still allow more than one retry, otherwise a single transient
	// failure ends the request.
	if maxFallbackWallClock < 2*requestTimeout(0) {
		t.Errorf("maxFallbackWallClock = %v is too tight: it allows fewer than "+
			"2 attempts at the %v per-attempt timeout", maxFallbackWallClock, requestTimeout(0))
	}
}
