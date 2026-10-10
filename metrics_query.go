package main

// DashboardQuery sums the attempt ring over the given window and returns the
// per-endpoint aggregates. It is the single source the three leaderboards are
// derived from, so they cannot disagree about what happened in the window.
//
// The window is inclusive of the current, still-filling tick: a leaderboard
// that silently omitted the last minute would under-report every endpoint
// whenever traffic was ongoing, and the omission would be largest exactly
// when the numbers mattered.
//
// The ring is read under the write lock because the ticker advances it by
// writing to the next slot while holding the same lock. A read lock would
// race with that write: the query could observe a half-advanced ring where
// the current tick is nil but the previous one has not been cleared, or
// vice versa, and the sums would be off by one tick's worth of data. The
// cost is a brief write-lock hold on every dashboard scrape, which is
// negligible — a scrape is a few map reads over at most 168 ticks.
func (m *Metrics) DashboardQuery(w dashboardWindow) []dashboardEndpointAgg {
	m.mu.Lock()
	defer m.mu.Unlock()

	seconds := dashboardWindowSeconds(w)
	if seconds <= 0 {
		return nil
	}

	ticks := seconds / dashboardTickSeconds
	if ticks <= 0 {
		ticks = 1
	}
	// The window includes the current tick, so we walk back ticks-1 from
	// the current position plus the current itself.
	if ticks > dashboardRingLen {
		ticks = dashboardRingLen
	}

	agg := make(map[labelKey]*dashboardEndpointAgg)
	endpoints := make(map[labelKey]string)

	for i := 0; i < ticks; i++ {
		idx := (m.attemptRingNow - i + dashboardRingLen) % dashboardRingLen
		t := m.attemptRing[idx]
		if t == nil {
			continue
		}
		for k, v := range t.attempts {
			a := agg[k]
			if a == nil {
				a = &dashboardEndpointAgg{}
				agg[k] = a
			}
			a.Attempts += v
		}
		for k, v := range t.failures {
			a := agg[k]
			if a == nil {
				a = &dashboardEndpointAgg{}
				agg[k] = a
			}
			a.Failures += v
		}
		for k, v := range t.latencyNS {
			a := agg[k]
			if a == nil {
				a = &dashboardEndpointAgg{}
				agg[k] = a
			}
			a.LatencyNS += v
		}
		for k, v := range t.tokens {
			a := agg[k]
			if a == nil {
				a = &dashboardEndpointAgg{}
				agg[k] = a
			}
			a.Tokens += v
		}
		for k, ep := range t.endpoints {
			if _, exists := endpoints[k]; !exists {
				endpoints[k] = ep
			}
		}
	}

	out := make([]dashboardEndpointAgg, 0, len(agg))
	for k, a := range agg {
		a.Endpoint = endpoints[k]
		if a.Endpoint == "" {
			a.Endpoint = string(k)
		}
		a.Window = string(w)
		out = append(out, *a)
	}
	return out
}

// DashboardChainStatus reports, for each logical model in cfg, how many of its
// configured endpoints are currently eligible. Eligibility is computed by the
// caller (which owns the router) rather than here, because the ring knows
// nothing about cooldowns or circuits.
func DashboardChainStatus(cfg *Config, eligible func(ep ModelEndpoint) bool) []dashboardChainStatus {
	if cfg == nil {
		return nil
	}
	out := make([]dashboardChainStatus, 0, len(cfg.Models))
	for model, mc := range cfg.Models {
		cs := dashboardChainStatus{
			Model:     model,
			Total:     len(mc.Chain),
			Endpoints: make([]dashboardEndpointInfo, 0, len(mc.Chain)),
		}
		for _, ep := range mc.Chain {
			ok := false
			if eligible != nil {
				ok = eligible(ep)
			}
			if ok {
				cs.Operational++
			}
			cs.Endpoints = append(cs.Endpoints, dashboardEndpointInfo{
				Endpoint: ep.Key(),
				Eligible: ok,
			})
		}
		out = append(out, cs)
	}
	return out
}
