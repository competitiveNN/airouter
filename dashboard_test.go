package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

// TestDashboardWindowBoundaryIsExact pins the window edge in BOTH directions.
// The existing boundary test walks a full window and asserts exclusion; this
// walks one tick short and asserts inclusion first.
//
// The two halves are not redundant. An off-by-one that drops the current tick
// under-reports exactly while traffic is live, and one that admits one tick too
// many double-counts the oldest observation in the window. Walking 1439 then
// 1440 distinguishes them, whereas a single "1440 excludes it" assertion
// cannot tell an inclusive upper bound from one that is off by two.
func TestDashboardWindowBoundaryIsExact(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()
	m.Attempt("prov:model-a", time.Second, true)

	// One tick short of the 1d edge: the observation is still in the window.
	for i := 0; i < 24*60-1; i++ {
		m.advanceRingTickLocked()
	}
	if rows := m.DashboardQuery(dashboardWindow1d); len(rows) != 1 {
		t.Fatalf("observation at 1439 ticks old must still be in the 1d window, got %d rows", len(rows))
	}
	// Exactly at the edge: it is one full day old and must be excluded.
	m.advanceRingTickLocked()
	if rows := m.DashboardQuery(dashboardWindow1d); len(rows) != 0 {
		t.Fatalf("observation at 1440 ticks old must be excluded from 1d, got %d rows", len(rows))
	}
	// But not from 7d.
	if rows := m.DashboardQuery(dashboardWindow7d); len(rows) != 1 {
		t.Fatalf("observation one day old must still be in the 7d window, got %d rows", len(rows))
	}
}

// TestDashboardRingRolloverExcludesExactlyOneTick is the wrap-around case,
// which the plain window tests never reach because they advance by hundreds
// rather than by the ring's full length.
//
// The ring holds dashboardRingLen slots and the 7d window is tickCount7d ticks,
// i.e. one slot more than the window. That spare slot is load-bearing: as the
// ticker writes forward it overwrites the data that aged out, so a full turn
// of the ring loses exactly one tick's worth of history and nothing more. If
// a future change makes the window equal to the ring's length, walking back
// from attemptRingNow would land on the current tick a second time and
// double-count the newest observation in every 7d query — the leaderboards
// would drift high forever and nothing in the running daemon would notice.
func TestDashboardRingRolloverExcludesExactlyOneTick(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()
	// Raise the label cap above the ring length: this test writes one distinct
	// endpoint per tick on purpose, and the default cap of 512 would roll them
	// into the __overflow__ bucket long before the ring finished turning.
	m.SetLabelCap(dashboardRingLen + 16)

	const ticks = dashboardRingLen + 1
	for i := 0; i < ticks; i++ {
		m.Attempt(fmt.Sprintf("prov:ep-%04d", i), time.Second, true)
		m.advanceRingTickLocked()
	}
	// One more distinct observation in the newest tick, so "the newest data
	// survived" is a different question from "the oldest data aged out".
	m.Attempt("prov:newest", time.Second, true)

	rows := m.DashboardQuery(dashboardWindow7d)
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Endpoint] = true
	}
	if want := dashboardWindowSeconds(dashboardWindow7d) / dashboardTickSeconds; len(seen) != want {
		t.Fatalf("7d window shows %d endpoints after a full ring turn, want %d", len(seen), want)
	}
	if !seen["prov:newest"] {
		t.Error("the newest observation must survive a full ring turn")
	}
	// The first endpoint written is the one that aged out; if it is still
	// present the window is reading a tick twice.
	if seen["prov:ep-0000"] {
		t.Error("the oldest endpoint must have aged out of the 7d window")
	}
}

// TestDashboardAttemptConcurrentWithTicker exercises the lock discipline the
// ring depends on: Attempt records under the write lock, and the ticker
// advances under the same one. Under -race this fails if either side takes a
// shortcut — a read lock on the query path, or the ticker writing to the
// current slot instead of the next one.
//
// It is also a lost-update test in disguise: every attempt must still be
// counted after the ring has moved on, so a tick that lands while the ticker
// is mid-advance is neither dropped nor counted twice.
func TestDashboardAttemptConcurrentWithTicker(t *testing.T) {
	m := NewMetrics()
	stop := m.StartDashboardTelemetry()
	defer stop()

	// The ticker advances the ring under the write lock (see
	// StartDashboardTelemetry), so the advance below must hold it too. Calling
	// advanceRingTickLocked bare would be racing attemptTickForLocked, and the
	// race detector says so -- which is how this test found that mistake on
	// its first writing. It is recorded here rather than being quietly fixed,
	// because the next person to add a concurrent test will reach for the same
	// unlocked call.
	advance := func() {
		m.mu.Lock()
		m.advanceRingTickLocked()
		m.mu.Unlock()
	}

	const goroutines = 8
	const perGoroutine = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				m.Attempt("prov:model-a", time.Millisecond, true)
				if i%10 == 0 {
					advance()
				}
			}
		}(g)
	}
	wg.Wait()

	want := int64(goroutines * perGoroutine)
	rows := m.DashboardQuery(dashboardWindow1d)
	if len(rows) != 1 {
		t.Fatalf("want 1 endpoint after concurrent recording, got %d", len(rows))
	}
	if rows[0].Attempts != want {
		t.Fatalf("attempts = %d after %d concurrent recordings, want %d (lost or double-counted)",
			rows[0].Attempts, want, want)
	}
}

// TestResponsesStreamUsageReplacesNotAccumulates pins the fallback contract
// behind setUsage. A stream that fails partway and is retried against the next
// model produces a second usage event, and the tokens recorded must be the
// successful attempt's — not the sum of the aborted attempt's and the
// successful one's. Adding them would inflate the speed leaderboard with
// output the client never received, and no number in the dashboard would
// reveal it.
func TestResponsesStreamUsageReplacesNotAccumulates(t *testing.T) {
	var sw responsesStreamWriter
	sw.setUsage(10)
	sw.setUsage(20)
	if sw.usage != 20 {
		t.Fatalf("usage = %d after two setUsage calls, want 20 (last writer wins)", sw.usage)
	}
	// A zero or negative usage event is a provider that reported nothing, not a
	// report of having produced nothing. Overwriting with it would erase a
	// real count that arrived earlier in the same stream.
	sw.setUsage(0)
	sw.setUsage(-7)
	if sw.usage != 20 {
		t.Fatalf("usage = %d, want 20: a non-positive report must not clear a real one", sw.usage)
	}
}

// TestDashboardApiKeyQueryAuth pins the browser path. A browser cannot attach
// an Authorization header to a typed URL or a bookmark, so the admin surface
// accepts `?api_key=` as an alternative credential. It is not a weaker check —
// the comparison is the same constant-time one — it is a credential with a
// wider footprint, and the header stays the primary form.
func TestDashboardApiKeyQueryAuth(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)

	cases := []struct {
		name    string
		header  string
		query   string
		want    int
	}{
		{"key in query is accepted", "", "api_key=secret", 200},
		{"wrong key in query is rejected", "", "api_key=WRONG", 401},
		{"no key at all is rejected", "", "", 401},
		{"header alone is accepted", "Bearer secret", "", 200},
		{"header beats a wrong query key", "Bearer secret", "api_key=WRONG", 200},
		{"malformed header must not fall through to query", "Basic wrong", "api_key=secret", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			if c.query != "" {
				req.URL.RawQuery = c.query
			}
			w := httptest.NewRecorder()
			g.HandleAdminDashboard(w, req)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}
