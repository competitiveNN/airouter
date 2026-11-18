package main

import "time"

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
// vice versa, and the sums would be off by one tick's worth of data. The lock
// is held only long enough to copy the current tick (the only one still being
// mutated) and to capture pointers to the completed ticks; aggregation then
// runs outside the lock so a dashboard scrape cannot stall the request path.
//
// The latency histogram is summed across ticks alongside the counters, and
// the p50/p95/p99 percentiles are computed from the cumulative distribution
// by percentileFromHistogram. They are carried on the aggregate so the
// dashboard can render "typical latency" and "tail latency" without the
// handler having to re-derive them, and so the three leaderboards cannot
// disagree about what a percentile means.
func (m *Metrics) DashboardQuery(w dashboardWindow) []dashboardEndpointAgg {
	seconds := dashboardWindowSeconds(w)
	if seconds <= 0 {
		return nil
	}

	ticks := seconds / dashboardTickSeconds
	if ticks <= 0 {
		ticks = 1
	}
	if ticks > dashboardRingLen {
		ticks = dashboardRingLen
	}

	// Snapshot the ring under a brief lock, then aggregate OUTSIDE the lock
	// so a dashboard scrape cannot stall behind (or stall) the ticker and
	// other writers. Completed ticks are never mutated after the ring advances
	// past them, so their pointers can be read outside the lock. The CURRENT
	// tick is still receiving observations, so it must be deep-copied under
	// the lock; sharing it would race the request path's map writes.
	m.mu.Lock()
	now := m.attemptRingNow
	snap := make([]*attemptTick, ticks)
	for i := 0; i < ticks; i++ {
		idx := (now - i + dashboardRingLen) % dashboardRingLen
		snap[i] = m.attemptRing[idx]
	}
	if snap[0] != nil {
		snap[0] = snap[0].clone()
	}
	m.mu.Unlock()

	agg := make(map[labelKey]*dashboardEndpointAgg)
	endpoints := make(map[labelKey]string)

	for i := 0; i < ticks; i++ {
		t := snap[i]
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
		for k, v := range t.activeTicks {
			a := agg[k]
			if a == nil {
				a = &dashboardEndpointAgg{}
				agg[k] = a
			}
			a.ActiveTicks += v
		}
		for k, v := range t.hist {
			a := agg[k]
			if a == nil {
				a = &dashboardEndpointAgg{}
				agg[k] = a
			}
			if a.LatencyHist == nil {
				a.LatencyHist = make([]int64, len(v))
			}
			for i := range v {
				a.LatencyHist[i] += v[i]
			}
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
		// Compute percentiles from the cumulative histogram. If the slice is
		// nil (no observations in the window) the function returns zero
		// values, and the omitempty tag suppresses them in the JSON — which
		// is the right answer: an endpoint with no attempts has no latency
		// distribution to show.
		if len(a.LatencyHist) > 0 {
			a.Percentiles = percentileFromHistogram(a.LatencyHist, m.attemptLatencyBounds)
		}
		out = append(out, *a)
	}
	return out
}

// DashboardChainStatus reports, for each logical model in cfg, how many of its
// configured endpoints are currently eligible. Eligibility is computed by the
// caller (which owns the router) rather than here, because the ring knows
// nothing about cooldowns or circuits.
//
// The callback returns four values, not one, because "eligible" is a summary
// that hides the two different reasons an endpoint can be down — an active
// cooldown and an open circuit — and hiding them is how an operator ends up
// staring at a pill that says "down" with no way to know which. All four are
// surfaced in dashboardEndpointInfo and rendered by the page, including the
// cooldown's expiry so the pill can say "down — cooldown until 14:22" instead
// of just "down".
func DashboardChainStatus(
	cfg *Config,
	eligible func(ep ModelEndpoint) (ok, cooldown bool, circuit string, cooldownUntil time.Time),
) []dashboardChainStatus {
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
			ok, cd, circ, until := false, false, "", time.Time{}
			if eligible != nil {
				ok, cd, circ, until = eligible(ep)
			}
			if ok {
				cs.Operational++
			}
			info := dashboardEndpointInfo{
				Endpoint: ep.Key(),
				Eligible: ok,
				Cooldown: cd,
				Circuit:  circ,
			}
			// Carry the expiry only when there is an active cooldown. A
			// zero time would serialize as 0001-01-01 and read as a real
			// (if nonsensical) cooldown, so omit it instead.
			if cd && !until.IsZero() {
				info.CooldownUntil = &until
			}
			cs.Endpoints = append(cs.Endpoints, info)
		}
		out = append(out, cs)
	}
	return out
}

// clone returns a deep copy of a tick suitable for aggregation outside the
// metrics lock. The current ring tick is mutated in place by every recorded
// attempt, so the dashboard must never iterate it after releasing the lock;
// completed ticks are immutable, but cloning the current one is the only way
// to include the still-filling minute in the window without a race.
func (t *attemptTick) clone() *attemptTick {
	if t == nil {
		return nil
	}
	c := &attemptTick{
		attempts:    cloneInt64Map(t.attempts),
		failures:    cloneInt64Map(t.failures),
		latencyNS:   cloneInt64Map(t.latencyNS),
		tokens:      cloneInt64Map(t.tokens),
		activeTicks: cloneInt64Map(t.activeTicks),
		endpoints:   make(map[labelKey]string, len(t.endpoints)),
		start:       t.start,
	}
	for k, v := range t.endpoints {
		c.endpoints[k] = v
	}
	if t.hist != nil {
		c.hist = make(map[labelKey][]int64, len(t.hist))
		for k, v := range t.hist {
			cp := make([]int64, len(v))
			copy(cp, v)
			c.hist[k] = cp
		}
	}
	return c
}

func cloneInt64Map(src map[labelKey]int64) map[labelKey]int64 {
	if src == nil {
		return nil
	}
	dst := make(map[labelKey]int64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
