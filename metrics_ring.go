package main

import (
	"log"
	"time"
)

// attemptTick is one minute's worth of per-endpoint attempt telemetry. It is
// the unit the attempt ring stores; the dashboard sums a range of ticks to
// answer its time-windowed questions.
//
// Each map is bounded by labelsCap like the cumulative maps, so a single tick
// cannot grow without bound even if the config churns. The ring as a whole is
// bounded by dashboardRingLen ticks, so the memory ceiling is fixed.
//
// activeSeconds counts the ticks within the window in which the endpoint was
// actually reached. It exists because the dashboard's speed leaderboard is
// tokens per SECOND, and "second" is not the window: an endpoint that served
// 500 tokens in a minute of a 7d window, then went quiet for the other six
// days, is not a 0.000001 tok/s endpoint — it is a 8.3 tok/s endpoint that
// happened to be idle. Dividing by the window would rank every quiet endpoint
// near zero and bury the genuinely fast ones, which is the opposite of what
// a "fastest" leaderboard should do. The denominator is the time the endpoint
// was actually busy, which is what the operator wants to compare.
//
// hist holds the per-endpoint attempt latency histogram for this tick. It is
// the ring's local copy of the cumulative attemptHistByEnd buckets, kept
// separately so the dashboard can answer "what was the p95 latency of this
// endpoint over the last N hours" without reading the cumulative maps (which
// are keyed by labelKey and would require a reverse lookup to render).
// The bucket boundaries are attemptLatencyBounds; the last slot is +Inf.
type attemptTick struct {
	attempts    map[labelKey]int64
	failures    map[labelKey]int64
	latencyNS   map[labelKey]int64
	tokens      map[labelKey]int64
	activeTicks map[labelKey]int64
	hist        map[labelKey][]int64
	// endpoints maps a key to the raw endpoint string (e.g. "provider:model")
	// the dashboard renders. The cumulative maps store the endpoint under the
	// endpoint= label; the ring keeps the raw string alongside so the
	// dashboard never has to reverse a label-escape to display one.
	endpoints map[labelKey]string
	start     time.Time
}

// newAttemptTick allocates a tick's maps. Every map is allocated rather than
// left to be created lazily on first write, because recordRingAttemptLocked
// writes to four of them unconditionally on every attempt: a nil map there is
// a panic on the hot request path, not a startup cost avoided.
//
// The cost this does pay is four maps per active minute rather than per
// process, which is the point: dashboardRingLen is 10081, but only the
// minutes that actually receive a request reach this function.
func newAttemptTick() *attemptTick {
	return &attemptTick{
		attempts:    make(map[labelKey]int64),
		failures:    make(map[labelKey]int64),
		latencyNS:   make(map[labelKey]int64),
		tokens:      make(map[labelKey]int64),
		activeTicks: make(map[labelKey]int64),
		hist:        make(map[labelKey][]int64),
		endpoints:   make(map[labelKey]string),
	}
}

// attemptTickForLocked returns the tick at the ring's current write position,
// creating it if this is the first observation in it. Caller must hold m.mu.
//
// The tick is created lazily: the ring is pre-allocated with nil pointers so
// a process that never records an attempt pays no per-tick map overhead, and
// only the ticks that actually receive data get their four maps.
func (m *Metrics) attemptTickForLocked() *attemptTick {
	t := m.attemptRing[m.attemptRingNow]
	if t == nil {
		t = newAttemptTick()
		// The tick belongs to the minute it was created in, aligned to
		// the minute boundary the ticker advances on: the journal slots
		// a tick by its minute, so a lazily created tick must carry the
		// boundary, not the instant of its first write.
		t.start = time.Now().Truncate(time.Minute)
		m.attemptRing[m.attemptRingNow] = t
	}
	return t
}

// recordRingAttemptLocked folds one attempt observation into the current tick
// of the ring. Caller must hold m.mu. The key passed in is the one
// boundedAttemptKeyLocked returned for this observation, so the ring and the
// cumulative maps cannot disagree about which bucket it landed in.
//
// The endpoint label is passed separately from the key because the ring's
// maps are keyed on the raw endpoint string (the same value the cumulative
// maps store under the endpoint= label), which is what the dashboard renders.
// They are identical here by construction — see boundedAttemptKeyLocked.
//
// The histogram bucket index is passed in because Attempt computes it before
// taking the lock (the boundary loop is cheap and lock-free) and the ring
// must record the observation in the same bucket the cumulative histogram
// will. Passing the index rather than re-deriving it keeps the two in sync
// by construction: if the boundary array ever changes, both sites break
// together rather than disagreeing silently.
func (m *Metrics) recordRingAttemptLocked(key labelKey, endpoint string, ns int64, ok bool, histIdx int) {
	t := m.attemptTickForLocked()
	t.attempts[key] = t.attempts[key] + 1
	t.activeTicks[key] = 1
	if !ok {
		t.failures[key] = t.failures[key] + 1
	}
	t.latencyNS[key] = t.latencyNS[key] + ns
	// The endpoint string is kept alongside the key so the dashboard can
	// render the raw "provider:model" without reversing a label-escape.
	if _, exists := t.endpoints[key]; !exists {
		t.endpoints[key] = endpoint
	}
	// The histogram is recorded in the same tick so the dashboard can
	// answer "what was the p95 latency over the last N hours" by summing
	// this slice across ticks and computing the percentile from the
	// cumulative distribution. The slice is created on first write, with
	// one slot per boundary plus the +Inf bucket — the same shape as the
	// cumulative attemptHistByEnd, so the two always agree about how many
	// buckets exist.
	h := t.hist[key]
	if h == nil {
		h = make([]int64, len(m.attemptLatencyBounds)+1)
		t.hist[key] = h
	}
	h[histIdx]++
}

// advanceRingTickLocked moves the ring forward by one minute, resetting the
// new current tick. Caller must hold m.mu.
//
// The invariant this relies on: Attempt holds m.mu for the whole of its
// observation, so the ticker can never advance the ring out from under an
// in-flight recordRingAttemptLocked. The ticker writes only to
// attemptRingNow and to the slot it is about to become current, which
// Attempt is not touching while it holds the lock.
//
// The new tick is left nil rather than pre-allocated: most minutes are quiet,
// and allocating four maps per minute for a week would be ~56k maps for a
// process that may never serve a dashboard request. attemptTickForLocked
// creates them on first write.
func (m *Metrics) advanceRingTickLocked() {
	m.attemptRingNow = (m.attemptRingNow + 1) % dashboardRingLen
	m.attemptRing[m.attemptRingNow] = nil
	m.attemptRingStarted = true
}

// restoreDashboardJournalLocked replays the journal into the ring.
// Caller must hold m.mu.
//
// Each tick is placed in the slot its minute maps to, so the ring's
// geometry after a restart is exactly what it would have been had the
// process never stopped: DashboardQuery walks back from
// attemptRingNow, and every restored tick sits at the position its
// minute occupies, so a 1d window still covers the last 1440 minutes
// rather than the last 1440 ticks this process happened to record.
func (m *Metrics) restoreDashboardJournalLocked(path string) (int, error) {
	nowMinute := journalMinute(time.Now())
	lines, err := replayDashboardJournal(path, nowMinute)
	if err != nil {
		return 0, err
	}
	for minute, line := range lines {
		slot := journalSlot(minute)
		if slot < 0 || slot >= dashboardRingLen {
			continue
		}
		m.attemptRing[slot] = line.attemptTick()
	}
	m.attemptRingNow = journalSlot(nowMinute)
	return len(lines), nil
}

// StartDashboardTelemetry starts the background goroutine that advances
// the attempt ring once a minute and appends each completed tick to
// the dashboard journal at journalPath (empty disables persistence).
// It is idempotent: calling it twice does not start a second ticker.
// The returned function stops it.
//
// It is wired from main.go alongside the config watcher, so the dashboard is
// live from startup rather than only after the first dashboard scrape. Without
// this the first 1d/7d leaderboard would report "no data" for up to a minute
// even though the gateway had been serving requests the whole time.
//
// On start it also restores the journal into the ring and prunes the
// journal back to the retention window. The prune is best-effort and
// its failure is logged, not fatal: the timer prunes daily, but a
// missed timer (machine off at 06:30) must not let the journal grow
// without bound, so every boot prunes too.
func (m *Metrics) StartDashboardTelemetry(journalPath string) func() {
	m.mu.Lock()
	if m.attemptRingStarted {
		m.mu.Unlock()
		return func() {}
	}
	m.attemptRingStarted = true
	if journalPath != "" {
		if restored, err := m.restoreDashboardJournalLocked(journalPath); err != nil {
			log.Printf("[dashboard] journal restore failed: %v", err)
		} else if restored > 0 {
			log.Printf("[dashboard] journal restored %d ticks from %s", restored, journalPath)
		}
		if kept, err := pruneDashboardJournal(journalPath); err != nil {
			log.Printf("[dashboard] journal prune failed: %v", err)
		} else if kept == 0 {
			log.Printf("[dashboard] journal %s holds nothing within the retention window", journalPath)
		}
	}
	m.mu.Unlock()

	stop := make(chan struct{})
	go func() {
		// Align to the minute boundary so a tick never straddles a wall-clock
		// minute: the first wait is to the next boundary, then every 60s.
		now := time.Now()
		next := now.Add(dashboardTickSeconds * time.Second)
		next = time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), next.Minute(), 0, 0, next.Location())
		select {
		case <-stop:
			return
		case <-time.After(time.Until(next)):
		}
		ticker := time.NewTicker(dashboardTickSeconds * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				// The tick being left behind is complete: Attempt holds the
				// same lock for the whole of an observation, so nothing can
				// land in it after the advance. It is journaled outside the
				// lock — the append is a file write, and the request path
				// must not wait on disk.
				completed := m.attemptRing[m.attemptRingNow]
				m.advanceRingTickLocked()
				m.mu.Unlock()
				if completed != nil && journalPath != "" {
					if err := appendDashboardJournal(journalPath, completed); err != nil {
						log.Printf("[dashboard] journal append: %v", err)
					}
				}
			}
		}
	}()
	return func() { close(stop) }
}

// RecordDashboardTokens records completion tokens for an endpoint in the
// current tick of the attempt ring. It is called from the streaming and
// non-streaming response paths once the upstream has reported how many
// completion tokens it produced.
//
// The attempt itself does not know the token count — that is a property of
// the response, not the attempt — so the token map starts empty and is only
// written when the caller has both the endpoint and the count. This keeps
// the TPS leaderboard honest: an endpoint that was reached but produced no
// tokens (an error mid-stream) shows 0 tokens rather than being omitted.
//
// The endpoint is keyed by the same labelKey the cumulative maps use, so the
// ring and the cumulative maps cannot disagree about which bucket a token
// count belongs to.
func (m *Metrics) RecordDashboardTokens(endpoint string, tokens int64) {
	if endpoint == "" || tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.attemptTickForLocked()
	key := singleLabelKey("endpoint", endpoint)
	t.tokens[key] = t.tokens[key] + tokens
	if _, exists := t.endpoints[key]; !exists {
		t.endpoints[key] = endpoint
	}
}
