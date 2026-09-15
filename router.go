package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
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
	config       atomic.Pointer[Config]
	sessions     map[string]sessionEntry
	cooldowns    map[string]CooldownEntry
	noVision     map[string]bool // endpoints that rejected an image request
	tps          map[string]float64
	cooldownPath string
	priorityPath string
	mu           sync.RWMutex
	sessionsDone chan struct{}
	sessionsWG   sync.WaitGroup
	cooldownSave *cooldownSaveState
}

func NewRouter(cfg *Config, cooldownPath string) *Router {
	r := &Router{
		sessions:     make(map[string]sessionEntry),
		cooldowns:    make(map[string]CooldownEntry),
		noVision:     make(map[string]bool),
		tps:          make(map[string]float64),
		cooldownPath: cooldownPath,
		priorityPath: strings.TrimSuffix(cooldownPath, ".json") + ".priority.json",
		sessionsDone: make(chan struct{}),
		cooldownSave: &cooldownSaveState{},
	}
	r.config.Store(cfg)
	r.loadCooldowns()
	r.loadPriorities()
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
	var loaded map[string]CooldownEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("[warn] failed to parse cooldowns (state reset): %v", err)
		return
	}
	now := time.Now()
	for k, v := range loaded {
		if v.Expiry.After(now) {
			r.cooldowns[k] = v
		}
	}
	log.Printf("[debug] loaded %d active cooldowns", len(r.cooldowns))
}

func (r *Router) saveCooldowns() {
	if r.cooldownPath == "" {
		return
	}
	// Copy active cooldowns under RLock to avoid blocking all routing during disk I/O.
	r.mu.RLock()
	now := time.Now()
	active := make(map[string]CooldownEntry)
	for k, v := range r.cooldowns {
		if v.Expiry.After(now) {
			active[k] = v
		}
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(active, "", "  ")
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

func (r *Router) isAvailableLocked(ep *ModelEndpoint) bool {
	cd, ok := r.cooldowns[ep.Key()]
	if !ok {
		return true
	}
	if time.Now().After(cd.Expiry) {
		// Cooldown elapsed: the model is available again. We deliberately do NOT
		// delete the entry here, so its ErrorCount is preserved across cooldown
		// windows. Escalation then reflects *consecutive* failures (the count is
		// only reset on a successful response via RecordSuccess, or explicitly
		// via ResetCooldown), instead of restarting at 1 every time a cooldown
		// expires. Expired entries are still ignored by saveCooldowns (they have
		// a past Expiry, so they are not persisted) and by minCooldownWait, and
		// in-memory growth is bounded by the number of configured endpoints.
		return true
	}
	return false
}

func (r *Router) IsAvailable(ep *ModelEndpoint) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
				r.sessions[sessionID] = sessionEntry{ep: next, lastUsed: time.Now()}
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
		// All endpoints are in cooldown. Pick the first one anyway so the
		// client gets a chance (the cooldown may expire between now and the
		// next retry). Without this, we'd return nil and error out.
		if len(chain.Chain) > 0 {
			next := chain.Chain[0]
			log.Printf("[debug] session=%s model=%s -> all cooled down, retrying %s/%s", sessionID, logicalModel, next.Provider, next.Model)
			r.sessions[sessionID] = sessionEntry{ep: next, lastUsed: time.Now()}
			return &next, 0
		}
	}

	// New session
	for _, ep := range chain.Chain {
		if r.isEligibleLocked(&ep, requireVision) {
			log.Printf("[debug] session=%s model=%s -> new session -> %s/%s", sessionID, logicalModel, ep.Provider, ep.Model)
			r.sessions[sessionID] = sessionEntry{ep: ep, lastUsed: time.Now()}
			return &ep, 0
		}
	}

	return nil, r.minCooldownWait(chain.Chain, requireVision)
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
	r.ApplyCooldownForSession(ep, statusCode, errMsg, "")
}

// ApplyCooldownForSession records a failure for an endpoint. The sessionID
// parameter is retained for API compatibility but no longer controls any
// per-session state — failed-endpoint tracking is now local to each request's
// fallback loop (see handleStream/handleCompletion) to avoid interference
// between concurrent requests that share a session ID.
func (r *Router) ApplyCooldownForSession(ep *ModelEndpoint, statusCode int, errMsg string, sessionID string) {
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
	cd.Expiry = time.Now().Add(duration)
	cd.StatusCode = statusCode
	// Truncate the error message so a verbose provider error body (e.g.
	// Gemini's multi-kilobyte JSON) doesn't bloat cooldowns.json on every
	// retry. Keep it short; the full detail is in the log line below.
	cd.LastError = truncateErr(errMsg, 200)
	r.cooldowns[key] = cd
	if isVisionUnsupported(errMsg) {
		r.noVision[key] = true
	}
	r.mu.Unlock()
	// Debounce: coalesce rapid successive failures into a single disk write.
	// The first failure arms a short timer; subsequent failures reset it. On
	// timer expiry we persist. This bounds disk I/O to at most one write per
	// `cooldownSaveInterval` per burst, instead of one per failure.
	r.cooldownSave.trigger(r.saveCooldowns, cooldownSaveInterval)
	log.Printf("[debug] cooldown %s/%s status=%d errors=%d for %v: %s", ep.Provider, ep.Model, statusCode, cd.ErrorCount, duration, summarizeError(errMsg))
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
// run away.
func (r *Router) RecordSuccess(ep *ModelEndpoint) {
	r.mu.Lock()
	deleted := false
	key := ep.Key()
	if _, ok := r.cooldowns[key]; ok {
		delete(r.cooldowns, key)
		deleted = true
	}
	r.mu.Unlock()
	if deleted {
		r.saveCooldowns()
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
	r.ApplyCooldownForSession(ep, statusCode, errMsg, sessionID)
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
		// Model-not-found / misconfiguration: effectively permanent, so cool it
		// for a long time instead of retrying every window.
		return 30 * time.Minute
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

type ProviderError struct {
	StatusCode int
	Body       []byte
	Err        error
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
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
