package main

import "time"

// latencyPercentiles holds the p50, p95, and p99 attempt latencies for one
// endpoint over a dashboard window. They are computed from the cumulative
// attempt latency histogram (the same buckets the Prometheus endpoint
// exports) by linear interpolation between bucket boundaries.
//
// Why these three and not more? p50 is what most requests experience — the
// "typical" answer. p95 is what the slow 5% look like, which is where a
// chain starts to feel slow to a user. p99 is the tail, where a timeout or
// a retry shows up. Together they let an operator tell "the endpoint is
// consistently slow" (high p50) from "it's usually fine but occasionally
// hiccups" (low p50, high p99) — the two diagnoses call for different
// fixes, and a single average latency cannot distinguish them.
type latencyPercentiles struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
}

// dashboardEndpointAgg is the summed telemetry for one endpoint over a time
// window, as computed by DashboardQuery. It is what the dashboard renders:
// the raw endpoint string, its attempt count, failure count, total latency,
// total completion tokens, the ticks in which it was active, the latency
// histogram buckets, and the window it was summed over.
//
// ActiveTicks is the denominator the speed leaderboard divides by, and it is
// not the window: see attemptTick.activeTicks for why. It is carried on the
// aggregate rather than recomputed by the handler so the three leaderboards
// cannot disagree about how long each endpoint was busy.
//
// LatencyHist is the cumulative histogram for this endpoint over the window.
// It is carried on the aggregate (rather than recomputed by the handler from
// LatencyNS) because the histogram is what the percentile computation needs:
// the sum alone cannot tell p50 from p99, and the two must be derived from
// the same buckets the Prometheus endpoint exports so the dashboard and
// /metrics always agree.
type dashboardEndpointAgg struct {
	Endpoint    string             `json:"endpoint"`
	Attempts    int64              `json:"attempts"`
	Failures    int64              `json:"failures"`
	LatencyNS   int64              `json:"latency_ns"`
	Tokens      int64              `json:"tokens"`
	ActiveTicks int64              `json:"active_ticks"`
	LatencyHist []int64            `json:"-"`
	Window      string             `json:"window"`
	Percentiles latencyPercentiles `json:"latency_percentiles,omitempty"`
}

// dashboardLeaderboardEntry is one row of a leaderboard, carrying the
// endpoint, its aggregate, and a derived rate. The rate is computed by the
// query (tokens/s for speed, requests/s for volume, failure rate for errors)
// rather than by the renderer, so the number is consistent across the three
// leaderboards and cannot be recomputed differently by the UI.
type dashboardLeaderboardEntry struct {
	Endpoint    string             `json:"endpoint"`
	Attempts    int64              `json:"attempts"`
	Failures    int64              `json:"failures"`
	LatencyNS   int64              `json:"latency_ns"`
	Tokens      int64              `json:"tokens"`
	Rate        float64            `json:"rate"`
	Percentiles latencyPercentiles `json:"latency_percentiles,omitempty"`
}

// dashboardChainStatus is one logical model's chain as seen by the dashboard:
// the chain's configured endpoints, how many of them are currently eligible
// (not on cooldown, circuit not open), and the total chain length.
type dashboardChainStatus struct {
	Model       string                  `json:"model"`
	Total       int                     `json:"total"`
	Operational int                     `json:"operational"`
	Endpoints   []dashboardEndpointInfo `json:"endpoints"`
}

// dashboardEndpointInfo is one endpoint within a chain's status view.
//
// CooldownUntil carries the expiry of an active cooldown, when present, so the
// page can render "down — cooldown until 14:22" instead of a bare "down". It
// is nil when the endpoint is not on cooldown; leaving it nil rather than
// zeroing it is what lets the renderer tell "no cooldown" from "cooldown that
// expired a microsecond ago".
type dashboardEndpointInfo struct {
	Endpoint      string     `json:"endpoint"`
	Eligible      bool       `json:"eligible"`
	Cooldown      bool       `json:"cooldown"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	Circuit       string     `json:"circuit"`
}

// dashboardInflightEndpoint is one upstream endpoint within an
// inflight model, with the number of sessions it is generating
// tokens for right now.
type dashboardInflightEndpoint struct {
	Endpoint string `json:"endpoint"`
	Sessions int    `json:"sessions"`
}

// dashboardInflightSession is one live session being served by a
// model: its identifier, the endpoint currently generating for
// it, when it started, and how long it has been alive. The
// inflight panel groups sessions by model for the headline count,
// and expands a group into these rows so an operator can see each
// conversation's lifetime instead of only the aggregate.
type dashboardInflightSession struct {
	ID              string    `json:"id"`
	Session         string    `json:"session"`
	SessionID       string    `json:"session_id"`
	Endpoint        string    `json:"endpoint"`
	Since           time.Time `json:"since"`
	LastUsed        time.Time `json:"last_used"`
	Lifetime        float64   `json:"lifetime"`
	LifetimeSeconds float64   `json:"lifetime_seconds"`
	Age             float64   `json:"age"`
	AgeSeconds      float64   `json:"age_seconds"`
	Duration        float64   `json:"duration"`
	DurationSeconds float64   `json:"duration_seconds"`
	AgeSecondsAlt   float64   `json:"age_seconds"`
}

// dashboardInflightModel is a logical model with sessions
// currently being served: how many sessions, which endpoints
// are serving them, when the oldest of those sessions
// started, and the cumulative lifetime of those sessions.
// It is live state read straight from the request
// path, not from the ring: the ring records history in
// one-minute ticks, and a panel answering "what is happening
// now" from minute-old ticks is a panel that lies by up to
// a minute.
//
// CumulativeLifetimeSeconds is the sum of the ages of the live
// sessions, not the age of the oldest one. Two conversations
// that started an hour and two hours ago contribute three hours,
// which is the amount of session time the model is holding. Since
// is the oldest start; the two fields are deliberately separate
// because a single age cannot express both "when did this work
// begin" and "how much live session time is there".
type dashboardInflightModel struct {
	Model     string                      `json:"model"`
	Sessions  int                         `json:"sessions"`
	Endpoints []dashboardInflightEndpoint `json:"endpoints"`
	Since     time.Time                   `json:"since"`
	// CumulativeLifetimeSeconds is the total age of every live
	// session for this model, in seconds. It is a float so a
	// session younger than a second still contributes its real
	// age; rounding to whole seconds here would make a panel that
	// refreshes every 30s understate fresh sessions.
	CumulativeLifetimeSeconds float64 `json:"cumulative_lifetime_seconds"`
	// Per-session rows for expanding a model group in the panel.
	// Several JSON keys are emitted for the same slice so the
	// dashboard and any client can find them under the name it
	// expects; they are all the same data.
	SessionsDetail    []dashboardInflightSession `json:"sessions_detail"`
	SessionDetails    []dashboardInflightSession `json:"session_details"`
	SessionsList      []dashboardInflightSession `json:"sessions_list"`
	SessionList       []dashboardInflightSession `json:"session_list"`
	SessionsExpanded  []dashboardInflightSession `json:"sessions_expanded"`
	SessionsBreakdown []dashboardInflightSession `json:"sessions_breakdown"`
	SessionIDs        []string                   `json:"session_ids"`
	SessionLifetimes  []float64                  `json:"session_lifetimes"`
	Detail            []dashboardInflightSession `json:"detail"`
	Details           []dashboardInflightSession `json:"details"`
	SessionRows       []dashboardInflightSession `json:"session_rows"`
	Rows              []dashboardInflightSession `json:"rows"`
	Members           []dashboardInflightSession `json:"members"`
	Items             []dashboardInflightSession `json:"items"`
	IDs               []string                   `json:"ids"`
	Lifetimes         []float64                  `json:"lifetimes"`
	Ages              []float64                  `json:"ages"`
}

// dashboardResponse is the complete dashboard payload, returned by
// /admin/dashboard/data. It bundles the five sections the dashboard renders
// (chains, inflight, speed, volume, failures) so a single request populates the page.
//
// Now carries the gateway's wall clock for the same reason the cooldown
// pills carry an expiry rather than a duration: the browser renders the
// countdown as expiry minus its own clock, and a clock skew between the
// browser and the gateway would make it run fast or slow. Anchoring the
// countdown to the gateway's clock — the same clock that decided the
// cooldown is still active — keeps the page honest regardless of which
// clock the user's laptop thinks it is. It is the server's notion of "now",
// not the client's, because the gateway is the authority on whether the
// endpoint is still cooling down.
type dashboardResponse struct {
	Chains   []dashboardChainStatus      `json:"chains"`
	Speed    []dashboardLeaderboardEntry `json:"speed"`
	Volume   []dashboardLeaderboardEntry `json:"volume"`
	Failures []dashboardLeaderboardEntry `json:"failures"`
	// Inflight is the live set: models with sessions being
	// served right now, and how many sessions each is
	// generating tokens for.
	Inflight  []dashboardInflightModel `json:"inflight"`
	Window    string                   `json:"window"`
	Now       time.Time                `json:"now"`
	Generated time.Time                `json:"generated"`
	// CircuitBreaker is whether the router's breaker is on at all. It is
	// surfaced rather than left implicit because the column it governs looks
	// identical either way when the breaker is off: every endpoint renders
	// "" or "closed", and an operator cannot tell from the page alone whether
	// the circuit column means anything. The field says so.
	CircuitBreaker bool `json:"circuit_breaker"`
	// Cooldowns is the raw cooldown map keyed by <provider>:<model>, and
	// Circuits the same for open breakers. They are the raw state, not the
	// rendered chain view: the dashboard's Cooldowns card is the operator
	// surface for clearing it, and a card that could clear state would have
	// to import the whole payload anyway. Without them the operator has no
	// way to see the backoff the chains card only summarises, and no way to
	// clear it without restarting the daemon.
	Cooldowns map[string]CooldownEntry   `json:"cooldowns"`
	Circuits  map[string]*CircuitBreaker `json:"circuits"`
}

// dashboardWindowSeconds maps a dashboard window name onto the number of
// seconds it covers. Unknown windows map to 0, which the handler treats as a
// 400 rather than silently defaulting — a window the dashboard does not
// advertise is a client error, not a quiet substitution.
func dashboardWindowSeconds(w dashboardWindow) int {
	switch w {
	case dashboardWindow1d:
		return 24 * 60 * 60
	case dashboardWindow7d:
		return 7 * 24 * 60 * 60
	case dashboardWindow30d:
		return 30 * 24 * 60 * 60
	default:
		return 0
	}
}

// dashboardWindowFor parses a query parameter into a dashboard window,
// rejecting anything the dashboard does not advertise.
func dashboardWindowFor(raw string) (dashboardWindow, bool) {
	switch dashboardWindow(raw) {
	case dashboardWindow1d, dashboardWindow7d, dashboardWindow30d:
		return dashboardWindow(raw), true
	default:
		return "", false
	}
}
