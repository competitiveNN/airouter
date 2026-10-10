package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDashboardQuerySumsOnlyTheWindow is the core correctness property of the
// dashboard leaderboards: a tick older than the window must not contribute,
// and a tick inside it must.
//
// It fails if the window arithmetic is off by one in either direction, which
// is the classic ring bug: an off-by-one that omits the current tick
// under-reports exactly while traffic is live, and one that includes one tick
// too many double-counts the oldest tick in the window.
func TestDashboardQuerySumsOnlyTheWindow(t *testing.T) {
	m := NewMetrics()
	// Bring the ticker up so attemptRingStarted is set, then advance the ring
	// by hand. Attempts recorded via Attempt always land in the CURRENT tick,
	// so to place observations in specific ticks we drive the ring directly.
	stop := m.StartDashboardTelemetry()
	defer stop()

	// A 1d window covers 1440 ticks. Record into tick 0 (current), advance
	// 1440 times so tick 0 is now ONE past the window edge, and confirm its
	// data is excluded while a fresh observation is included.
	const ep = "prov:model-a"
	m.Attempt(ep, time.Second, true)
	if got := len(m.DashboardQuery(dashboardWindow1d)); got != 1 {
		t.Fatalf("fresh observation should appear in the 1d window, got %d rows", got)
	}
	// Walk forward exactly one window's worth of ticks. The observation is now
	// one full day old.
	for i := 0; i < 24*60; i++ {
		m.advanceRingTickLocked()
	}
	if rows := m.DashboardQuery(dashboardWindow1d); len(rows) != 0 {
		t.Fatalf("observation older than the window must be excluded, got %d rows", len(rows))
	}
	// And a 7d window still sees it.
	if rows := m.DashboardQuery(dashboardWindow7d); len(rows) != 1 {
		t.Fatalf("observation one day old must still appear in the 7d window, got %d rows", len(rows))
	}
}

// TestDashboardQueryIncludesCurrentTick pins the inclusive upper bound: the
// still-filling current tick counts toward the window. Excluding it would
// under-report every endpoint whenever traffic was ongoing, which is exactly
// when the numbers matter.
func TestDashboardQueryIncludesCurrentTick(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()
	for i := 0; i < 5; i++ {
		m.Attempt("prov:model-a", 100*time.Millisecond, true)
	}
	rows := m.DashboardQuery(dashboardWindow1d)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Attempts != 5 {
		t.Fatalf("attempts = %d, want 5", rows[0].Attempts)
	}
	if rows[0].Endpoint != "prov:model-a" {
		t.Fatalf("endpoint = %q, want the raw string not the label key", rows[0].Endpoint)
	}
}

// TestDashboardQueryAggregatesAcrossTicks checks that observations in
// different ticks of the same window are summed, not overwritten, and that
// the endpoint string survives the aggregation.
func TestDashboardQueryAggregatesAcrossTicks(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()

	m.Attempt("prov:a", 100*time.Millisecond, true)
	m.Attempt("prov:b", 100*time.Millisecond, false)
	m.advanceRingTickLocked()
	m.Attempt("prov:a", 200*time.Millisecond, false)
	m.RecordDashboardTokens("prov:a", 42)

	rows := m.DashboardQuery(dashboardWindow1d)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	byEp := map[string]dashboardEndpointAgg{}
	for _, r := range rows {
		byEp[r.Endpoint] = r
	}
	a := byEp["prov:a"]
	if a.Attempts != 2 || a.Failures != 1 || a.Tokens != 42 {
		t.Fatalf("prov:a agg = %+v, want attempts 2 failures 1 tokens 42", a)
	}
	if a.LatencyNS != int64(300*time.Millisecond) {
		t.Fatalf("prov:a latency = %d ns, want %d", a.LatencyNS, int64(300*time.Millisecond))
	}
	b := byEp["prov:b"]
	if b.Attempts != 1 || b.Failures != 1 || b.Tokens != 0 {
		t.Fatalf("prov:b agg = %+v, want attempts 1 failures 1 tokens 0", b)
	}
}

// TestDashboardQueryUnknownWindowIsEmpty guards the contract that an
// unadvertised window yields no data rather than a silent default. The
// handler rejects it as a 400, but the query itself must not fall back to a
// real window if that check is ever bypassed.
func TestDashboardQueryUnknownWindowIsEmpty(t *testing.T) {
	m := NewMetrics()
	m.Attempt("prov:a", time.Millisecond, true)
	if rows := m.DashboardQuery("99d"); rows != nil {
		t.Fatalf("unknown window must return nil, got %d rows", len(rows))
	}
}

// TestRecordDashboardTokensRejectsNonPositive pins the guard: a zero or
// negative token count is a provider that reported nothing (or reported
// nonsense), and writing it would create a zero-valued row that dilutes the
// leaderboard.
func TestRecordDashboardTokensRejectsNonPositive(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()
	m.Attempt("prov:a", time.Millisecond, true)
	m.RecordDashboardTokens("prov:a", 0)
	m.RecordDashboardTokens("prov:a", -5)
	m.RecordDashboardTokens("", 100)

	rows := m.DashboardQuery(dashboardWindow1d)
	if len(rows) != 1 || rows[0].Tokens != 0 {
		t.Fatalf("tokens = %d, want 0 after rejected writes", rows[0].Tokens)
	}
}

// TestDashboardChainStatusCountsEligible checks the chain summary the
// dashboard renders as "operational/total", including the nil-callback case
// (no eligibility function means nothing is eligible, not a panic).
func TestDashboardChainStatusCountsEligible(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{
		"smart": {Chain: []ModelEndpoint{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
			{Provider: "p3", Model: "m3"},
		}},
	}}

	eligible := func(ep ModelEndpoint) bool { return ep.Provider != "p2" }
	chains := DashboardChainStatus(cfg, eligible)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d", len(chains))
	}
	c := chains[0]
	if c.Model != "smart" || c.Total != 3 || c.Operational != 2 {
		t.Fatalf("chain = %+v, want smart 3 total 2 operational", c)
	}
	if len(c.Endpoints) != 3 {
		t.Fatalf("want 3 endpoints, got %d", len(c.Endpoints))
	}
	if c.Endpoints[0].Endpoint != "p1:m1" || !c.Endpoints[0].Eligible {
		t.Fatalf("endpoints[0] = %+v, want p1:m1 eligible", c.Endpoints[0])
	}
	if c.Endpoints[1].Eligible {
		t.Fatal("p2:m2 must be ineligible")
	}

	if got := DashboardChainStatus(cfg, nil); len(got) != 1 || got[0].Operational != 0 {
		t.Fatalf("nil eligible must report 0 operational, got %+v", got)
	}
	if got := DashboardChainStatus(nil, eligible); got != nil {
		t.Fatal("nil config must return nil")
	}
}

// TestDashboardWindowForRejectsUnknown pins the two advertised windows and
// the rejection of everything else, which is what keeps the handler from
// silently answering a question the dashboard never asked.
func TestDashboardWindowForRejectsUnknown(t *testing.T) {
	for _, ok := range []dashboardWindow{dashboardWindow1d, dashboardWindow7d} {
		got, valid := dashboardWindowFor(string(ok))
		if !valid || got != ok {
			t.Fatalf("dashboardWindowFor(%q) = %q,%v; want accepted", ok, got, valid)
		}
	}
	for _, bad := range []string{"", "1h", "30d", "1D", "d1"} {
		if _, valid := dashboardWindowFor(bad); valid {
			t.Fatalf("dashboardWindowFor(%q) must be rejected", bad)
		}
	}
	if dashboardWindowSeconds(dashboardWindow7d) != 7*24*60*60 {
		t.Fatalf("7d window = %d s", dashboardWindowSeconds(dashboardWindow7d))
	}
}

// dashboardTestGateway builds a gateway with a configured chain so the data
// endpoint can be exercised without a live upstream.
func dashboardTestGateway(t *testing.T, cfg *Config) *GatewayContext {
	t.Helper()
	router := NewRouter(cfg, t.TempDir()+"/cooldowns.json")
	proxy := NewProxy(cfg)
	router.SetMetrics(nil)
	g := NewGatewayContext(router, proxy, cfg, "config.yaml", "secret")
	return g
}

// TestHandleAdminDashboardDataRejectsUnknownWindow checks the handler's
// validation: an unadvertised window is a 400, not a silent substitution.
func TestHandleAdminDashboardDataRejectsUnknownWindow(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{
		"smart": {Chain: []ModelEndpoint{{Provider: "p", Model: "m"}}},
	}}
	g := dashboardTestGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/data?window=99d", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	g.HandleAdminDashboardData(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown window: status = %d, want 400", w.Code)
	}
}

// TestHandleAdminDashboardDataServesPayload exercises the full path the page
// uses: auth, chain status, and the three leaderboards, all over the same
// window. It is the integration point where a nil metric or a broken window
// would surface.
func TestHandleAdminDashboardDataServesPayload(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{
		"work": {Chain: []ModelEndpoint{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		}},
	}}
	g := dashboardTestGateway(t, cfg)
	stop := g.metrics.StartDashboardTelemetry()
	defer stop()
	g.metrics.Attempt("p1:m1", 100*time.Millisecond, true)
	g.metrics.Attempt("p2:m2", 100*time.Millisecond, false)
	g.metrics.RecordDashboardTokens("p1:m1", 500)

	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/data?window=1d", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	g.HandleAdminDashboardData(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	var resp dashboardResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if resp.Window != "1d" {
		t.Fatalf("window = %q, want 1d", resp.Window)
	}
	if len(resp.Chains) != 1 || resp.Chains[0].Total != 2 {
		t.Fatalf("chains = %+v, want one chain of 2", resp.Chains)
	}
	// Speed is sorted by tokens/s, so the only endpoint with tokens leads.
	if len(resp.Speed) == 0 || resp.Speed[0].Endpoint != "p1:m1" {
		t.Fatalf("speed leaderboard = %+v, want p1:m1 first", resp.Speed)
	}
	// Failures is sorted by failure RATE, so the endpoint that failed leads.
	if resp.Failures[0].Endpoint != "p2:m2" {
		t.Fatalf("failures leaderboard = %+v, want p2:m2 first", resp.Failures)
	}
}

// TestHandleAdminDashboardDataRequiresAuth guards the admin surface: the
// chain topology and per-endpoint failure data is operator telemetry.
func TestHandleAdminDashboardDataRequiresAuth(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/data?window=1d", nil)
	w := httptest.NewRecorder()
	g.HandleAdminDashboardData(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a key", w.Code)
	}
}

// TestDashboardRingIsBounded is the memory-bound guarantee: the ring holds
// a fixed number of slots regardless of process lifetime, and advancing it
// more steps than its length wraps rather than growing or panicking.
func TestDashboardRingIsBounded(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()
	before := len(m.attemptRing)
	for i := 0; i < dashboardRingLen+10; i++ {
		m.advanceRingTickLocked()
	}
	if len(m.attemptRing) != before {
		t.Fatalf("ring length grew from %d to %d", before, len(m.attemptRing))
	}
	if m.attemptRingNow >= len(m.attemptRing) {
		t.Fatalf("ring index %d out of range %d", m.attemptRingNow, len(m.attemptRing))
	}
}

// TestStartDashboardTelemetryIsIdempotent pins the once-only guarantee: a
// second call must not start a second ticker, which would advance the ring
// twice as fast and silently halve every reported window.
func TestStartDashboardTelemetryIsIdempotent(t *testing.T) {
	m := NewMetrics()
	stop1 := m.StartDashboardTelemetry()
	stop2 := m.StartDashboardTelemetry()
	defer stop1()
	defer stop2()
	if !m.attemptRingStarted {
		t.Fatal("attemptRingStarted must be set after the first call")
	}
}

// TestDashboardHandlerServesHTML checks the page itself: auth, content type,
// and the safety headers the audit flagged. The body must carry the sections
// the page promises.
func TestDashboardHandlerServesHTML(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	g.HandleAdminDashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	body := w.Body.String()
	for _, want := range []string{"/admin/dashboard/data", "Fastest", "Chains", "failure rate"} {
		if !contains(body, want) {
			t.Errorf("dashboard page missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
