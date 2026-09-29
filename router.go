package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// VisionChainState describes the vision capability state of a logical model's
// entire fallback chain.
type VisionChainState int

const (
	// VisionUnsupported — no endpoint in the chain advertises vision support.
	VisionUnsupported VisionChainState = iota
	// VisionUnavailable — at least one endpoint supports vision but all of them
	// are currently cooled down, marked noVision, or the sticky session is bound
	// to a non-vision model with no eligible replacement.
	VisionUnavailable
	// VisionAvailable — at least one vision-capable endpoint is currently eligible.
	VisionAvailable
)

// VisionChainStatus reports whether the chain for logicalModel can serve a
// vision request right now and, if not, the shortest remaining cooldown before
// any vision-capable endpoint becomes available. It also takes sessionID so the
// sticky-session model is re-evaluated (a session pinned to a non-vision model
// cannot satisfy a vision request and must be replaced with a vision-capable
// one).
func (r *Router) VisionChainStatus(logicalModel, sessionID string) (VisionChainState, time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	chain, ok := r.config.Load().Models[logicalModel]
	if !ok || len(chain.Chain) == 0 {
		return VisionUnsupported, 0
	}

	// A session pinned to a non-vision model must be replaced with a vision-
	// capable one, so we treat such a sticky model as not blocking other
	// vision-capable candidates.
	stickyBlocks := false
	if se, ok := r.sessions[sessionID]; ok {
		bound := se.ep
		if !bound.SupportsVision() || r.noVision[bound.Key()] {
			stickyBlocks = true
		}
	}

	hasVisionSupport := false
	hasEligibleVision := false
	var minVisionWait time.Duration

	for _, ep := range chain.Chain {
		if !ep.SupportsVision() {
			continue
		}
		hasVisionSupport = true
		if r.noVision[ep.Key()] {
			continue
		}
		// A sticky session is allowed to fall through to any vision-capable
		// model in the chain; we count the sticky model itself only if it
		// already supports vision (otherwise the next model in the chain is
		// the one the router will actually pick).
		if se, ok := r.sessions[sessionID]; ok && se.ep.Equal(ep) && stickyBlocks {
			continue
		}
		if cd, ok := r.cooldowns[ep.Key()]; ok {
			remaining := time.Until(cd.Expiry)
			if remaining > 0 {
				if minVisionWait == 0 || remaining < minVisionWait {
					minVisionWait = remaining
				}
				continue
			}
		}
		hasEligibleVision = true
	}

	if !hasVisionSupport {
		return VisionUnsupported, 0
	}
	if !hasEligibleVision {
		return VisionUnavailable, minVisionWait
	}
	return VisionAvailable, 0
}

type CooldownEntry struct {
	Expiry     time.Time `json:"expiry"`
	StatusCode int       `json:"status_code"`
	ErrorCount int       `json:"error_count"`
	LastError  string    `json:"last_error"`
}

// cooldownsState is the on-disk envelope for cooldowns.json. Cooldowns and
// circuits are stored together so a restart reconstructs both the backoff
// windows and the circuit breaker state machine from a single file. The
// legacy bare map[string]CooldownEntry shape is still accepted on load (see
// loadCooldowns) so upgrades from older releases don't lose backoff state.
type cooldownsState struct {
	Cooldowns map[string]CooldownEntry  `json:"cooldowns"`
	Circuits  map[string]CircuitBreaker `json:"circuits"`
}

// CircuitState describes the state of a circuit breaker for an endpoint.
type CircuitState int

const (
	// CircuitClosed is the normal operating state; requests flow through.
	CircuitClosed CircuitState = iota
	// CircuitOpen is a failed state; requests are blocked until a probe
	// succeeds in half-open (or the cooldown expires, triggering half-open).
	CircuitOpen
	// CircuitHalfOpen allows a limited number of probe requests through
	// to test whether the endpoint has recovered.
	CircuitHalfOpen
)

// String returns a lowercase label for CircuitState, used in log lines and
// Prometheus labels.
func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	}
	return "unknown"
}

// CircuitBreaker implements the closed/open/half-open state machine that
// governs endpoint availability after repeated failures. It works alongside
// the cooldown system: cooldowns prevent immediate retries; the circuit
// breaker adds a half-open probe phase to safely restore availability.
type CircuitBreaker struct {
	State      CircuitState `json:"state"`
	OpenedAt   time.Time    `json:"opened_at"`
	ProbesSent int          `json:"probes_sent"`
}

const (
	// defaultCircuitBreakerThreshold is the number of consecutive failures
	// that trip the circuit from Closed to Open.
	defaultCircuitBreakerThreshold = 5
	// defaultCircuitBreakerHalfOpenProbes is the max probes allowed in
	// half-open state before another request is blocked.
	defaultCircuitBreakerHalfOpenProbes = 1
)

type sessionEntry struct {
	ep       ModelEndpoint
	lastUsed time.Time
}

const (
	// sessionTTL bounds how long a sticky session survives without being
	// accessed. Sessions idle longer than this are evicted by the background
	// sweeper so the sessions map can't grow without bound under sustained
	// traffic with many distinct conversations.
	sessionTTL = 2 * time.Hour
	// sessionSweepInterval is how often the sweeper scans for stale sessions.
	sessionSweepInterval = time.Hour
	// cooldownSaveInterval is how long to wait before flushing cooldown state
	// to disk after a change. A burst of failures coalesces into a single
	// write instead of one per failure, bounding disk I/O under load.
	cooldownSaveInterval = 5 * time.Second
)

// cooldownSaveState batches cooldown writes so a burst of failures doesn't
// trigger a disk write on every single one. The first failure marks the state
// dirty; a short timer coalesces subsequent changes into a single write. If the
// timer is already pending, we stop it and re-arm with the latest delay. On Close
// we flush any pending write so state isn't lost on shutdown.
//
// SAFETY: Each trigger creates a new timer and stops the old one rather than
// calling Reset on an already-fired AfterFunc timer (which is undefined per the
// time package docs). The callback checks its identity against the current timer
// so that a callback from a superseded timer aborts without writing — preventing
// both timer leaks and orphaned callbacks that could clobber a newer trigger's
// timer reference.
type cooldownSaveState struct {
	mu    sync.Mutex
	timer *time.Timer
}

func (c *cooldownSaveState) trigger(save func(), delay time.Duration) {
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
	}
	// t escapes to the heap; the callback closes over it for identity comparison.
	var t *time.Timer
	t = time.AfterFunc(delay, func() {
		c.mu.Lock()
		// Only proceed if no newer trigger has replaced this timer.
		if c.timer != t {
			c.mu.Unlock()
			return
		}
		c.timer = nil
		c.mu.Unlock()
		save()
	})
	c.timer = t
	c.mu.Unlock()
}

func (c *cooldownSaveState) stop() {
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

type Router struct {
	config       *atomic.Pointer[Config]
	sessions     map[string]sessionEntry
	cooldowns    map[string]CooldownEntry
	noVision     map[string]bool // endpoints that rejected an image request
	tps          map[string]float64
	circuits     map[string]*CircuitBreaker
	cooldownPath string
	priorityPath string
	mu           sync.RWMutex
	sessionsDone chan struct{}
	sessionsWG   sync.WaitGroup
	cooldownSave *cooldownSaveState
	// cooldownJitter is the random fraction (0..0.25) added to transient
	// cooldowns to break thundering herds. It is set via SetCooldownJitter;
	// zero (the default) disables jitter entirely.
	cooldownJitter float64
	// circuitBreakerThreshold is the number of consecutive failures that
	// trip the circuit from Closed to Open. Zero means circuit breaker
	// is disabled (default enabled when threshold is set via config).
	circuitBreakerThreshold int
	// circuitHalfOpenProbes is the max probes allowed in half-open state.
	circuitHalfOpenProbes int
	// metrics is the Prometheus metrics collector. Set via SetMetrics so
	// circuit state transitions can be observed. Nil in tests that never
	// construct a real gateway.
	metrics *Metrics
}

// SetCooldownJitter sets the random fraction (0..0.25) added to transient
// cooldowns to break thundering herds. It is read by ApplyCooldownForSession
// on every failure; zero (the default) disables jitter entirely.
//
// Callers should validate the upper bound themselves; values above 0.25 are
// silently clamped so a misconfiguration can never produce a cooldown longer
// than 25% above the base.
func (r *Router) SetCooldownJitter(f float64) {
	if f < 0 {
		f = 0
	}
	if f > 0.25 {
		f = 0.25
	}
	r.mu.Lock()
	r.cooldownJitter = f
	r.mu.Unlock()
}

// CooldownJitter returns the currently configured jitter fraction.
func (r *Router) CooldownJitter() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cooldownJitter
}

// SetMetrics attaches a Metrics collector to the router so circuit state
// transitions are observable via the admin metrics endpoint. Nil disables
// transition recording (useful for tests that never build a gateway).
func (r *Router) SetMetrics(m *Metrics) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = m
}

// emitCircuitTransition records a circuit state change in the metrics
// collector, if one is attached. Nil-safe and lock-safe: the metrics
// collector has its own internal lock, so it can be called while r.mu is
// held.
func (r *Router) emitCircuitTransition(from, to CircuitState, endpoint string) {
	if r.metrics != nil {
		r.metrics.CircuitTransition(from, to, endpoint)
	}
}

// SetCircuitBreakerThreshold sets the number of consecutive failures that
// trip the circuit from Closed to Open. A value of 0 disables the circuit
// breaker entirely (ad-hoc cooldown escalation handles failures instead).
func (r *Router) SetCircuitBreakerThreshold(n int) {
	if n < 0 {
		n = 0
	}
	r.mu.Lock()
	r.circuitBreakerThreshold = n
	r.mu.Unlock()
}

// CircuitBreakerThreshold returns the effective threshold (0 disables).
func (r *Router) CircuitBreakerThreshold() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.circuitBreakerThreshold
}

// SetCircuitHalfOpenProbes sets the max probes allowed in half-open state.
func (r *Router) SetCircuitHalfOpenProbes(n int) {
	if n < 1 {
		n = defaultCircuitBreakerHalfOpenProbes
	}
	r.mu.Lock()
	r.circuitHalfOpenProbes = n
	r.mu.Unlock()
}

// CircuitHalfOpenProbes returns the effective half-open probe count.
func (r *Router) CircuitHalfOpenProbes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.circuitHalfOpenProbes
}

func NewRouter(cfg *Config, cooldownPath string) *Router {
	r := &Router{
		sessions:     make(map[string]sessionEntry),
		cooldowns:    make(map[string]CooldownEntry),
		noVision:     make(map[string]bool),
		tps:          make(map[string]float64),
		circuits:     make(map[string]*CircuitBreaker),
		cooldownPath: cooldownPath,
		priorityPath: strings.TrimSuffix(cooldownPath, ".json") + ".priority.json",
		sessionsDone: make(chan struct{}),
		cooldownSave: &cooldownSaveState{},
	}
	r.config = &atomic.Pointer[Config]{}
	r.config.Store(cfg)
	r.loadCooldowns()
	r.loadPriorities()
	r.SetCooldownJitter(cfg.Preferences.CooldownJitterFraction())
	r.SetCircuitBreakerThreshold(cfg.Preferences.CircuitBreakerThresholdValue())
	r.SetCircuitHalfOpenProbes(cfg.Preferences.CircuitHalfOpenProbesValue())
	r.sessionsWG.Add(1)
	go r.sweepSessions()
	return r
}

// cleanupStaleEntries removes entries from cooldowns, noVision, and tps maps
// for endpoints that are no longer in the current config. Without this, removing
// a model from the config leaves its entries in these maps forever (a slow
// memory leak). Also caps ErrorCount so a persistently-failing model's counter
// can't grow without bound. Called periodically by the session sweeper.
func (r *Router) cleanupStaleEntries() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.config.Load()
	// Build the set of all currently-configured endpoint keys
	valid := map[string]bool{}
	for _, mc := range cfg.Models {
		for _, ep := range mc.Chain {
			valid[ep.Key()] = true
		}
	}
	for k := range r.cooldowns {
		if !valid[k] {
			delete(r.cooldowns, k)
			continue
		}
		// Cap ErrorCount so a persistently-failing model's counter can't grow
		// without bound. The cooldown is already at maxCooldown by errorCount=10,
		// so further increments only waste memory.
		if cd := r.cooldowns[k]; cd.ErrorCount > 100 {
			cd.ErrorCount = 100
			r.cooldowns[k] = cd
		}
	}
	for k := range r.noVision {
		if !valid[k] {
			delete(r.noVision, k)
		}
	}
	for k := range r.tps {
		if !valid[k] {
			delete(r.tps, k)
		}
	}
	r.cleanupCircuitsLocked()
}

// sweepSessions periodically removes sessions that haven't been accessed in
// over sessionTTL and cleans up stale cooldown/noVision/tps entries. Without
// this, the sessions map would grow without bound under sustained traffic with
// many distinct conversations (each unique model+messages hash becomes a
// permanent entry).
func (r *Router) sweepSessions() {
	defer r.sessionsWG.Done()
	ticker := time.NewTicker(sessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.sessionsDone:
			return
		case <-ticker.C:
			r.evictStaleSessions()
			r.cleanupStaleEntries()
		}
	}
}

func (r *Router) evictStaleSessions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for sid, se := range r.sessions {
		if now.Sub(se.lastUsed) > sessionTTL {
			delete(r.sessions, sid)
		}
	}
}

// Close stops the background session sweeper and flushes any pending cooldown
// save so state isn't lost on shutdown. Called on graceful shutdown.
func (r *Router) Close() {
	r.cooldownSave.stop()
	close(r.sessionsDone)
	r.sessionsWG.Wait()
	r.saveCooldowns()
}

func (r *Router) loadCooldowns() {
	if r.cooldownPath == "" {
		return
	}
	data, err := os.ReadFile(r.cooldownPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("[warn] failed to load cooldowns: %v", err)
		return
	}
	// Two on-disk shapes are supported:
	//  1. The current merged envelope: {"cooldowns":{...},"circuits":{...}}.
	//  2. Legacy: a bare map[string]CooldownEntry (older releases). We detect
	//     the shape by peeking at the first non-whitespace byte: '{' followed
	//     by '"'cooldowns"'"' is the envelope; '{' followed by anything else
	//     is the legacy bare map. This lets an upgrade from an older binary
	//     keep its existing cooldowns.json without losing backoff state.
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") && !strings.Contains(trimmed[:min(len(trimmed), 200)], "\"cooldowns\"") {
		var loaded map[string]CooldownEntry
		if err := json.Unmarshal(data, &loaded); err != nil {
			log.Printf("[warn] failed to parse legacy cooldowns (state reset): %v", err)
			return
		}
		now := time.Now()
		for k, v := range loaded {
			if v.Expiry.After(now) {
				r.cooldowns[k] = v
			}
		}
		log.Printf("[debug] loaded %d active cooldowns (legacy format)", len(r.cooldowns))
		return
	}
	// Decode the two sections independently. A single json.Unmarshal into the
	// merged envelope is all-or-nothing: one malformed value anywhere — most
	// easily a hand-edited circuits entry whose "state" is the string "open"
	// rather than the numeric CircuitState — aborts the whole parse and every
	// cooldown is dropped, so one bad byte silently discards all backoff state
	// and the daemon re-hammers every failing endpoint from scratch. Parsing
	// each section on its own means a bad circuit costs only that circuit.
	var state cooldownsState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[warn] failed to parse cooldowns envelope (recovering sections): %v", err)
		// Recover the cooldowns even though the envelope failed to decode.
		var cooldownsOnly struct {
			Cooldowns map[string]CooldownEntry `json:"cooldowns"`
		}
		if err := json.Unmarshal(data, &cooldownsOnly); err != nil {
			log.Printf("[warn] failed to parse cooldowns (state reset): %v", err)
			return
		}
		state.Cooldowns = cooldownsOnly.Cooldowns
	}
	now := time.Now()
	for k, v := range state.Cooldowns {
		if v.Expiry.After(now) {
			r.cooldowns[k] = v
		}
	}
	// Reconstruct circuit state. A circuit is only meaningful if it is Open
	// or HalfOpen with a still-active window; Closed circuits carry no
	// information and are omitted. We restore Open only when the cooldown is
	// still active (otherwise the cooldown-expiry path in isAvailableLocked
	// would re-open it on the next request anyway). HalfOpen is restored as
	// written — a half-open circuit with an expired cooldown will immediately
	// admit a probe, which is the desired behaviour.
	// Same independent-decode treatment for circuits: a malformed circuit
	// must not cost us the cooldowns recovered above.
	circuits := state.Circuits
	if circuits == nil {
		var circuitsOnly struct {
			Circuits map[string]CircuitBreaker `json:"circuits"`
		}
		if err := json.Unmarshal(data, &circuitsOnly); err != nil {
			log.Printf("[warn] failed to parse circuits (cooldowns kept): %v", err)
		} else {
			circuits = circuitsOnly.Circuits
		}
	}
	for k, v := range circuits {
		cb := v
		switch cb.State {
		case CircuitOpen:
			// Only keep the circuit open if the underlying cooldown is still
			// active; otherwise the endpoint is effectively healthy again and
			// the first request will close it.
			if cd, ok := r.cooldowns[k]; ok && cd.Expiry.After(now) {
				r.circuits[k] = &cb
			}
		case CircuitHalfOpen:
			r.circuits[k] = &cb
		}
	}
	log.Printf("[debug] loaded %d active cooldowns, %d circuits", len(r.cooldowns), len(r.circuits))
}

func (r *Router) saveCooldowns() {
	if r.cooldownPath == "" {
		return
	}
	// Copy active cooldowns and open/half-open circuits under RLock to avoid
	// blocking all routing during disk I/O. Closed circuits carry no state
	// worth persisting (they'd be reconstructed as Closed on load anyway).
	r.mu.RLock()
	now := time.Now()
	state := cooldownsState{
		Cooldowns: make(map[string]CooldownEntry),
		Circuits:  make(map[string]CircuitBreaker),
	}
	for k, v := range r.cooldowns {
		if v.Expiry.After(now) {
			state.Cooldowns[k] = v
		}
	}
	for k, v := range r.circuits {
		if v.State == CircuitOpen || v.State == CircuitHalfOpen {
			cp := *v
			state.Circuits[k] = cp
		}
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[debug] failed to marshal cooldowns: %v", err)
		return
	}
	// Atomic write: a temp file in the same directory followed by rename, so a
	// crash mid-write can't leave a truncated cooldowns.json (which would
	// otherwise be silently dropped on next start, losing all backoff state).
	dir := filepath.Dir(r.cooldownPath)
	tmp, err := os.CreateTemp(dir, ".cooldowns-*.tmp")
	if err != nil {
		log.Printf("[debug] failed to create cooldowns temp file: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("[debug] failed to write cooldowns temp file: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		log.Printf("[debug] failed to close cooldowns temp file: %v", err)
		return
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		log.Printf("[debug] failed to chmod cooldowns temp file: %v", err)
	}
	if err := os.Rename(tmpName, r.cooldownPath); err != nil {
		os.Remove(tmpName)
		log.Printf("[debug] failed to rename cooldowns file: %v", err)
	}
}

func (r *Router) loadPriorities() {
	if r.priorityPath == "" {
		return
	}
	data, err := os.ReadFile(r.priorityPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[debug] failed to load priorities: %v", err)
		}
		return
	}
	var loaded map[string]float64
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("[debug] failed to parse priorities: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range loaded {
		r.tps[k] = v
	}
}

func (r *Router) savePriorities() {
	if r.priorityPath == "" || r.cooldownPath == "" {
		return
	}
	r.mu.RLock()
	data, err := json.MarshalIndent(r.tps, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		log.Printf("[debug] failed to marshal priorities: %v", err)
		return
	}
	dir := filepath.Dir(r.priorityPath)
	tmp, err := os.CreateTemp(dir, ".priority-*.tmp")
	if err != nil {
		log.Printf("[debug] failed to create priorities temp file: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("[debug] failed to write priorities temp file: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		log.Printf("[debug] failed to close priorities temp file: %v", err)
		return
	}
	if err := os.Rename(tmpName, r.priorityPath); err != nil {
		os.Remove(tmpName)
		log.Printf("[debug] failed to rename priorities file: %v", err)
	}
}

func (r *Router) RecordTPS(ep ModelEndpoint, tps float64) {
	r.mu.Lock()
	key := ep.Key()
	r.tps[key] = (r.tps[key] * 0.8) + (tps * 0.2)
	r.mu.Unlock()
	r.savePriorities()
}

// isAvailableLocked reports whether ep can accept a request, considering both
// cooldowns and the circuit breaker state machine.
//
//   - A model whose cooldown hasn't expired is unavailable, UNLESS the circuit
//     is in half-open and the probe budget is available (a probe request is
//     allowed through even before a cooldown fully expires — this is how the
//     breaker safely restores availability).
//   - A model whose cooldown HAS expired is available again, subject to the
//     circuit breaker: an Open circuit keeps it blocked (it's still
//     recovering), and the first request past the open-duration boundary
//     enters half-open as a probe.
func (r *Router) isAvailableLocked(ep *ModelEndpoint) bool {
	key := ep.Key()
	cd, hasCooldown := r.cooldowns[key]
	cb := r.circuits[key]

	// Half-open: allow up to the probe budget through.
	if cb != nil && cb.State == CircuitHalfOpen {
		max := r.circuitHalfOpenProbes
		if max <= 0 {
			max = defaultCircuitBreakerHalfOpenProbes
		}
		if cb.ProbesSent < max {
			cb.ProbesSent++
			r.circuits[key] = cb
			return true
		}
		return false
	}

	// Active cooldown → unavailable, unless disabled by circuit half-open
	// (handled above) or the circuit opens the endpoint as a probe.
	if hasCooldown && time.Now().Before(cd.Expiry) {
		// Cooldown still active. If the circuit is open, the probe is blocked
		// by the cooldown too; no need to override.
		return false
	}

	// Cooldown expired (or no cooldown): check circuit state.
	if cb != nil && cb.State == CircuitOpen {
		// Cooldown elapsed while Open: transition to half-open and admit a probe.
		r.emitCircuitTransition(CircuitOpen, CircuitHalfOpen, key)
		cb.State = CircuitHalfOpen
		cb.ProbesSent = 1
		cb.OpenedAt = time.Now()
		r.circuits[key] = cb
		log.Printf("[debug] circuit %s: open -> half-open", key)
		return true
	}

	return true
}

// IsAvailable reports whether ep is currently eligible for non-sticky traffic.
func (r *Router) IsAvailable(ep *ModelEndpoint) bool {
	// isAvailableLocked may mutate circuit state (Open→HalfOpen, probe
	// counters), so it must run under a write lock. IsAvailable is not on the
	// hot request path (SelectEndpoint calls isAvailableLocked directly under
	// its existing lock); it exists for admin queries and tests.
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isAvailableLocked(ep)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (r *Router) minCooldownWait(chain []ModelEndpoint, requireVision bool) time.Duration {
	minWait := time.Duration(0)
	for _, ep := range chain {
		// A model that can never serve a vision request (either marked noVision
		// at runtime or configured with vision:false) is irrelevant to the
		// wait calculation for vision requests.
		if requireVision && (!ep.SupportsVision() || r.noVision[ep.Key()]) {
			continue
		}
		if cd, ok := r.cooldowns[ep.Key()]; ok {
			remaining := time.Until(cd.Expiry)
			if remaining > 0 && (minWait == 0 || remaining < minWait) {
				minWait = remaining
			}
		}
	}
	return minWait
}

// SelectEndpoint selects the next endpoint for a logical model, considering the
// session's sticky routing, cooldowns, vision eligibility, and the set of
// endpoints already tried in the current fallback sequence (tried).
//
// The tried set is per-request (local to each handleStream/handleCompletion
// fallback loop), NOT stored in the shared session. This prevents concurrent
// requests with the same session ID from interfering with each other's
// endpoint selection — one request's failures do not cause the other to skip
// an endpoint it has not independently tried.
func (r *Router) SelectEndpoint(logicalModel, sessionID string, requireVision bool, tried map[string]bool) (*ModelEndpoint, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	chain, ok := r.config.Load().Models[logicalModel]
	if !ok || len(chain.Chain) == 0 {
		return nil, 0
	}

	if se, ok := r.sessions[sessionID]; ok {
		// Update lastUsed so the session isn't evicted while active.
		se.lastUsed = time.Now()
		r.sessions[sessionID] = se
		ep := se.ep
		available := r.isAvailableLocked(&ep)
		if available && r.visionEligible(&ep, requireVision) && !tried[ep.Key()] {
			return &ep, 0
		}
		// The sticky model is out: either cooled, marked no-vision, or already
		// tried and failed in this request's fallback sequence.
		capabilityFallback := available && !r.visionEligible(&ep, requireVision)
		for _, next := range chain.Chain {
			if r.isEligibleLocked(&next, requireVision) && !tried[next.Key()] {
				if capabilityFallback {
					log.Printf("[debug] session=%s model=%s -> fallback (no vision) %s/%s -> %s/%s", sessionID, logicalModel, ep.Provider, ep.Model, next.Provider, next.Model)
				}
				if len(tried) == 0 {
					r.sessions[sessionID] = sessionEntry{ep: next, lastUsed: time.Now()}
				}
				return &next, 0
			}
		}
		// All endpoints have been tried in this fallback sequence. Fall back to
		// the first available one (ignoring the tried set) so we don't return
		// nil and error out the client when there's still a chance one will work.
		for _, next := range chain.Chain {
			if r.isEligibleLocked(&next, requireVision) {
				log.Printf("[debug] session=%s model=%s -> retry exhausted, retrying %s/%s", sessionID, logicalModel, next.Provider, next.Model)
				r.sessions[sessionID] = sessionEntry{ep: next, lastUsed: time.Now()}
				return &next, 0
			}
		}
		// All endpoints are in cooldown. Return nil so the handler waits
		// for the shortest cooldown to expire rather than hammering a
		// cooled model on every iteration (which caused the
		// "all cooled down, retrying X" spam loop). The handler's
		// attempts counter bounds the total retries, so this never
		// causes an infinite loop.
		return nil, r.minCooldownWait(chain.Chain, requireVision)
	}

	// New session
	if ep := r.selectInitialEndpoint(logicalModel, sessionID, &chain, requireVision); ep != nil {
		log.Printf("[debug] session=%s model=%s -> new session -> %s/%s", sessionID, logicalModel, ep.Provider, ep.Model)
		r.sessions[sessionID] = sessionEntry{ep: *ep, lastUsed: time.Now()}
		return ep, 0
	}

	return nil, r.minCooldownWait(chain.Chain, requireVision)
}

// selectInitialEndpoint picks the first endpoint for a brand new session.
//
// Without rotation, every new session starts at depth 0, so steady-state
// traffic lands on a single model while the rest of the chain sits idle --
// capacity that is only ever reached reactively, after a failure or a
// cooldown. To make the chain load-bearing, consecutive new sessions are
// started at successive depths within a bounded window at the head of the
// chain, so load spreads evenly across the top N endpoints.
//
// The rotation is deliberately narrow, because it trades quality for spread:
//
//   - It applies ONLY to the very first endpoint of a brand new session. Once
//     a session is pinned, every later request in it is sticky; and once a
//     request is retrying, the chain is walked strictly in configured order.
//     So rotation can never delay escalation to a better model.
//   - The window is bounded (preferences.initial_rotation_window, default 8),
//     so a session is never started on a model far down the chain. Only the
//     top of the chain is load-bearing; the tail remains reactive fallback.
//   - Only endpoints that are currently eligible participate: one in cooldown
//     or lacking a required capability is passed over rather than handed to a
//     new session.
//
// The cursor is per logical model and advances by one per new session, so
// sessions spread across the window and then wrap.
func (r *Router) selectInitialEndpoint(logicalModel, sessionID string, chain *ModelConfig, requireVision bool) *ModelEndpoint {
	cfg := r.config.Load()
	window := cfg.Preferences.RotationWindow()
	if window <= 0 {
		// Rotation disabled: strict chain order.
		for i := range chain.Chain {
			if r.isEligibleLocked(&chain.Chain[i], requireVision) {
				ep := chain.Chain[i]
				return &ep
			}
		}
		return nil
	}

	// Build the rotation window from the first `window` chain POSITIONS,
	// dropping any that are currently ineligible. Anchoring to positions
	// rather than to the first `window` eligible endpoints matters: it means
	// a cooldown shrinks the rotation set instead of pulling a deeper model
	// up into it, so the top of the chain stays the load-bearing region and
	// the quality of a new session's first pick can only improve.
	candidates := make([]int, 0, window)
	limit := window
	if limit > len(chain.Chain) {
		limit = len(chain.Chain)
	}
	for i := 0; i < limit; i++ {
		if r.isEligibleLocked(&chain.Chain[i], requireVision) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		// Every endpoint in the window is ineligible. Rather than give up
		// (which would make the request wait), continue down the chain: this
		// is the same escalation the fallback loop performs.
		for i := limit; i < len(chain.Chain); i++ {
			if r.isEligibleLocked(&chain.Chain[i], requireVision) {
				ep := chain.Chain[i]
				return &ep
			}
		}
		return nil
	}

	// The starting depth is derived from the session ID rather than from a
	// shared arrival counter.
	//
	// A counter would spread load correctly on average, but it breaks the
	// session-affinity guarantee: concurrent requests sharing a session ID
	// (client retry storms, parallel sub-requests) each advance the counter
	// and get a DIFFERENT starting depth, so the first burst of a new session
	// fans out across several models and the session ends up pinned to
	// whichever request happened to finish last. That contradicts the
	// documented invariant that a session is always routed to the same model,
	// and it also defeats upstream prompt-cache affinity.
	//
	// Hashing the session ID gives the same aggregate spread -- distinct
	// sessions land on different depths, evenly -- while making the choice a
	// pure function of the session, so every concurrent request of the same
	// session independently computes the SAME endpoint.
	//
	// The logical model is mixed into the hash so the same conversation does
	// not correlate its depth across profiles, and the window membership is
	// mixed in so a config reload reshuffles rather than preserving a stale
	// assignment.
	h := fnv.New64a()
	_, _ = io.WriteString(h, logicalModel)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, sessionID)
	for _, i := range candidates {
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, chain.Chain[i].Key())
	}
	pos := int(h.Sum64() % uint64(len(candidates)))

	ep := chain.Chain[candidates[pos]]
	return &ep
}

// ChainLength returns the number of endpoints in a logical model's fallback
// chain (0 if the model is unknown). Used to bound retry loops.
func (r *Router) ChainLength(logicalModel string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	chain, ok := r.config.Load().Models[logicalModel]
	if !ok {
		return 0
	}
	return len(chain.Chain)
}

func (r *Router) ApplyCooldown(ep *ModelEndpoint, statusCode int, errMsg string) {
	r.ApplyCooldownForSession(ep, statusCode, errMsg, "", 0)
}

// ApplyCooldownForSession records a failure for an endpoint. The sessionID
// parameter is retained for API compatibility but no longer controls any
// per-session state — failed-endpoint tracking is now local to each request's
// fallback loop (see handleStream/handleCompletion) to avoid interference
// between concurrent requests that share a session ID.
//
// retryAfter, when set, is honored as a floor on the computed cooldown so a
// 429's upstream Retry-After header is never ignored. Transient errors
// (429/5xx) also receive up to 25% random jitter to break thundering herds when
// many clients hit the same rate-limited model simultaneously.
func (r *Router) ApplyCooldownForSession(ep *ModelEndpoint, statusCode int, errMsg string, sessionID string, retryAfter time.Duration) time.Duration {
	r.mu.Lock()
	key := ep.Key()
	cd, ok := r.cooldowns[key]
	if !ok {
		cd = CooldownEntry{}
	}
	cd.ErrorCount++
	// Cap ErrorCount so a persistently-failing model's counter can't grow
	// without bound. The cooldown is already at maxCooldown by errorCount=10,
	// so further increments only waste memory.
	if cd.ErrorCount > 100 {
		cd.ErrorCount = 100
	}
	duration := r.cooldownForError(statusCode, cd.ErrorCount)
	// Honor the upstream's Retry-After as a floor: never cool down for less
	// than the provider asked, since it knows its own rate-limit windows.
	if retryAfter > duration {
		duration = retryAfter
	}
	// Add up to 25% random jitter on transient errors (429/5xx) so concurrent
	// clients hitting the same rate-limited model don't all wake up at the same
	// instant and stampede the provider again.
	if statusCode == 429 || (statusCode >= 500 && statusCode <= 599) {
		if r.cooldownJitter > 0 {
			duration += time.Duration(float64(duration) * r.cooldownJitter)
		}
	}
	cd.Expiry = time.Now().Add(duration)
	cd.StatusCode = statusCode
	// Truncate the error message so a verbose provider error body (e.g.
	// Gemini's multi-kilabyte JSON) doesn't bloat cooldowns.json on every
	// retry. Keep it short; the full detail is in the log line below.
	cd.LastError = truncateErr(errMsg, 200)
	r.cooldowns[key] = cd
	if isVisionUnsupported(errMsg) {
		r.noVision[key] = true
	}
	r.recordCircuitFailureLocked(ep, statusCode, &cd)
	r.mu.Unlock()
	// Debounce: coalesce rapid successive failures into a single disk write.
	// The first failure arms a short timer; subsequent failures reset it. On
	// timer expiry we persist. This bounds disk I/O to at most one write per
	// `cooldownSaveInterval` per burst, instead of one per failure.
	r.cooldownSave.trigger(r.saveCooldowns, cooldownSaveInterval)
	log.Printf("[debug] cooldown %s/%s status=%d errors=%d for %v: %s", ep.Provider, ep.Model, statusCode, cd.ErrorCount, duration, summarizeError(errMsg))
	return duration
}

// ApplyCooldownWithDuration records a failure for an endpoint with a
// caller-specified cooldown duration (used by tests to avoid sleeping
// for hours). Otherwise identical to ApplyCooldownForSession.
func (r *Router) ApplyCooldownWithDuration(ep *ModelEndpoint, statusCode int, errMsg string, sessionID string, duration time.Duration) {
	r.mu.Lock()
	key := ep.Key()
	cd, ok := r.cooldowns[key]
	if !ok {
		cd = CooldownEntry{}
	}
	cd.ErrorCount++
	if cd.ErrorCount > 100 {
		cd.ErrorCount = 100
	}
	cd.Expiry = time.Now().Add(duration)
	cd.StatusCode = statusCode
	cd.LastError = truncateErr(errMsg, 200)
	r.cooldowns[key] = cd
	if isVisionUnsupported(errMsg) {
		r.noVision[key] = true
	}
	r.recordCircuitFailureLocked(ep, statusCode, &cd)
	r.mu.Unlock()
}

// truncateErr reduces an error message to at most maxLen runes, adding an
// ellipsis if it was cut. It rounds on runes (not bytes) so multi-byte UTF-8
// characters are never split.
func truncateErr(msg string, maxLen int) string {
	runes := []rune(msg)
	if len(runes) <= maxLen {
		return msg
	}
	return string(runes[:maxLen]) + "..."
}

// summarizeError reduces a provider error body to a single short line for logs
// so we don't dump multi-kilobyte JSON (e.g. Gemini's full error payload) on
// every cooldown event.
func summarizeError(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 120
	runes := []rune(msg)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return msg
}

// RecordSuccess resets a model's cooldown on a successful response so the
// error count only reflects *recent* consecutive failures and escalation can't
// run away. It also closes the circuit breaker (any open/half-open circuit
// returns to Closed) and clears the probe counter, since a successful request
// proves the endpoint is healthy.
func (r *Router) RecordSuccess(ep *ModelEndpoint) {
	r.mu.Lock()
	deleted := false
	key := ep.Key()
	if _, ok := r.cooldowns[key]; ok {
		delete(r.cooldowns, key)
		deleted = true
	}
	// Close any open circuit and reset its probe counter.
	if cb, ok := r.circuits[key]; ok {
		if cb.State != CircuitClosed {
			r.emitCircuitTransition(cb.State, CircuitClosed, key)
			log.Printf("[debug] circuit %s: %d -> closed", key, int(cb.State))
		}
		cb.State = CircuitClosed
		cb.ProbesSent = 0
		cb.OpenedAt = time.Time{}
		r.circuits[key] = cb
	}
	r.mu.Unlock()
	if deleted {
		r.saveCooldowns()
	}
}

// recordCircuitFailureLocked is the lock-free core of RecordFailure, usable
// from within callers that already hold r.mu. It uses cd.ErrorCount as the
// consecutive-failure counter so the circuit breaker and cooldown escalation
// share one source of truth.
//
// Only transient errors (429/5xx) advance the circuit: 404/401/403 are
// permanent and are handled purely by cooldowns (opening a circuit on auth
// errors would just delay a retry that still won't succeed until creds
// rotate).
func (r *Router) recordCircuitFailureLocked(ep *ModelEndpoint, statusCode int, cd *CooldownEntry) {
	if r.circuitBreakerThreshold <= 0 {
		return
	}
	// Only transient errors trip the circuit. Status 0 (connection error,
	// timeout, DNS failure) is treated as transient because the endpoint is
	// reachable in principle — the failure is in the network path, not the
	// model itself. 404/401/403 are permanent and handled purely by cooldowns.
	if statusCode != 429 && statusCode != 0 && (statusCode < 500 || statusCode > 599) {
		return
	}
	key := ep.Key()
	cb := r.circuits[key]
	if cb == nil {
		cb = &CircuitBreaker{State: CircuitClosed}
		r.circuits[key] = cb
	}
	// ProbesSent is the single source of truth for consecutive failures,
	// shared by both the cooldown escalation path (ApplyCooldown*) and the
	// standalone RecordFailure API. Using cd.ErrorCount here instead would
	// create a second independent counter and trip the circuit at half the
	// configured threshold.
	switch cb.State {
	case CircuitClosed:
		cb.ProbesSent++
		if cb.ProbesSent >= r.circuitBreakerThreshold {
			r.emitCircuitTransition(CircuitClosed, CircuitOpen, key)
			cb.State = CircuitOpen
			cb.OpenedAt = time.Now()
			log.Printf("[debug] circuit %s: closed -> open (failures=%d, status=%d)", key, cb.ProbesSent, statusCode)
		}
	case CircuitHalfOpen:
		// Failed probe → reopen with a fresh open window.
		cb.ProbesSent++
		r.emitCircuitTransition(CircuitHalfOpen, CircuitOpen, key)
		cb.State = CircuitOpen
		cb.OpenedAt = time.Now()
		log.Printf("[debug] circuit %s: half-open probe failed -> open", key)
	case CircuitOpen:
		// Already open; a failure extends the open window.
		cb.ProbesSent++
		cb.OpenedAt = time.Now()
	}
}

// RecordFailure records a failed request for ep and, if the circuit breaker
// is enabled, advances the state machine: Closed → Open after the threshold
// is reached, Open → Open (with a new open-duration), and HalfOpen → Open on
// a failed probe.
//
// The circuit breaker is opt-in via preferences.circuit_breaker_threshold;
// a threshold of 0 disables it entirely (ad-hoc cooldown escalation handles
// failures instead). When disabled this method is a no-op.
func (r *Router) RecordFailure(ep *ModelEndpoint, statusCode int) {
	if r.circuitBreakerThreshold <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := ep.Key()
	cb := r.circuits[key]
	if cb == nil {
		cb = &CircuitBreaker{State: CircuitClosed}
		r.circuits[key] = cb
	}

	switch cb.State {
	case CircuitClosed:
		// Count consecutive failures since the last success (which reset
		// ProbesSent to 0). The threshold is reached only after that many
		// consecutive failures; a single failure never trips the circuit.
		cb.ProbesSent++
		if cb.ProbesSent >= r.circuitBreakerThreshold {
			r.emitCircuitTransition(CircuitClosed, CircuitOpen, key)
			cb.State = CircuitOpen
			cb.OpenedAt = time.Now()
			log.Printf("[debug] circuit %s: closed -> open (after %d failures, status=%d)", key, cb.ProbesSent, statusCode)
		}
	case CircuitOpen:
		// Still open: a failed probe (shouldn't normally happen since the
		// cooldown blocks probes, but a half-open probe that fails re-opens
		// the circuit with a fresh open-time so the backoff restarts).
		cb.State = CircuitOpen
		cb.OpenedAt = time.Now()
		cb.ProbesSent++
	case CircuitHalfOpen:
		// A probe failed: re-open the circuit with a fresh open-time so the
		// backoff restarts. The next cooldown expiry will re-admit a probe.
		r.emitCircuitTransition(CircuitHalfOpen, CircuitOpen, key)
		cb.State = CircuitOpen
		cb.OpenedAt = time.Now()
		cb.ProbesSent++
		log.Printf("[debug] circuit %s: half-open probe failed -> open", key)
	}
}

// RecordProbeSuccess records that a half-open probe request succeeded: the
// circuit closes and the probe counter resets. Called by the request path
// after a successful response from a half-open endpoint.
func (r *Router) RecordProbeSuccess(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := ep.Key()
	if cb, ok := r.circuits[key]; ok && cb.State == CircuitHalfOpen {
		r.emitCircuitTransition(CircuitHalfOpen, CircuitClosed, key)
		cb.State = CircuitClosed
		cb.ProbesSent = 0
		cb.OpenedAt = time.Time{}
		log.Printf("[debug] circuit %s: half-open probe succeeded -> closed", key)
	}
}

// CircuitState returns the current state of the circuit breaker for ep.
// If the circuit breaker is disabled (threshold 0) it reports CircuitClosed.
func (r *Router) CircuitState(ep *ModelEndpoint) CircuitState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.circuitBreakerThreshold <= 0 {
		return CircuitClosed
	}
	if cb, ok := r.circuits[ep.Key()]; ok {
		return cb.State
	}
	return CircuitClosed
}

// GetCircuitState is an alias for CircuitState (admin/tests).
func (r *Router) GetCircuitState(ep *ModelEndpoint) CircuitState {
	return r.CircuitState(ep)
}

// GetAllCircuits returns a snapshot of every circuit breaker's state, keyed
// by endpoint. Used by the admin endpoint for observability.
func (r *Router) GetAllCircuits() map[string]*CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*CircuitBreaker, len(r.circuits))
	for k, v := range r.circuits {
		cp := *v
		out[k] = &cp
	}
	return out
}

// ResetCircuit manually resets the circuit breaker for ep to Closed. Used by
// operators to force an endpoint back into rotation after a transient outage.
func (r *Router) ResetCircuit(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, ok := r.circuits[ep.Key()]; ok {
		cb.State = CircuitClosed
		cb.ProbesSent = 0
		cb.OpenedAt = time.Time{}
	}
}

// ClearCooldowns drops cooldown and circuit state so endpoints can be probed
// again immediately. When modelKey is empty every entry is cleared; otherwise
// only the named endpoint is reset. It returns the number of entries removed.
//
// This exists because the only other way to clear backoff state was to stop
// the daemon, hand-edit cooldowns.json, and start it again. That is a
// footgun: the router holds the authoritative copy in memory and rewrites the
// whole file from it on the next failure, so an edit made while the daemon is
// running is silently reverted (and, if the file is left malformed, the
// restart can drop every cooldown instead of just the intended ones).
func (r *Router) ClearCooldowns(modelKey string) int {
	r.mu.Lock()
	removed := 0
	if modelKey == "" {
		removed = len(r.cooldowns) + len(r.circuits)
		r.cooldowns = make(map[string]CooldownEntry)
		r.circuits = make(map[string]*CircuitBreaker)
	} else {
		if _, ok := r.cooldowns[modelKey]; ok {
			delete(r.cooldowns, modelKey)
			removed++
		}
		if _, ok := r.circuits[modelKey]; ok {
			delete(r.circuits, modelKey)
			removed++
		}
	}
	r.mu.Unlock()
	if removed > 0 {
		// Persist immediately rather than waiting for the debounced save timer:
		// an operator clearing state needs it durable now, and a pending
		// failure would otherwise re-arm the timer and rewrite the file.
		r.saveCooldowns()
	}
	return removed
}

// cleanupCircuits removes circuit breaker entries for endpoints that are no
// longer in the current config. Called periodically by the session sweeper.
func (r *Router) cleanupCircuits() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupCircuitsLocked()
}

// cleanupCircuitsLocked is the lock-free core of cleanupCircuits. Callers must
// already hold r.mu.
func (r *Router) cleanupCircuitsLocked() {
	cfg := r.config.Load()
	valid := map[string]bool{}
	for _, mc := range cfg.Models {
		for _, ep := range mc.Chain {
			valid[ep.Key()] = true
		}
	}
	for k := range r.circuits {
		if !valid[k] {
			delete(r.circuits, k)
		}
	}
}

func (r *Router) ApplyCooldownFromError(ep *ModelEndpoint, err error) {
	r.ApplyCooldownFromErrorForSession(ep, err, "")
}

func (r *Router) ApplyCooldownFromErrorForSession(ep *ModelEndpoint, err error, sessionID string) {
	if err == nil {
		return
	}
	statusCode := 0
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		statusCode = providerErr.StatusCode
	}
	errMsg := err.Error()
	retryAfter := time.Duration(0)
	if errors.As(err, &providerErr) {
		retryAfter = providerErr.RetryAfter
	}
	r.ApplyCooldownForSession(ep, statusCode, errMsg, sessionID, retryAfter)
}

func baseCooldownForError(statusCode int) time.Duration {
	switch statusCode {
	case 429:
		// Rate limit: start at 30s. Escalation below handles persistent limits.
		return 30 * time.Second
	case 500, 502, 503:
		return 30 * time.Second
	case 504:
		return 60 * time.Second
	case 404:
		// Model-not-found / misconfiguration: effectively permanent,
		// so cool it for 7 days to avoid wasting requests.
		return 7 * 24 * time.Hour
	case 401, 403:
		// Auth failure: retrying won't help until credentials rotate.
		return 30 * time.Minute
	default:
		return 30 * time.Second
	}
}

// cooldownForError returns how long to keep a model out of rotation after a
// failure. Transient errors (429/5xx) escalate modestly and are hard-capped so
// a burst can never disable a model for long. Errors that are effectively
// permanent (4xx auth/404, and repeated timeouts) get a very long cooldown so a
// misconfigured model is not retried forever. After many consecutive failures,
// the model is hard-banned for a very long time to avoid wasting requests on a
// permanently broken endpoint. The error count is reset on a successful request
// (see Router.RecordSuccess), so escalation only reflects recent consecutive
// failures.
func (r *Router) cooldownForError(statusCode int, errorCount int) time.Duration {
	base := baseCooldownForError(statusCode)
	if errorCount <= 1 {
		return base
	}
	// Transient: escalate but bound the total. Use a gentler exponential-ish
	// curve so a persistently rate-limited model backs off to minutes (not
	// seconds) instead of hammering it every 60s forever.
	if statusCode == 429 || (statusCode >= 500 && statusCode <= 599) {
		factor := time.Duration(errorCount)
		if factor > 10 {
			factor = 10
		}
		d := base * factor
		const maxCooldown = 30 * time.Minute
		if d > maxCooldown {
			d = maxCooldown
		}
		return d
	}
	// Permanent-looking (4xx / repeated): after a few consecutive failures,
	// treat as a soft ban so we stop hammering the chain on every request.
	if errorCount >= 3 {
		// After many consecutive failures, the model is effectively
		// permanently broken. Hard-ban it for a very long time to avoid
		// wasting requests on every retry cycle.
		if errorCount >= 10 {
			return 7 * 24 * time.Hour // 7 days
		}
		return 24 * time.Hour
	}
	return base
}

func (r *Router) ResetCooldown(ep *ModelEndpoint) {
	r.mu.Lock()
	delete(r.cooldowns, ep.Key())
	r.mu.Unlock()
	r.saveCooldowns()
}

// MarkNoVision records that an endpoint rejected an image request, so it is
// skipped for subsequent vision requests (without a cooldown, since the model
// itself is healthy). Runtime-only; not persisted.
func (r *Router) MarkNoVision(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noVision[ep.Key()] = true
}

// visionEligible reports whether ep can serve a request that requires vision.
// Non-vision requests (requireVision=false) are always eligible.
func (r *Router) visionEligible(ep *ModelEndpoint, requireVision bool) bool {
	if !requireVision {
		return true
	}
	return ep.SupportsVision() && !r.noVision[ep.Key()]
}

// isEligibleLocked combines cooldown availability with capability eligibility.
func (r *Router) isEligibleLocked(ep *ModelEndpoint, requireVision bool) bool {
	return r.isAvailableLocked(ep) && r.visionEligible(ep, requireVision)
}

// isVisionUnsupported reports whether a provider error indicates the model
// cannot handle image/multimodal content, so we can mark it noVision.
func isVisionUnsupported(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "image_url") ||
		strings.Contains(s, "multimodal") ||
		(strings.Contains(s, "vision") && strings.Contains(s, "support")) ||
		strings.Contains(s, "does not support image")
}

func (r *Router) GetSession(sessionID string) (ModelEndpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	se, ok := r.sessions[sessionID]
	if !ok {
		return ModelEndpoint{}, false
	}
	return se.ep, true
}

func (r *Router) GetAllSessions() map[string]ModelEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]ModelEndpoint, len(r.sessions))
	for k, se := range r.sessions {
		result[k] = se.ep
	}
	return result
}

func (r *Router) GetAllCooldowns() map[string]CooldownEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]CooldownEntry, len(r.cooldowns))
	for k, v := range r.cooldowns {
		result[k] = v
	}
	return result
}

// CircuitStateSnapshot is a combined view of an endpoint's circuit breaker and
// cooldown state, returned by the /v1/airouter/state endpoint so operators
// can diagnose fail-over behavior from a single call.
type CircuitStateSnapshot struct {
	ModelKey       string        `json:"model_key"`
	CircuitState   string        `json:"circuit_state"`
	OpenedAt       time.Time     `json:"opened_at"`
	ProbesSent     int           `json:"probes_sent"`
	CooldownExpiry time.Time     `json:"cooldown_expiry,omitempty"`
	CooldownIn     time.Duration `json:"cooldown_remaining_ms"`
	ErrorCount     int           `json:"error_count"`
	StatusCode     int           `json:"status_code"`
	LastError      string        `json:"last_error,omitempty"`
}

// GetAllCircuitsState returns a combined snapshot of every endpoint's circuit
// breaker and cooldown state, with the cooldown remaining computed against
// the current wall clock. Used by the /v1/airouter/state endpoint.
func (r *Router) GetAllCircuitsState() []CircuitStateSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now()
	out := make([]CircuitStateSnapshot, 0, len(r.circuits))
	for key, cb := range r.circuits {
		snap := CircuitStateSnapshot{
			ModelKey:     key,
			CircuitState: cb.State.String(),
			OpenedAt:     cb.OpenedAt,
			ProbesSent:   cb.ProbesSent,
		}
		if cd, ok := r.cooldowns[key]; ok {
			snap.CooldownExpiry = cd.Expiry
			snap.ErrorCount = cd.ErrorCount
			snap.StatusCode = cd.StatusCode
			snap.LastError = cd.LastError
			if cd.Expiry.After(now) {
				snap.CooldownIn = cd.Expiry.Sub(now)
			}
		}
		out = append(out, snap)
	}
	return out
}

type ProviderError struct {
	StatusCode int
	Body       []byte
	Err        error
	// RetryAfter, when set, is the upstream's requested backoff for a 429
	// (parsed from the Retry-After header). The cooldown logic honors it as a
	// floor so we never wait less than the provider asked.
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		if e.RetryAfter > 0 {
			return fmt.Sprintf("%v (retry-after %v)", e.Err, e.RetryAfter)
		}
		return e.Err.Error()
	}
	if len(e.Body) > 0 {
		return string(e.Body)
	}
	return "provider error"
}

func (e *ProviderError) Unwrap() error {
	return e.Err
}

// ParseRetryAfter parses an HTTP Retry-After header value into a Duration.
// It accepts both the delta-seconds form (an integer, e.g. "120") and the
// HTTP-date form (RFC 1123 / RFC 7231). On any parse failure it returns 0 so
// callers can fall back to their own backoff. A negative or zero value is
// treated as "no useful hint" and also returns 0.
func ParseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	// Delta-seconds form: "120"
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	// HTTP-date form: "Wed, 21 Oct 2015 07:28:00 GMT"
	if t, err := http.ParseTime(raw); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		return d
	}
	return 0
}
