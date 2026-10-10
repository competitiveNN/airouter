package main

import "time"

// dashboardEndpointAgg is the summed telemetry for one endpoint over a time
// window, as computed by DashboardQuery. It is what the dashboard renders:
// the raw endpoint string, its attempt count, failure count, total latency,
// total completion tokens, the ticks in which it was active, and the window
// it was summed over.
//
// ActiveTicks is the denominator the speed leaderboard divides by, and it is
// not the window: see attemptTick.activeTicks for why. It is carried on the
// aggregate rather than recomputed by the handler so the three leaderboards
// cannot disagree about how long each endpoint was busy.
type dashboardEndpointAgg struct {
	Endpoint    string `json:"endpoint"`
	Attempts    int64  `json:"attempts"`
	Failures    int64  `json:"failures"`
	LatencyNS   int64  `json:"latency_ns"`
	Tokens      int64  `json:"tokens"`
	ActiveTicks int64  `json:"active_ticks"`
	Window      string `json:"window"`
}

// dashboardLeaderboardEntry is one row of a leaderboard, carrying the
// endpoint, its aggregate, and a derived rate. The rate is computed by the
// query (tokens/s for speed, requests/s for volume, failure rate for errors)
// rather than by the renderer, so the number is consistent across the three
// leaderboards and cannot be recomputed differently by the UI.
type dashboardLeaderboardEntry struct {
	Endpoint  string  `json:"endpoint"`
	Attempts  int64   `json:"attempts"`
	Failures  int64   `json:"failures"`
	LatencyNS int64   `json:"latency_ns"`
	Tokens    int64   `json:"tokens"`
	Rate      float64 `json:"rate"`
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

// dashboardResponse is the complete dashboard payload, returned by
// /admin/dashboard/data. It bundles the four sections the dashboard renders
// (chains, speed, volume, failures) so a single request populates the page.
type dashboardResponse struct {
	Chains    []dashboardChainStatus      `json:"chains"`
	Speed     []dashboardLeaderboardEntry `json:"speed"`
	Volume    []dashboardLeaderboardEntry `json:"volume"`
	Failures  []dashboardLeaderboardEntry `json:"failures"`
	Window    string                      `json:"window"`
	Generated time.Time                   `json:"generated"`
}

// dashboardWindowSeconds maps a dashboard window name onto the number of
// ticks it covers. Unknown windows map to 0, which the handler treats as a
// 400 rather than silently defaulting — a window the dashboard does not
// advertise is a client error, not a quiet substitution.
func dashboardWindowSeconds(w dashboardWindow) int {
	switch w {
	case dashboardWindow1d:
		return 24 * 60 * 60
	case dashboardWindow7d:
		return 7 * 24 * 60 * 60
	default:
		return 0
	}
}

// dashboardWindowFor parses a query parameter into a dashboard window,
// rejecting anything the dashboard does not advertise.
func dashboardWindowFor(raw string) (dashboardWindow, bool) {
	switch dashboardWindow(raw) {
	case dashboardWindow1d, dashboardWindow7d:
		return dashboardWindow(raw), true
	default:
		return "", false
	}
}
